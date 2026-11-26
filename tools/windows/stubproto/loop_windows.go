package main

import (
	"fmt"
	"sync/atomic"
	"time"
	"unsafe"

	"stubproto/stubasm"
)

func (c *Ctl) getCtx(h uintptr, flags uint32) bool {
	*(*uint32)(ptr(c.ctxBuf + 0x30)) = flags
	r, _, _ := pGetCtx.Call(h, c.ctxBuf)
	return r != 0
}

func (c *Ctl) setCtx(h uintptr) bool {
	r, _, _ := pSetCtx.Call(h, c.ctxBuf)
	return r != 0
}

func (c *Ctl) ctxU64(off uintptr) *uint64 { return (*uint64)(ptr(c.ctxBuf + off)) }

const ctxRip = 0xF8

func (c *Ctl) serve(t *Thread) int {
	p := t.p
	now := time.Now()
	if !p.gotFirst {
		p.gotFirst = true
		p.firstDur = now.Sub(p.created)
		if p.isFork {
			p.forkDur = now.Sub(p.forkStart)
			c.forkDurs = append(c.forkDurs, p.forkDur)
		}
	}
	nr := t.reg(rAX)
	a := [6]uint64{t.reg(rDI), t.reg(rSI), t.reg(rDX), t.reg(r10), t.reg(r8), t.reg(r9)}
	if c.benchOn {
		switch {
		case nr == 39 && !t.viaUD2:
			c.stamps["patched"] = append(c.stamps["patched"], qpcNs())
		case nr == 110 && t.viaUD2:
			c.stamps["ud2"] = append(c.stamps["ud2"], qpcNs())
		}
	}
	if t.viaUD2 {
		c.udServed++
	} else {
		c.patchedServed++
	}
	t.viaUD2 = false
	if nr == 9999 { // probe hook: register the guest address of a counter
		c.probeAddr = a[0]
		c.reply(t, 0)
		return stRet
	}
	ret, st := c.syscall(t, nr, a)
	if c.trace {
		fmt.Fprintf(c.stderr, "trace pid %d tid %d: syscall %d(%#x, %#x, %#x, %#x) = %d [%d] rip=%#x\n", p.pid, t.tid, nr, a[0], a[1], a[2], a[3], ret, st, *t.u64(stubasm.PgRIP))
	}
	if st == stRet {
		c.reply(t, ret)
	}
	return st
}

func (c *Ctl) startLoop(t *Thread) {
	c.wg.Add(1)
	go c.threadLoop(t)
}

// threadLoop serves one guest thread: spin briefly for its request, then sleep on
// its request event (the stub signals only if it sees the sleeping flag).
func (c *Ctl) threadLoop(t *Thread) {
	defer c.wg.Done()
	sleepFlag := (*uint32)(unsafe.Add(t.page, stubasm.PgCtlSlp))
	for !t.quit.Load() {
		got := false
		for i := 0; i < ctlSpin; i++ {
			if t.state() == 1 {
				got = true
				break
			}
			if i&1023 == 1023 && t.quit.Load() {
				return
			}
		}
		if !got {
			atomic.SwapUint32(sleepFlag, 1)
			if t.state() == 1 {
				atomic.StoreUint32(sleepFlag, 0)
			} else {
				pWaitSingle.Call(t.evReq, 50)
				atomic.StoreUint32(sleepFlag, 0)
				continue
			}
		}
		c.mu.Lock()
		if t.quit.Load() {
			c.mu.Unlock()
			return
		}
		st := stRet
		if !t.waiting && t.state() == 1 {
			st = c.serve(t)
		}
		c.mu.Unlock()
		if st == stBlocked {
			select {
			case <-t.doneCh:
			case <-t.quitCh:
				return
			}
		}
	}
}

func (c *Ctl) count(kind string) { c.ev[kind]++ }

func u32at(ev uintptr, off uintptr) uint32 { return *(*uint32)(ptr(ev + off)) }
func u64at(ev uintptr, off uintptr) uint64 { return *(*uint64)(ptr(ev + off)) }

// handleEvent processes one debug event and returns the continue status.
func (c *Ctl) handleEvent() uint32 {
	ev := c.evBuf
	code, pid, tid := u32at(ev, 0), u32at(ev, 4), u32at(ev, 8)
	p := c.procs[pid]
	t := c.threads[tid]
	switch code {
	case 1:
		return c.onException(p, t)
	case 2:
		c.count("CREATE_THREAD")
	case 3:
		c.count("CREATE_PROCESS")
		if h := uintptr(u64at(ev, 16)); h != 0 {
			pCloseHandle.Call(h)
		}
	case 4:
		c.count("EXIT_THREAD")
	case 5:
		c.count("EXIT_PROCESS")
		if p != nil {
			if !p.exited {
				c.exitProc(p, int(u32at(ev, 16)), 0)
			}
			p.dead = true
			for i, q := range c.live {
				if q == p {
					c.live = append(c.live[:i], c.live[i+1:]...)
					break
				}
			}
		}
	case 6:
		c.count("LOAD_DLL")
		if h := uintptr(u64at(ev, 16)); h != 0 {
			pCloseHandle.Call(h)
		}
	default:
		c.count(fmt.Sprintf("OTHER_%d", code))
	}
	return dbgContinue
}

func (c *Ctl) crash(p *Proc, t *Thread, what string, sig int) uint32 {
	c.count("EXCEPTION:guest-crash")
	rip := uint64(0)
	if t != nil && c.getCtx(t.h, ctxControlInt) {
		rip = *c.ctxU64(ctxRip)
	}
	fmt.Fprintf(c.stderr, "stubproto: pid %d crashed: %s rip=%#x\n", p.pid, what, rip)
	c.killSelf(p, sig)
	return dbgContinue
}

func (c *Ctl) onException(p *Proc, t *Thread) uint32 {
	ev := c.evBuf
	ecode := u32at(ev, 16)
	if p == nil {
		return dbgNotHandled
	}
	switch ecode {
	case 0x80000003: // breakpoint
		if !p.seenBP {
			p.seenBP = true
			c.count("EXCEPTION:loader-breakpoint")
			return dbgContinue
		}
		return c.crash(p, t, "int3", 5)
	case 0xC0000005:
		if t == nil {
			return dbgNotHandled
		}
		kind, addr := u64at(ev, 48), u64at(ev, 56)
		if kind == 1 && p.as.wxWrite(addr) {
			c.count("EXCEPTION:wx-drop-exec")
			return dbgContinue
		}
		if kind == 8 && p.wxExec(addr) {
			c.count("EXCEPTION:wx-rescan")
			return dbgContinue
		}
		if p.as.findVMA(addr) != nil && p.as.fault(addr, kind == 1, kind == 8) {
			c.count("EXCEPTION:av-lazy-map")
			if c.probeAddr != 0 && c.holdProbe {
				c.holdEvent(p)
			}
			return dbgContinue
		}
		if (addr < 0x10000 || addr >= 0xFFFF800000000000) && *t.u64(stubasm.PgFS) != 0 && c.getCtx(t.h, ctxControl) {
			rip := *c.ctxU64(ctxRip)
			now := time.Now()
			if rip == t.lastRip && now.Sub(t.lastRepair) < 5*time.Millisecond {
				t.repairN++
			} else {
				t.repairN = 0
			}
			t.lastRip, t.lastRepair = rip, now
			if t.repairN < 20 {
				*t.u64(stubasm.PgRetRIP) = rip
				*c.ctxU64(ctxRip) = t.addr + stubasm.PgRepair
				if c.setCtx(t.h) {
					c.count("EXCEPTION:av-fs-repair")
					return dbgContinue
				}
			}
		}
		return c.crash(p, t, fmt.Sprintf("access violation kind=%d addr=%#x", kind, addr), 11)
	case 0xC000001D:
		if t == nil {
			return dbgNotHandled
		}
		if !c.getCtx(t.h, ctxControlInt) {
			return c.crash(p, t, "GetThreadContext failed", 4)
		}
		rip := *c.ctxU64(ctxRip)
		if b, ok := p.as.read(rip, 2); ok && b[0] == 0x0F && b[1] == 0x0B {
			for i := 0; i < 16; i++ {
				t.setReg(i, *c.ctxU64(0x78 + uintptr(8*i)))
			}
			*t.u64(stubasm.PgRIP) = rip + 2
			t.viaUD2 = true
			atomic.SwapUint32((*uint32)(t.page), 1)
			pSetEvent.Call(t.evReq)
			*c.ctxU64(ctxRip) = stubasm.StubCode + stubasm.OffTrampWait
			if !c.setCtx(t.h) {
				return c.crash(p, t, "SetThreadContext failed", 4)
			}
			c.count("EXCEPTION:ud2-syscall-redirect")
			return dbgContinue
		}
		return c.crash(p, t, "illegal instruction", 4)
	}
	return c.crash(p, t, fmt.Sprintf("exception %#x", ecode), 11)
}

// holdEvent keeps a debug event pending for 20 ms and checks whether the guest
// counter still advances: it does not if the debug event stops every thread.
func (c *Ctl) holdEvent(p *Proc) {
	read := func() uint64 {
		v, _ := p.as.readU64(c.probeAddr)
		return v
	}
	v0 := read()
	time.Sleep(20 * time.Millisecond)
	v1 := read()
	c.holdSamples = append(c.holdSamples, v1-v0)
}

// eventStep waits up to 1 ms for one debug event and handles it; it also runs the
// tasks other threads hand over because they must happen on this (the debugger's) thread.
func (c *Ctl) eventStep() {
	select {
	case f := <-c.tasks:
		f()
	default:
	}
	r, _, _ := pWaitDebugEvent.Call(c.evBuf, 1)
	if r == 0 {
		return
	}
	c.mu.Lock()
	status := c.handleEvent()
	c.mu.Unlock()
	pContinueDebug.Call(uintptr(u32at(c.evBuf, 4)), uintptr(u32at(c.evBuf, 8)), uintptr(status))
}

// run drives the debugger loop until the main process has exited.
func (c *Ctl) run(main *Proc, limit time.Duration) error {
	c.mainProc = main
	deadline := time.Now().Add(limit)
	for {
		c.mu.Lock()
		done := main.dead
		c.mu.Unlock()
		if done {
			break
		}
		c.eventStep()
		if time.Now().After(deadline) {
			c.killAll()
			return fmt.Errorf("timeout after %v", limit)
		}
	}
	c.killAll()
	return nil
}

// pump runs the debugger loop for d.
func (c *Ctl) pump(d time.Duration) {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		c.eventStep()
	}
}

func (c *Ctl) killAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.live {
		if !p.exited {
			p.exited = true
			for _, t := range p.threads {
				t.die()
			}
			p.kill()
		}
	}
}

// wxWrite handles a guest write to an executable page: the page becomes writable
// and loses execute permission.
func (as *AS) wxWrite(a uint64) bool {
	v := as.findVMA(a)
	p := as.pages[pageDown(a)]
	if v == nil || p == nil || p.state != 2 || v.prot&2 == 0 || v.prot&4 == 0 || p.dirty {
		return false
	}
	as.dropExec(pageDown(a), p)
	return true
}

// wxExec handles an execute fault on a page that was written: every mapped page of
// the VMA is rescanned (and its syscalls patched) before it becomes executable again.
func (p *Proc) wxExec(a uint64) bool {
	as := p.as
	v := as.findVMA(a)
	q := as.pages[pageDown(a)]
	if v == nil || q == nil || q.state != 2 || !q.dirty || v.prot&4 == 0 {
		return false
	}
	lo, hi, eff := v.lo, v.hi, effProt(v.prot)
	st, err := p.scanRange(lo, hi)
	if err != nil {
		p.c.logf("rescan: %v", err)
	}
	p.stats.add(st)
	for x := lo; x < hi; x += pageSize {
		if pp := as.pages[x]; pp != nil && pp.state == 2 && pp.dirty {
			as.vprotect(x, eff)
			pp.prot, pp.dirty = eff, false
		}
	}
	return true
}
