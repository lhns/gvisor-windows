// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build windows && amd64

package winstub

import (
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	pkgcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/interrupt"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
)

// winContext implements platform.Context: it owns one stub thread, in the address
// space of the memory manager it last ran with.
type winContext struct {
	platform.NoCPUNumbers

	intr        interrupt.Forwarder
	intrPending atomic.Bool
	cur         atomic.Pointer[thread]
	t           *thread

	// needRestore: ac's FP state must be written to the thread before it runs
	// (first run, FullStateChanged). needPull: the thread's FP state is newer than
	// ac's (every stop); PullFullState copies it.
	needRestore bool
	needPull    bool
	nreq        int
}

const eflagsUser = 0x246 // what a Linux syscall leaves in r11

// Switch implements platform.Context.Switch.
func (c *winContext) Switch(ctx pkgcontext.Context, mm platform.MemoryManager, ac *arch.Context64, cpu int32) (*linux.SignalInfo, hostarch.AccessType, error) {
	as, ok := mm.AddressSpace().(*addressSpace)
	if !ok {
		return nil, hostarch.NoAccess, &platform.ContextError{Err: errUnsupported, Errno: unix.EINVAL}
	}
	if as.dead.Load() {
		return nil, hostarch.NoAccess, &platform.ContextError{Err: errDead, Errno: unix.EPERM}
	}
	as.reserveHoles(ctx, mm)
	t, err := c.bind(as)
	if err != nil {
		return nil, hostarch.NoAccess, &platform.ContextError{Err: err, Errno: unix.EAGAIN}
	}
	c.cur.Store(t)
	if !c.intr.Enable() {
		return nil, hostarch.NoAccess, platform.ErrContextInterrupt
	}
	defer c.intr.Disable()

	if c.needRestore {
		copy(unsafe.Slice((*byte)(unsafe.Add(t.page, stubasm.XsaveOff)), t.as.p.fpSize), ac.FloatingPointData().Slice())
		c.needRestore = false
	}
	c.needPull = true
	if err := c.resume(t, ac); err != nil {
		return nil, hostarch.NoAccess, &platform.ContextError{Err: err, Errno: unix.EAGAIN}
	}
	req, err := c.wait(t)
	if err != nil {
		return nil, hostarch.NoAccess, &platform.ContextError{Err: err, Errno: unix.EPERM}
	}
	r := &ac.StateData().Regs
	switch req.kind {
	case reqSyscall:
		loadRegs(t, r, true)
		return nil, hostarch.NoAccess, nil
	case reqInterrupt:
		loadRegs(t, r, false)
		c.intrPending.Store(false)
		return nil, hostarch.NoAccess, platform.ErrContextInterrupt
	default:
		loadRegs(t, r, false)
		si := &linux.SignalInfo{Signo: int32(req.sig), Code: req.code}
		si.SetAddr(req.addr)
		return si, req.at, platform.ErrContextSignal
	}
}

// bind returns the stub thread for as, creating it (and dropping the thread of
// another address space, as after execve) when needed.
func (c *winContext) bind(as *addressSpace) (*thread, error) {
	if c.t != nil {
		if c.t.as == as {
			return c.t, nil
		}
		c.t.as.freeThread(c.t)
		c.t = nil
	}
	t, err := as.allocThread()
	if err != nil {
		return nil, err
	}
	c.t = t
	c.needRestore, c.needPull = true, false
	return t, nil
}

// hwRegs returns the registers in the order of the page's register file.
func hwRegs(r *linux.PtraceRegs) [16]uint64 {
	return [16]uint64{r.Rax, r.Rcx, r.Rdx, r.Rbx, r.Rsp, r.Rbp, r.Rsi, r.Rdi,
		r.R8, r.R9, r.R10, r.R11, r.R12, r.R13, r.R14, r.R15}
}

// resume writes the whole register state of ac into the page and lets the thread
// run: it starts the thread the first time, otherwise it answers the request the
// thread is waiting on.
func (c *winContext) resume(t *thread, ac *arch.Context64) error {
	r := &ac.StateData().Regs.PtraceRegs
	as := t.as
	// A syscall return needs only rax; anything else the Sentry changed (signal
	// delivery, restart, sigreturn) takes the full-context path.
	mode := uint32(1)
	if t.lastKind == reqSyscall && r.Rcx == r.Rip && r.R11 == eflagsUser && r.Eflags == eflagsUser {
		mode = 0
	}
	rip := as.restartTarget(r.Rip)
	for i, v := range hwRegs(r) {
		*t.reg(i) = v
	}
	*t.u64(stubasm.PgRIP) = rip
	*t.u64(stubasm.PgFS) = r.Fs_base
	*t.u64(stubasm.PgReply) = r.Rax
	*t.u64(stubasm.PgFlags) = (r.Eflags & 0xCD5) | 0x202
	*t.u32(stubasm.PgMode) = mode
	if !t.started {
		return as.startThread(t)
	}
	t.mu.Lock()
	t.parked = false
	t.mu.Unlock()
	atomic.StoreUint32(t.u32(0), 2)
	if atomic.LoadUint32(t.u32(stubasm.PgStubSlp)) != 0 {
		pSetEvent.Call(t.evReply)
	}
	return nil
}

// loadRegs copies the registers the thread published into ac. For a syscall the
// state is what a Linux syscall entry shows: rax and orig_rax are the number,
// rcx the return address and r11 the flags.
func loadRegs(t *thread, r *arch.Registers, syscall bool) {
	p := &r.PtraceRegs
	p.Rax, p.Rcx, p.Rdx, p.Rbx, p.Rsp, p.Rbp, p.Rsi, p.Rdi = *t.reg(0), *t.reg(1), *t.reg(2), *t.reg(3), *t.reg(4), *t.reg(5), *t.reg(6), *t.reg(7)
	p.R8, p.R9, p.R10, p.R11, p.R12, p.R13, p.R14, p.R15 = *t.reg(8), *t.reg(9), *t.reg(10), *t.reg(11), *t.reg(12), *t.reg(13), *t.reg(14), *t.reg(15)
	p.Rip = *t.u64(stubasm.PgRIP)
	if syscall {
		p.Orig_rax = p.Rax
		p.Rcx = p.Rip
		p.R11 = eflagsUser
		p.Eflags = eflagsUser
	} else {
		p.Orig_rax = ^uint64(0)
		p.Eflags = *t.u64(stubasm.PgFlags)
	}
}

// wait blocks until the thread publishes a request: it spins briefly, then sleeps
// on the request event, which the stub signals only if it sees the sleep flag, and
// which Interrupt and the process-exit path signal as well.
func (c *winContext) wait(t *thread) (request, error) {
	as := t.as
	sleep := t.u32(stubasm.PgCtlSlp)
	for {
		for i := 0; i < ctlSpin && t.state() != 1; i++ {
			if i&1023 == 1023 && (as.dead.Load() || c.intrPending.Load()) {
				break
			}
		}
		if t.state() == 1 {
			break
		}
		if as.dead.Load() {
			return request{}, errDead
		}
		if c.intrPending.Load() {
			if !as.interrupt(t) {
				time.Sleep(50 * time.Microsecond)
			}
			continue
		}
		atomic.StoreUint32(sleep, 1)
		if t.state() == 1 || as.dead.Load() || c.intrPending.Load() {
			atomic.StoreUint32(sleep, 0)
			continue
		}
		pWaitSingle.Call(t.evReq, infinite)
		atomic.StoreUint32(sleep, 0)
	}
	t.mu.Lock()
	req := t.req
	t.req = request{}
	t.lastKind = req.kind
	t.mu.Unlock()
	if traceOn {
		if c.nreq == 0 {
			Trace("ctx %p: first request (kind %d)", c, req.kind)
		}
		c.nreq++
	}
	return req, nil
}

// Interrupt implements platform.Context.Interrupt.
func (c *winContext) Interrupt() { c.intr.NotifyInterrupt() }

// NotifyInterrupt implements interrupt.Receiver.NotifyInterrupt.
func (c *winContext) NotifyInterrupt() {
	c.intrPending.Store(true)
	if t := c.cur.Load(); t != nil {
		pSetEvent.Call(t.evReq)
	}
}

// Preempt implements platform.Context.Preempt: Windows schedules the stub threads
// itself, so there is nothing to preempt.
func (c *winContext) Preempt() {}

// PullFullState implements platform.Context.PullFullState: ac always holds the
// full state.
func (c *winContext) PullFullState(as platform.AddressSpace, ac *arch.Context64) error {
	if !c.needPull || c.t == nil {
		return nil
	}
	copy(ac.FloatingPointData().Slice(), unsafe.Slice((*byte)(unsafe.Add(c.t.page, stubasm.XsaveOff)), c.t.as.p.fpSize))
	c.needPull = false
	return nil
}

// FullStateChanged implements platform.Context.FullStateChanged.
func (c *winContext) FullStateChanged() {
	c.needRestore, c.needPull = true, false
}

// Release implements platform.Context.Release.
func (c *winContext) Release() {
	if traceOn && c.nreq > 0 {
		Trace("ctx %p: release after %d requests", c, c.nreq)
	}
	c.cur.Store(nil)
	if c.t != nil {
		c.t.as.freeThread(c.t)
		c.t = nil
	}
}

// PrepareSleep implements platform.Context.PrepareSleep.
func (c *winContext) PrepareSleep() {}

// PrepareUninterruptibleSleep implements platform.Context.PrepareUninterruptibleSleep.
func (c *winContext) PrepareUninterruptibleSleep() {}

// PrepareStop implements platform.Context.PrepareStop.
func (c *winContext) PrepareStop() {}

// PrepareExecve implements platform.Context.PrepareExecve.
func (c *winContext) PrepareExecve() {}

// PrepareExit implements platform.Context.PrepareExit.
func (c *winContext) PrepareExit() {}
