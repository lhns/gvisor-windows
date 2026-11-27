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
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/pe"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
)

// Spin budgets before a waiting side goes to sleep on its event: pause
// iterations on the stub side, polls on the Sentry side. WINSTUB_STUBSPIN and
// WINSTUB_CTLSPIN override them for tuning.
var (
	stubSpin = envInt("WINSTUB_STUBSPIN", 3000)
	ctlSpin  = envInt("WINSTUB_CTLSPIN", 20000)
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

// Request kinds a stopped thread publishes (state 1).
const (
	reqSyscall   = iota // the page holds a syscall's registers
	reqFault            // the page holds the full context at a fault
	reqInterrupt        // the page holds the full context at an interrupt
	reqNone             // (lastKind of a reused thread: nothing handed to this context yet)
)

type request struct {
	kind int
	sig  int   // Linux signal number for reqFault
	code int32 // si_code
	addr uint64
	at   hostarch.AccessType
}

// thread is one stub thread slot: a control page shared with the stub plus the
// Windows thread running there.
type thread struct {
	as      *addressSpace
	idx     int
	addr    uint64         // address of the page in the stub
	page    unsafe.Pointer // the page in the Sentry
	evReq   uintptr        // the stub signals it after publishing a request
	evReply uintptr        // the Sentry signals it after a reply
	hThread uintptr
	tid     uint32
	// diagnostics: replies sent, SetEvent calls made for them
	nReply, nSet, nSetFail, lastSetErr atomic.Uint32

	// stopMu serializes stopping the thread (debug event handler, interrupt).
	stopMu sync.Mutex
	// mu guards req and parked.
	mu     sync.Mutex
	req    request
	parked bool // stopped at a fault or interrupt and not yet resumed
	// started: the Windows thread exists. lastKind: the kind of the last request
	// handed to the Sentry.
	started  bool
	lastKind int
	// idle: the Windows thread of a freed slot still runs, waiting for a reply;
	// allocThread hands it out again (threadReuse).
	idle bool
	lost bool // its thread was ended in a hurry: the slot is not used again

	// teb: address of the stub thread's TEB (rewrite mode, read lazily); dyn: the
	// veneer the debugger set up for it, if any.
	teb uint64
	dyn *fsVenInfo

	lastRepairRIP  uint64
	lastRepairAddr uint64
	repairN        int
	repairSince    time.Time
}

func (t *thread) u64(off int) *uint64 { return (*uint64)(unsafe.Add(t.page, off)) }
func (t *thread) u32(off int) *uint32 { return (*uint32)(unsafe.Add(t.page, off)) }
func (t *thread) state() uint32       { return atomic.LoadUint32(t.u32(0)) }
func (t *thread) reg(i int) *uint64   { return t.u64(stubasm.PgRegs + 8*i) }

// slot returns thread slot idx, creating its page and events on first use.
func (as *addressSpace) slot(idx int) *thread {
	if t := as.slots[idx]; t != nil {
		return t
	}
	addr := stubasm.CtlBase + uint64(idx)*stubasm.ThreadRegion
	t := &thread{as: as, idx: idx, addr: addr,
		page: unsafe.Add(as.ctlView, uint64(idx)*stubasm.ThreadRegion)}
	// The control section is reserved, not committed; commit this slot (shared
	// by all views of the section, so the stub sees it too).
	if _, err := windows.VirtualAlloc(uintptr(t.page), uintptr(stubasm.ThreadRegion), windows.MEM_COMMIT, windows.PAGE_READWRITE); err != nil {
		panic(fmt.Sprintf("winstub: cannot commit a thread slot: %v", err))
	}
	t.evReq, t.evReply = newEvent(), newEvent()
	*t.u64(stubasm.PgNtSet) = as.p.ntSet
	*t.u64(stubasm.PgNtWait) = as.p.ntWait
	*t.u64(stubasm.PgNtTerm) = as.p.ntTerm
	*t.u64(stubasm.PgNtSetInfo) = as.p.ntSetInfo
	*t.u64(stubasm.PgHReq) = uint64(dupInto(as.h, t.evReq))
	*t.u64(stubasm.PgHReply) = uint64(dupInto(as.h, t.evReply))
	*t.u64(stubasm.PgStkTop) = addr + stubasm.ThreadRegion - 0x100
	*t.u64(stubasm.PgSpins) = uint64(stubSpin)
	*t.u64(stubasm.PgXMask) = as.p.xmask
	if !as.p.fsRewrite {
		code := stubasm.RepairCode(addr)
		copy(unsafe.Slice((*byte)(unsafe.Add(t.page, stubasm.PgRepair)), len(code)), code)
	}
	as.slots[idx] = t
	return t
}

// allocThread returns a free thread slot (slot 0 is the anchor thread) and the
// generation of the stub it belongs to.
func (as *addressSpace) allocThread() (*thread, uint64, error) {
	as.tmu.Lock()
	defer as.tmu.Unlock()
	if as.dead.Load() {
		return nil, 0, errDead
	}
	var t *thread
	for t == nil {
		var idx int
		if n := len(as.freeSlots); n > 0 {
			idx, as.freeSlots = as.freeSlots[n-1], as.freeSlots[:n-1]
		} else {
			for idx = 1; idx < len(as.slots) && as.slots[idx] != nil; idx++ {
			}
			if idx == len(as.slots) {
				return nil, 0, fmt.Errorf("winstub: more than %d threads in one address space", len(as.slots)-1)
			}
		}
		t = as.slot(idx)
		if !t.idle {
			break
		}
		t.idle = false
		if t.started && t.hThread != 0 && t.state() == 1 {
			// Its first reply restores the whole context (resume), wherever the
			// thread was waiting.
			t.parked, t.lastKind, t.req, t.dyn = false, reqNone, request{}, nil
			*t.u32(stubasm.PgCtlSlp) = 0
			pResetEvent.Call(t.evReq)
			return t, as.gen.Load(), nil
		}
		// (Cannot happen: nothing answers an idle thread.) It is ended and its slot
		// is not used again: until it is gone it could take the next wake-up.
		t.lost = true
		as.nTermFallback.Add(1)
		pTerminateThread.Call(t.hThread, 0)
		t = nil
	}
	t.started, t.parked, t.lastKind = false, false, reqSyscall
	t.req = request{}
	*t.u32(0), *t.u32(stubasm.PgStubSlp), *t.u32(stubasm.PgCtlSlp), *t.u32(stubasm.PgMode) = 0, 0, 0, 0
	*t.u32(stubasm.PgRunning) = 0
	pResetEvent.Call(t.evReq) // no stale signal from the previous thread of the slot
	pResetEvent.Call(t.evReply)
	return t, as.gen.Load(), nil
}

// freeThread terminates a thread of generation gen and returns its slot. A thread of
// an earlier generation is gone already (recycle_windows.go).
func (as *addressSpace) freeThread(t *thread, gen uint64) {
	as.relMu.Lock()
	defer as.relMu.Unlock()
	if gen != as.gen.Load() {
		return
	}
	as.tmu.Lock()
	h := t.hThread
	terminate := t.started && !as.dead.Load() && h != 0
	if terminate && threadReuse && t.state() == 1 && t.idx != 0 {
		// Keep the thread for the next context: creating one costs a CreateRemoteThread
		// and ntdll's thread start-up, more than a millisecond.
		t.idle = true
		as.freeSlots = append(as.freeSlots, t.idx)
		as.tmu.Unlock()
		return
	}
	as.tmu.Unlock()
	if terminate {
		// A thread waiting for its reply ends itself when answered with mode 2: it
		// leaves no ntdll lock behind, which TerminateThread may.
		if t.state() == 1 {
			*t.u32(stubasm.PgMode) = 2
			atomic.StoreUint32(t.u32(0), 2)
			if atomic.LoadUint32(t.u32(stubasm.PgStubSlp)) != 0 {
				pSetEvent.Call(t.evReply)
			}
			if r, _, _ := pWaitSingle.Call(h, 2000); r != 0 {
				pTerminateThread.Call(h, 0)
			}
		} else {
			as.nTermFallback.Add(1)
			pTerminateThread.Call(h, 0)
		}
		// TerminateThread is asynchronous. Until the thread is gone it may still
		// be waiting on the slot's reply event, and would take the wake-up meant
		// for the next thread of the slot. (Not under tmu: the thread's exit
		// waits for the debugger loop to continue its EXIT_THREAD event, and the
		// loop takes tmu.)
		pWaitSingle.Call(h, 5000)
	}
	as.tmu.Lock()
	defer as.tmu.Unlock()
	if t.hThread != 0 {
		pCloseHandle.Call(t.hThread)
		t.hThread = 0
	}
	delete(as.byTid, t.tid)
	t.started = false
	if t.idx != 0 {
		as.freeSlots = append(as.freeSlots, t.idx)
	}
}

// threadReuse keeps the Windows threads of freed slots (WINSTUB_THREADREUSE=0 turns it off).
var threadReuse = os.Getenv("WINSTUB_THREADREUSE") != "0"

// replaceStalled re-enables the old rule that ends and recreates a thread that has not
// run after three seconds (WINSTUB_REPLACE_STALLED=1). It was a workaround for the
// hang whose cause was a Go stack that moved under a debug-event buffer
// (docs/results/1l-platform-stability.md); ending a thread that is inside ntdll's
// start-up can leave the loader lock held, so it is off by default and a thread that
// does not start in 15 s is an error (docs/results/1m-platform-repair.md).
var replaceStalled = envInt("WINSTUB_REPLACE_STALLED", 0) != 0

// startThread creates the Windows thread of t; it begins at the page's registers.
func (as *addressSpace) startThread(t *thread) error {
	// One thread is created at a time, and the next only after this one runs the
	// stub's entry code: several threads starting at once in a process under a
	// debugger sometimes never get past their start.
	as.createMu.Lock()
	defer as.createMu.Unlock()
	wait, tries := 15000, 0
	if replaceStalled {
		wait, tries = 3000, 3
	}
	for try := 0; ; try++ {
		if err := as.createThread(t); err != nil {
			return err
		}
		for i := 0; i < wait && atomic.LoadUint32(t.u32(stubasm.PgRunning)) == 0; i++ {
			if as.dead.Load() {
				return nil
			}
			time.Sleep(time.Millisecond)
		}
		if atomic.LoadUint32(t.u32(stubasm.PgRunning)) != 0 {
			return nil
		}
		if try >= tries {
			if !replaceStalled {
				as.nThreadReplaced.Add(1) // (counted in the dump: a thread that never started)
				return fmt.Errorf("winstub: thread %d (tid %d) did not start in %d s", t.idx, t.tid, wait/1000)
			}
			return nil
		}
		debugf("thread %d (tid %d) never started; replacing it", t.idx, t.tid)
		as.nThreadReplaced.Add(1)
		h := t.hThread
		pTerminateThread.Call(h, 0)
		pWaitSingle.Call(h, 5000)
		as.tmu.Lock()
		delete(as.byTid, t.tid)
		as.tmu.Unlock()
		pCloseHandle.Call(h)
		t.hThread = 0
	}
}

func (as *addressSpace) createThread(t *thread) error {
	var tid uint32
	const createSuspended = 4
	h, err := call(pCreateRemoteThr, as.h, 0, 0, uintptr(stubasm.StubCode+stubasm.OffThreadEnt), uintptr(t.addr), createSuspended, uintptr(unsafe.Pointer(&tid)))
	if err != nil {
		return fmt.Errorf("CreateRemoteThread: %w", err)
	}
	t.hThread, t.tid, t.teb, t.dyn = h, tid, 0, nil // a new thread: a new TEB (readTebSlot), no veneer of the old one
	as.tmu.Lock()
	as.byTid[tid] = t
	as.tmu.Unlock()
	t.started = true
	if r, _, e := pResumeThread.Call(h); int32(r) == -1 {
		return fmt.Errorf("ResumeThread: %v", e)
	}
	Trace("thread %d started (tid %d)", t.idx, tid)
	return nil
}

// ---- starting a stub ----------------------------------------------------------

// newAS starts a stub process on a goroutine of its own, which becomes its
// debugger: Windows delivers debug events only to the thread that created the
// process, so that goroutine stays locked to its OS thread for the process's
// lifetime.
func (p *Platform) newAS() (*addressSpace, error) {
	Trace("as: start")
	as := newAddressSpace(p)
	registerAS(as)
	go as.debugLoop()
	if err := <-as.started; err != nil {
		<-as.loopDone
		return nil, err
	}
	Trace("as: process set up")
	// Wait for the anchor thread to reach its wait: Windows finishes initializing
	// the process on it, and CreateRemoteThread needs that.
	anchor := as.slots[0]
	deadline := time.Now().Add(5 * time.Second) // (NewAddressSpace tries again)
	for atomic.LoadUint32(anchor.u32(stubasm.PgStubSlp)) == 0 {
		if as.dead.Load() || time.Now().After(deadline) {
			as.Release()
			return nil, fmt.Errorf("winstub: the stub did not start")
		}
		time.Sleep(200 * time.Microsecond)
	}
	Trace("as: anchor parked")
	if as.p.tripwire {
		if st := uint32(*anchor.u64(stubasm.PgICBStat)); st != 0 {
			log.Printf("winstub: the instrumentation callback could not be registered: NtSetInformationProcess = %#x", st)
		}
	}
	return as, nil
}

func (as *addressSpace) mapSys(lo, size uint64, sec uintptr, secOff uint64, prot int) error {
	v := &view{lo: lo, hi: lo + size, sec: sec, off: secOff, prot: prot, sys: true}
	return as.mapView(v)
}

func (as *addressSpace) start() error {
	cl, _ := syscall.UTF16PtrFromString(`"` + as.p.stubPath + `"`)
	var si syscall.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi syscall.ProcessInformation
	const createSuspended, debugOnlyThis, noWindow = 0x4, 0x2, 0x08000000
	if err := syscall.CreateProcess(nil, cl, nil, nil, false, createSuspended|debugOnlyThis|noWindow, nil, nil, &si, &pi); err != nil {
		return fmt.Errorf("CreateProcess: %w", err)
	}
	as.h, as.hMain, as.wpid = uintptr(pi.Process), uintptr(pi.Thread), pi.ProcessId
	fail := func(err error) error {
		pTerminateProcess.Call(as.h, 1)
		return err
	}
	as.mu.Lock()
	defer as.mu.Unlock()
	Trace("start: CreateProcess done")
	used := queryUsed(as.h, stubasm.ReserveMin, stubasm.ReserveMax)
	if debugEnabled {
		for _, u := range used {
			if u.lo < 0x1000000 {
				debugf("stub process uses %v", u)
			}
		}
	}
	if err := as.reserve(used); err != nil {
		return fail(err)
	}
	Trace("start: placeholders reserved")
	section := func(size uint64, reserve bool) (uintptr, unsafe.Pointer, error) {
		prot := uintptr(pageExecRW)
		if reserve {
			prot |= secReserveAttr // pages are committed when a thread slot is first used
		}
		s, err := call(pCreateMapping, ^uintptr(0), 0, prot, uintptr(size>>32), uintptr(size&0xFFFFFFFF), 0)
		if err != nil {
			return 0, nil, err
		}
		b, err := call(pMapViewOfFile, s, fileMapAllExec, 0, 0, 0)
		if err != nil {
			return 0, nil, err
		}
		return s, ptr(b), nil
	}
	var err error
	if as.areaSec, as.areaView, err = section(stubasm.AreaHi-stubasm.AreaLo, false); err != nil {
		return fail(fmt.Errorf("stub area: %w", err))
	}
	if as.ctlSec, as.ctlView, err = section(stubasm.CtlSize, true); err != nil {
		return fail(fmt.Errorf("control area: %w", err))
	}
	if as.p.fsRewrite {
		if as.fbigSec, as.fbigView, err = section(stubasm.FSBigSize, true); err != nil {
			return fail(fmt.Errorf("%%fs veneer area: %w", err))
		}
		as.gcur, as.gcommit = stubasm.FSBig, stubasm.FSBig
	}
	Trace("start: sections created")
	for _, m := range []struct {
		lo, size uint64
		prot     int
	}{
		{stubasm.TrapPtrs, stubasm.TrapPtrsSize, protR},
		{stubasm.TrapCode, stubasm.TrapCodeSize, protR | protX},
		{stubasm.StubCode, stubasm.StubCodeSize, protR | protX},
		{stubasm.Veneers, stubasm.VeneersSize, protR | protX},
		{stubasm.FSVeneers, stubasm.FSVeneersSize, protR | protX},
	} {
		if err := as.mapSys(m.lo, m.size, as.areaSec, m.lo-stubasm.AreaLo, m.prot); err != nil {
			return fail(err)
		}
	}
	if err := as.mapSys(stubasm.CtlBase, stubasm.CtlSize, as.ctlSec, 0, protR|protW|protX); err != nil {
		return fail(err)
	}
	if as.fbigView != nil {
		if err := as.mapSys(stubasm.FSBig, stubasm.FSBigSize, as.fbigSec, 0, protR|protX); err != nil {
			return fail(err)
		}
	}
	Trace("start: views mapped")
	as.areaWrite(stubasm.StubCode, as.p.image)

	// Slot 0 is the anchor: the initial thread, parked for good, so that the
	// process outlives any guest thread.
	t := as.slot(0)
	*t.reg(4) = t.addr + stubasm.ThreadRegion - 0x100 // rsp
	*t.u64(stubasm.PgRIP) = stubasm.StubCode + stubasm.OffTrampWait
	if as.p.tripwire {
		// The anchor registers the callback before it parks (NtSetInformationProcess
		// on its own process: no privilege needed, checked as a standard user).
		*t.u64(stubasm.PgICBInfo + 8) = stubasm.StubCode + stubasm.OffICB
		*t.u64(stubasm.PgRIP) = stubasm.StubCode + stubasm.OffICBInit
	}
	*t.u32(0) = 1
	t.hThread, t.tid, t.started = as.hMain, pi.ThreadId, true
	as.byTid[pi.ThreadId] = t
	if r, _, e := pResumeThread.Call(as.hMain); int32(r) == -1 {
		return fail(fmt.Errorf("ResumeThread: %v", e))
	}
	Trace("start: main thread resumed")
	return nil
}

// ---- the debugger goroutine ---------------------------------------------------

func (as *addressSpace) debugLoop() {
	runtime.LockOSThread() // never unlocked: the OS thread ends with the goroutine
	defer close(as.loopDone)
	if err := as.start(); err != nil {
		as.dead.Store(true)
		as.started <- err
		return
	}
	as.started <- nil
	ev, ctx := newDebugEvent(), newCtxBuf()
	seenBP := false
	fails := 0 // consecutive ContinueDebugEvent failures
	for {
		// A bounded wait: a Release that terminated the process must not depend on
		// the exit event ever arriving (a dump of a hung fork+exec loop showed an
		// exiting task stuck in Release waiting for this goroutine, which sat in an
		// unbounded WaitForDebugEvent).
		if r, _, e := pWaitDebugEvent.Call(ev.base(), 100); r == 0 {
			if en, ok := e.(syscall.Errno); ok && (en == 121 || en == 0 || en == 258 || en == 1460) { // a timeout (ERROR_SEM_TIMEOUT)
				if as.released.Load() {
					as.markDead()
					return
				}
				if w, _, _ := pWaitSingle.Call(as.h, 0); w == 0 { // the process is gone
					as.markDead()
					return
				}
				continue
			}
			debugf("WaitForDebugEvent: %v", e)
			as.markDead()
			return
		}
		as.nEvents.Add(1)
		status := uint32(dbgContinue)
		exited := false
		as.lastEvent.Store(fmt.Sprintf("handling event %d tid %d code %#x", ev.code(), ev.tid(), ev.u32(16)))
		if traceOn && ev.code() != 1 {
			name := ""
			if h := uintptr(ev.u64(16)); (ev.code() == 6 || ev.code() == 3) && h != 0 {
				var buf [260]uint16
				if n, _ := windows.GetFinalPathNameByHandle(windows.Handle(h), &buf[0], 260, 0); n > 0 {
					name = windows.UTF16ToString(buf[:])
				}
			}
			Trace("debug event %d (tid %d) %s", ev.code(), ev.tid(), name)
		}
		switch ev.code() {
		case 1: // EXCEPTION_DEBUG_EVENT
			status = as.onException(ev, ctx, &seenBP)
		case 3, 6: // CREATE_PROCESS, LOAD_DLL: close the file handle
			if h := uintptr(ev.u64(16)); h != 0 {
				pCloseHandle.Call(h)
			}
		case 4: // EXIT_THREAD
			as.tmu.Lock()
			delete(as.byTid, ev.tid())
			as.tmu.Unlock()
		case 5: // EXIT_PROCESS
			exited = true
		}
		cr, _, ce := pContinueDebug.Call(uintptr(ev.pid()), uintptr(ev.tid()), uintptr(status))
		as.lastEvent.Store(fmt.Sprintf("continued event %d tid %d code %#x (status %#x, ContinueDebugEvent ok=%v err=%v)", ev.code(), ev.tid(), ev.u32(16), status, cr != 0, ce))
		if cr != 0 {
			fails = 0
		} else if fails++; !exited && fails >= 3 {
			// A debug event that cannot be continued leaves the process frozen
			// for good: end this address space (the tasks using it fail) rather
			// than hang.
			debugf("ContinueDebugEvent for event %d tid %d failed: %v; ending the stub", ev.code(), ev.tid(), ce)
			pTerminateProcess.Call(as.h, 1)
			as.markDead()
			return
		}
		if exited {
			as.markDead()
			return
		}
	}
}

// Windows exception codes the platform maps to Linux signals.
const (
	excAccessViolation = 0xC0000005
	excBreakpoint      = 0x80000003
	excSingleStep      = 0x80000004
	excIllegalInstr    = 0xC000001D
	excPrivileged      = 0xC0000096
	excIntDivide       = 0xC0000094
	excIntOverflow     = 0xC0000095
	excStackOverflow   = 0xC00000FD
	excGuardPage       = 0x80000001
	excDatatypeMisalgn = 0x80000002
	excInPageError     = 0xC0000006
)

func (as *addressSpace) onException(ev *debugEvent, ctx *ctxBuf, seenBP *bool) uint32 {
	code := ev.u32(16)
	as.tmu.Lock()
	t := as.byTid[ev.tid()]
	as.tmu.Unlock()
	if t == nil || t.idx == 0 {
		if code == excBreakpoint && !*seenBP {
			*seenBP = true // the loader breakpoint
			return dbgContinue
		}
		debugf("exception %#x in an unknown thread %d", code, ev.tid())
		return dbgNotHandled
	}
	if code == excBreakpoint && !*seenBP {
		*seenBP = true
		return dbgContinue
	}
	excAddr := ev.u64(32)
	if traceOn {
		if code == excAccessViolation {
			Trace("exception %#x at %#x (thread %d) kind %d addr %#x %s", code, excAddr, t.idx, ev.u64(48), ev.u64(56), as.viewDesc(ev.u64(56)))
		} else {
			Trace("exception %#x at %#x (thread %d)", code, excAddr, t.idx)
		}
	}
	t.stopMu.Lock()
	defer t.stopMu.Unlock()
	t.mu.Lock()
	parked := t.parked
	t.mu.Unlock()
	// A stale event for a thread that was moved away meanwhile.
	if !ctx.get(t.hThread, ctxControlInt) {
		return dbgContinue
	}
	rip := *ctx.u64(ctxRipOff)
	if parked {
		// An interrupt captured the thread (saved its registers and redirected
		// it) while this event was pending. The event may resume the thread with
		// the context it had when it faulted, undoing the redirect: if the thread
		// still is at the faulting instruction, redirect it again. Its registers
		// are in the page already.
		if rip == excAddr {
			*ctx.u64(ctxRipOff) = stubasm.StubCode + stubasm.OffTrampPark
			ctx.set(t.hThread)
		}
		return dbgContinue
	}
	if rip != excAddr && code != excBreakpoint && code != excSingleStep {
		return dbgContinue
	}

	switch code {
	case excAccessViolation:
		kind, addr := ev.u64(48), ev.u64(56)
		switch {
		case kind == 1 && as.wxWrite(addr):
			return dbgContinue
		case kind == 8 && as.wxExec(addr):
			return dbgContinue
		}
		if !as.p.fsRewrite && (addr < 0x10000 || addr >= 0xFFFF800000000000) && *t.u64(stubasm.PgFS) != 0 {
			if as.repairFS(t, ctx, rip, addr) {
				return dbgContinue
			}
		}
		if as.p.fsRewrite {
			// The %fs base is always zero in this mode: an %fs instruction that runs
			// natively is one the patcher missed (the %fs tripwire).
			if b, ok := as.readStub(rip, 16); ok && hasFSPrefix(b) {
				ctxb, _ := as.readStub(rip-16, 48)
				log.Printf("winstub: FS TRIPWIRE: an %%fs instruction at %#x ran unrewritten (fault address %#x, thread %d): % x", rip, addr, t.idx, ctxb)
			}
		}
		if debugEnabled {
			b, _ := as.readStub(rip-24, 48)
			debugf("guest fault: addr %#x kind %d rip %#x (thread %d) code[-24:+24] % x regs % x", addr, kind, rip, t.idx, b, unsafe.Slice((*byte)(unsafe.Pointer(ctx.u64(ctxRegsOff))), 128))
		}
		at := hostarch.AccessType{Read: true}
		switch kind {
		case 1:
			at = hostarch.AccessType{Write: true}
		case 8:
			at = hostarch.AccessType{Execute: true}
		}
		sc := int32(1) // SEGV_MAPERR
		if as.mapped(addr) {
			sc = 2 // SEGV_ACCERR
		}
		as.park(t, ctx, request{kind: reqFault, sig: 11, code: sc, addr: addr, at: at})
	case excIllegalInstr:
		b, ok := as.readStub(rip, 2)
		isUD2 := ok && b[0] == 0x0F && b[1] == 0x0B
		if isUD2 && rip >= stubasm.StubCode+stubasm.OffICB && rip < stubasm.StubCode+stubasm.OffICB+0x100 {
			as.tripped(t)
			break
		}
		if isUD2 && as.isUD2Site(rip) {
			// A rewritten syscall that could not be patched: make it a request.
			as.parkAs(t, ctx, request{kind: reqSyscall}, rip+2)
		} else if orig := as.fsUD2At(rip); isUD2 && orig != nil && as.fsRunInline(t, ctx, rip, orig) {
			// An %fs instruction that could not be patched runs in a veneer of the thread's own.
		} else {
			if debugEnabled {
				bb, _ := as.readStub(rip-8, 24)
				debugf("illegal instruction at %#x (thread %d): % x", rip, t.idx, bb)
			}
			as.park(t, ctx, request{kind: reqFault, sig: 4, code: 1, addr: rip})
		}
	case excBreakpoint:
		if as.fsSplitRun(t, ctx, excAddr) || as.windowResume(t, ctx, excAddr) {
			break
		}
		as.park(t, ctx, request{kind: reqFault, sig: 5, code: 128, addr: rip})
	case excSingleStep:
		as.park(t, ctx, request{kind: reqFault, sig: 5, code: 2, addr: rip})
	case excIntDivide, excIntOverflow:
		as.park(t, ctx, request{kind: reqFault, sig: 8, code: 1, addr: rip})
	case excPrivileged, excStackOverflow, excGuardPage, excInPageError:
		as.park(t, ctx, request{kind: reqFault, sig: 11, code: 1, addr: ev.u64(56)})
	case excDatatypeMisalgn:
		as.park(t, ctx, request{kind: reqFault, sig: 7, code: 1, addr: rip})
	default:
		debugf("unexpected exception %#x at %#x", code, rip)
		as.park(t, ctx, request{kind: reqFault, sig: 11, code: 1, addr: rip})
	}
	return dbgContinue
}

// tripped reports a thread that the instrumentation callback caught returning from
// the kernel into guest code that follows a syscall instruction: a syscall that was
// never patched ran on Windows. The address space is ended, loudly.
func (as *addressSpace) tripped(t *thread) {
	addr := *t.u64(stubasm.PgICBTrip)
	log.Printf("winstub: TRIPWIRE: the kernel returned to %#x in guest text, after a syscall instruction that was not patched (thread %d, stub pid %d)", addr, t.idx, as.wpid)
	if b, ok := as.readStub(addr-32, 48); ok {
		log.Printf("winstub: TRIPWIRE: bytes at %#x-32: % x", addr, b)
	}
	as.markDead()
	pTerminateProcess.Call(as.h, 1)
}

func (as *addressSpace) readStub(a uint64, n int) ([]byte, bool) {
	b := make([]byte, n)
	var got uintptr
	if _, err := call(pReadProcMem, as.h, uintptr(a), uintptr(unsafe.Pointer(&b[0])), uintptr(n), uintptr(unsafe.Pointer(&got))); err != nil {
		return nil, false
	}
	return b, true
}

// repairFS handles an access violation that looks like a lost %fs base: the CPU
// loses the base when Windows deschedules the thread, and a debug event of any thread
// can drop the bases of the others. The thread continues in its repair routine, which
// reloads the base and returns to the instruction.
//
// Whether the base was lost is read off the fault itself: the address of an access
// without a base is the bare operand (displacement plus registers), with the right base
// it is that plus the base. An instruction without an %fs prefix, or a fault at the
// address the right base would give, is the guest's own and is reported. A tight loop
// can lose its base at the same instruction any number of times, so repairs are never
// counted against a thread unless the instruction cannot be decoded (then 100000 in a
// row end it).
func (as *addressSpace) repairFS(t *thread, ctx *ctxBuf, rip, addr uint64) bool {
	lost, decided := as.fsBaseLost(ctx, rip, addr, *t.u64(stubasm.PgFS))
	if decided && !lost {
		as.nFSGenuine.Add(1)
		return false
	}
	if !decided {
		if rip == t.lastRepairRIP && addr == t.lastRepairAddr {
			if t.repairN++; t.repairN >= 100000 {
				t.repairN = 0
				as.nFSGenuine.Add(1)
				return false
			}
		} else {
			t.lastRepairRIP, t.lastRepairAddr, t.repairN = rip, addr, 0
		}
	}
	as.nFSRepair.Add(1)
	*t.u64(stubasm.PgRetRIP) = rip
	*ctx.u64(ctxRipOff) = t.addr + stubasm.PgRepair
	for try := 0; try < 3; try++ {
		if ctx.set(t.hThread) {
			return true
		}
	}
	// The thread continues at the faulting instruction and faults again: another try.
	as.nFSRepairFail.Add(1)
	return true
}

// fsBaseLost tells whether the access violation at addr, in the instruction at rip,
// is an %fs access that ran with a zero base (lost) or with the right one (fsBase);
// decided is false if the instruction cannot be taken apart or the address fits neither.
func (as *addressSpace) fsBaseLost(ctx *ctxBuf, rip, addr, fsBase uint64) (lost, decided bool) {
	b, ok := as.readStub(rip, 16)
	if !ok {
		return false, false
	}
	if !hasFSPrefix(b) {
		return false, true
	}
	r, ok := parseFS(b)
	if !ok {
		return false, false
	}
	x := uint64(int64(r.disp))
	if r.base >= 0 {
		x += *ctx.u64(ctxRegsOff + uintptr(8*r.base))
	}
	if r.index >= 0 {
		x += *ctx.u64(ctxRegsOff + uintptr(8*r.index)) << r.scale
	}
	switch addr {
	case x:
		return true, true
	case x + fsBase:
		return false, true
	}
	return false, false
}

// park saves the context of t (stopped at a debug event, or suspended) into its
// page, makes it continue in the stub's wait routine and publishes req. rip is
// taken from ctx.
func (as *addressSpace) park(t *thread, ctx *ctxBuf, req request) {
	as.parkAs(t, ctx, req, *ctx.u64(ctxRipOff))
}

// parkAs is park with the resume address chosen by the caller (a ud2 syscall
// resumes after the instruction). For a request of kind reqSyscall the registers
// are those of a syscall: rcx and r11 are the caller's to ignore.
func (as *addressSpace) parkAs(t *thread, ctx *ctxBuf, req request, resume uint64) {
	for i := 0; i < 16; i++ {
		*t.reg(i) = *ctx.u64(ctxRegsOff + uintptr(8*i))
	}
	// A thread inside an %fs veneer is moved out of it first (ADR 0020).
	if nr, ok := as.fsFix(t, resume); ok {
		resume = nr
	}
	*t.u64(stubasm.PgRIP) = resume
	*t.u64(stubasm.PgFlags) = uint64(ctx.eflags())
	*ctx.u64(ctxRipOff) = stubasm.StubCode + stubasm.OffTrampPark
	t.mu.Lock()
	t.req = req
	t.parked = true
	t.mu.Unlock()
	// The thread continues in the stub's park routine: it saves the FP and vector
	// state, then publishes the request (state 1) and wakes the Sentry itself.
	ctx.set(t.hThread)
}

// Why interrupt refused a thread (counted by the signal latency trace).
const (
	refuseNone    = iota
	refuseStartup // still in ntdll's thread start-up
	refuseWaking  // answered, waking from (or asleep in) its reply wait
	refuseSuspend // SuspendThread or GetThreadContext failed
	refuseStub    // in the stub's code
	refuseRepair  // in the repair code above CtlBase
	refuseJump    // under a patch jump
	refuseLow     // below AreaLo
	nRefuse
)

var refuseNames = [nRefuse]string{"", "startup", "waking", "suspend", "stub", "repair", "jump", "low"}

// interrupt stops a running thread at its current instruction and publishes an
// interrupt request. It reports why not (non-zero) if the thread is not (yet)
// somewhere that can be captured exactly (inside the stub's own code, or in ntdll
// during thread start); the caller retries.
func (as *addressSpace) interrupt(t *thread) int {
	t.stopMu.Lock()
	defer t.stopMu.Unlock()
	if t.state() == 1 || !t.started {
		return refuseNone // a request is already there
	}
	// A thread still in ntdll's start-up (waiting for the debugger to continue its
	// CREATE_THREAD event, running the loader) is left alone: suspending it there
	// can wedge it for good.
	if atomic.LoadUint32(t.u32(stubasm.PgRunning)) == 0 {
		return refuseStartup
	}
	// A thread that has just been answered is waking up from its wait in the
	// kernel (or still asleep in it): suspending it there loses the wake-up
	// often enough to hang the guest. Only a thread that has gone through the
	// whole reply path (state 0) and runs guest code is captured.
	if t.state() == 2 || atomic.LoadUint32(t.u32(stubasm.PgStubSlp)) != 0 {
		return refuseWaking
	}
	t.mu.Lock()
	if t.parked {
		t.mu.Unlock()
		return refuseNone
	}
	t.mu.Unlock()
	if r, _, _ := pSuspendThread.Call(t.hThread); int32(r) == -1 {
		return refuseSuspend
	}
	ctx := newCtxBuf()
	if !ctx.get(t.hThread, ctxControlInt) {
		pResumeThread.Call(t.hThread)
		return refuseSuspend
	}
	rip := *ctx.u64(ctxRipOff)
	inStub := rip >= stubasm.StubCode && rip < stubasm.StubCode+stubasm.StubCodeSize
	inRepair := rip >= stubasm.CtlBase
	// A thread inside an %fs veneer that would be reported inside the bytes of the
	// jump to it, where a signal handler may return with a plain ret (Go's
	// asyncPreempt), is caught once it is past them, a few instructions later.
	// (Refusing every thread in a veneer starves a loop of %fs accesses:
	// docs/results/1q-window-resume.md.)
	inJump := false
	if as.p.fsRewrite {
		if info := as.fsLocate(t, rip); info != nil {
			inJump = info.inJump(rip)
		}
	}
	why := refuseNone
	switch {
	case inStub:
		why = refuseStub
	case inRepair:
		why = refuseRepair
	case inJump:
		why = refuseJump
	case rip < stubasm.AreaLo:
		why = refuseLow
	}
	if why != refuseNone {
		pResumeThread.Call(t.hThread)
		return why
	}
	as.park(t, ctx, request{kind: reqInterrupt})
	pResumeThread.Call(t.hThread)
	return refuseNone
}

// ---- the stub image -----------------------------------------------------------

func stubImage() []byte {
	return pe.Stub(stubasm.CtlBase, stubasm.StubCode+stubasm.OffThreadEnt)
}
