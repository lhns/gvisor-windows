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
	"syscall"
	"os"
	"runtime"
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
	tgen        uint64 // the generation of t's stub (recycle_windows.go)
	cpu         int32 // the task's CPU number at the last Switch, for the vDSO's getcpu
	needPull    bool
	nreq        int

	// Spinning policy state (see decideSpin). spin: the Sentry side spins while
	// it waits for the next request; ctlScore and stubScore: how often spinning
	// on each side paid off recently (adaptive policy).
	spin      bool
	hasToken  bool
	uses      int
	cooldown  int
	ctlScore  int
	stubScore int
	nwaits    int
}

// Spin policies, WINSTUB_POLICY.
const (
	policyAdaptive = iota // per context, from whether spinning paid off recently
	policyLimit           // while no more contexts run than there are CPUs
	policyAlways
	policyTokens // a few contexts at a time hold a spinning token
)

var spinPolicy = func() int {
	switch os.Getenv("WINSTUB_POLICY") {
	case "limit":
		return policyLimit
	case "always":
		return policyAlways
	case "tokens":
		return policyTokens
	}
	return policyTokens
}()

const maxScore = 6

// decideSpin chooses, before the context runs, whether the Sentry side and the
// stub spin while they wait for each other, and sets the stub's spin budget.
//
// Spinning pays when the other side answers within a few microseconds, and costs
// a CPU when it does not: with more runnable threads than CPUs the spinners keep
// the thread that has the work from running. The adaptive policy keeps a score
// per side and context: a wait that ended during the spin raises it, one that had
// to sleep lowers it faster; at zero the side does not spin, except for one wait
// in eight that probes whether spinning would work again.
func (c *winContext) decideSpin(t *thread) {
	p := t.as.p
	n := p.active.Add(1)
	spinSentry, spinStub := true, true
	switch spinPolicy {
	case policyLimit:
		spinSentry = int(n) <= p.spinLimit
		spinStub = spinSentry
	case policyTokens:
		if c.cooldown > 0 {
			c.cooldown--
		} else if !c.hasToken && p.tokens.Add(1) <= p.maxTokens {
			c.hasToken, c.uses = true, 0
		} else if !c.hasToken {
			p.tokens.Add(-1)
			p.denied.Add(1)
		}
		spinSentry, spinStub = c.hasToken, c.hasToken
		if c.hasToken {
			// Hand the token over after a while, but only if someone asked.
			if c.uses++; c.uses > tokenUses {
				c.uses = 0
				if p.denied.Swap(0) > 0 {
					c.releaseToken(p)
					c.cooldown = 32
				}
			}
		}
	case policyAdaptive:
		c.nwaits++
		probe := c.nwaits&7 == 0
		spinSentry = c.ctlScore > 0 || probe
		spinStub = c.stubScore > 0 || probe
	}
	c.spin = spinSentry
	if spinStub {
		*t.u64(stubasm.PgSpins) = uint64(stubSpin)
	} else {
		*t.u64(stubasm.PgSpins) = 1 // the stub counts down from this: 0 would spin 2^64 times
	}
}

// tokenUses is how many round trips a context spins before it hands its token
// to another.
const tokenUses = 1000

func (c *winContext) releaseToken(p *Platform) {
	if c.hasToken {
		c.hasToken = false
		p.tokens.Add(-1)
	}
}

func scoreUpdate(score int, paidOff bool) int {
	if paidOff {
		return min(score+1, maxScore)
	}
	return max(score-3, 0)
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
	// The getcpu syscall answers the task's CPU (Task.CPU), not the rseq CPU the
	// cpu argument carries (-1 without rseq).
	if tc, ok := ctx.(interface{ CPU() int32 }); ok {
		c.cpu = tc.CPU()
	} else {
		c.cpu = max(cpu, 0)
	}
	if !c.intr.Enable() {
		return nil, hostarch.NoAccess, platform.ErrContextInterrupt
	}
	defer c.intr.Disable()

	if c.needRestore {
		copy(unsafe.Slice((*byte)(unsafe.Add(t.page, stubasm.XsaveOff)), t.as.p.fpSize), ac.FloatingPointData().Slice())
		c.needRestore = false
	}
	c.needPull = true
	c.decideSpin(t)
	defer t.as.p.active.Add(-1)
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
		if c.t.as == as && c.tgen == as.gen.Load() {
			return c.t, nil
		}
		c.t.as.freeThread(c.t, c.tgen)
		c.t = nil
	}
	t, gen, err := as.allocThread()
	if err != nil {
		return nil, err
	}
	c.t, c.tgen = t, gen
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
	*t.u32(stubasm.PgCPU) = uint32(c.cpu)
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
	t.nReply.Add(1)
	if atomic.LoadUint32(t.u32(stubasm.PgStubSlp)) != 0 {
		t.nSet.Add(1)
		// The stub's spin ended before we answered: it sleeps on the event.
		c.stubScore = scoreUpdate(c.stubScore, false)
		c.releaseToken(t.as.p)
		if r, _, e := pSetEvent.Call(t.evReply); r == 0 {
			t.nSetFail.Add(1)
			t.lastSetErr.Store(uint32(e.(syscall.Errno)))
		}
	} else {
		c.stubScore = scoreUpdate(c.stubScore, true)
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
	slept := false
	refused := 0
	defer func() {
		c.ctlScore = scoreUpdate(c.ctlScore, !slept)
		if slept {
			c.releaseToken(t.as.p)
		}
	}()
	for {
		spins := ctlSpin
		if !c.spin {
			spins = 0
		}
		for i := 0; i < spins && t.state() != 1; i++ {
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
				// The thread is somewhere it cannot be captured for a few
				// instructions (under a veneer's jump, in the stub's code):
				// yield first, since a timer sleep on Windows lasts 0.5 ms or more.
				if refused++; refused < 64 {
					runtime.Gosched()
				} else {
					time.Sleep(50 * time.Microsecond)
				}
			}
			continue
		}
		atomic.StoreUint32(sleep, 1)
		if t.state() == 1 || as.dead.Load() || c.intrPending.Load() {
			atomic.StoreUint32(sleep, 0)
			continue
		}
		slept = true
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
	if !c.needPull || c.t == nil || c.tgen != c.t.as.gen.Load() {
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
	if c.t != nil {
		c.releaseToken(c.t.as.p)
	}
	if traceOn && c.nreq > 0 {
		Trace("ctx %p: release after %d requests", c, c.nreq)
	}
	c.cur.Store(nil)
	if c.t != nil {
		c.t.as.freeThread(c.t, c.tgen)
		c.t = nil
	}
}

// PrepareSleep implements platform.Context.PrepareSleep.
func (c *winContext) PrepareSleep() { c.sleeping() }

// PrepareUninterruptibleSleep implements platform.Context.PrepareUninterruptibleSleep.
func (c *winContext) PrepareUninterruptibleSleep() { c.sleeping() }

// PrepareStop implements platform.Context.PrepareStop.
func (c *winContext) PrepareStop() { c.sleeping() }

// sleeping gives up the spinning token of a context that is about to block.
func (c *winContext) sleeping() {
	if c.t != nil {
		c.releaseToken(c.t.as.p)
	}
}

// PrepareExecve implements platform.Context.PrepareExecve.
func (c *winContext) PrepareExecve() {}

// PrepareExit implements platform.Context.PrepareExit.
func (c *winContext) PrepareExit() {}
