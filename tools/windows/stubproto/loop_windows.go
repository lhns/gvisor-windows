package main

import (
	"fmt"
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

// poll serves pending shared-memory syscall requests and completes blocked calls.
func (c *Ctl) poll() bool {
	any := false
	for _, p := range c.live {
		if p.exited {
			continue
		}
		for _, t := range p.threads {
			if !t.dead && !t.waiting && t.state() == 1 {
				c.serve(t)
				any = true
			}
		}
	}
	if len(c.pendings) > 0 && c.runPendings() {
		any = true
	}
	return any
}

func (c *Ctl) serve(t *Thread) {
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
		return
	}
	ret, st := c.syscall(t, nr, a)
	if c.trace {
		fmt.Fprintf(c.stderr, "trace pid %d tid %d: syscall %d(%#x, %#x, %#x, %#x) = %d [%d] rip=%#x\n", p.pid, t.tid, nr, a[0], a[1], a[2], a[3], ret, st, *t.u64(stubasm.PgRIP))
	}
	if st == stRet {
		c.reply(t, ret)
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
			if rip == t.lastRip && now.Sub(t.lastRepair) < time.Millisecond {
				t.repairN++
			} else {
				t.repairN = 0
			}
			t.lastRip, t.lastRepair = rip, now
			if t.repairN < 3 {
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
			t.setState(1)
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

// step runs one iteration of the event loop.
func (c *Ctl) step(idle bool) {
	for i := 0; i < 100; i++ {
		if c.poll() {
			i = 0
			idle = false
		}
	}
	to := uintptr(0)
	if idle {
		to = 1
	}
	r, _, _ := pWaitDebugEvent.Call(c.evBuf, to)
	if r == 0 {
		return
	}
	status := c.handleEvent()
	pContinueDebug.Call(uintptr(u32at(c.evBuf, 4)), uintptr(u32at(c.evBuf, 8)), uintptr(status))
	c.lastEvent = time.Now()
}

// run drives the loop until the main process has exited.
func (c *Ctl) run(main *Proc, limit time.Duration) error {
	c.mainProc = main
	deadline := time.Now().Add(limit)
	c.lastEvent = time.Now()
	for !main.dead {
		c.step(time.Since(c.lastEvent) > 2*time.Millisecond)
		if time.Now().After(deadline) {
			c.killAll()
			return fmt.Errorf("timeout after %v", limit)
		}
	}
	c.killAll()
	return nil
}

// pump runs the loop for d.
func (c *Ctl) pump(d time.Duration) {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		c.step(false)
	}
}

func (c *Ctl) killAll() {
	for _, p := range c.live {
		if !p.exited {
			p.exited = true
			p.kill()
		}
	}
}

var _ = unsafe.Pointer(nil)
