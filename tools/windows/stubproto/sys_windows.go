package main

import (
	"crypto/rand"
	"fmt"
	"time"

	"stubproto/stubasm"
)

const (
	stRet     = 0 // reply now
	stBlocked = 1 // a pending completion will reply
	stNoReply = 2 // the thread or process is gone
)

const (
	eperm     = 1
	enoent    = 2
	ebadf     = 9
	echild    = 10
	eagain    = 11
	enomem    = 12
	efault    = 14
	eexist    = 17
	enodev    = 19
	einval    = 22
	enotty    = 25
	espipe    = 29
	enosys    = 38
	etimedout = 110
)

type waiter struct {
	p     *Proc
	addr  uint64
	woken bool
}

var futexQ []*waiter

const monoOffset = time.Hour

func (c *Ctl) monoNow() time.Duration { return monoOffset + time.Since(c.start) }

func (c *Ctl) block(t *Thread, poll func() (int64, bool)) (int64, int) {
	t.waiting = true
	c.pendings = append(c.pendings, &pending{t, poll})
	return 0, stBlocked
}

func (c *Ctl) runPendings() bool {
	any := false
	out := c.pendings[:0]
	for _, pe := range c.pendings {
		if pe.t.dead || pe.t.p.exited {
			continue
		}
		if ret, done := pe.poll(); done {
			c.reply(pe.t, ret)
			any = true
			continue
		}
		out = append(out, pe)
	}
	c.pendings = out
	return any
}

func (c *Ctl) reply(t *Thread, ret int64) {
	*t.u64(stubasm.PgReply) = uint64(ret)
	t.waiting = false
	t.setState(2)
}

func (p *Proc) fileStat(buf uint64) int64 {
	st := make([]byte, 144)
	copy(st[16:], putLE(1, 8))        // nlink
	copy(st[24:], putLE(0o010600, 4)) // S_IFIFO|0600
	copy(st[56:], putLE(4096, 8))     // blksize
	if !p.as.write(buf, st) {
		return -efault
	}
	return 0
}

func timespecDur(p *Proc, a uint64) (time.Duration, bool) {
	b, ok := p.as.read(a, 16)
	if !ok {
		return 0, false
	}
	return time.Duration(int64(le64(b)))*time.Second + time.Duration(int64(le64(b[8:]))), true
}

func (c *Ctl) wake(p *Proc, addr uint64, n int) int {
	woke := 0
	out := futexQ[:0]
	for _, w := range futexQ {
		if woke < n && w.p == p && w.addr == addr && !w.woken {
			w.woken = true
			woke++
			continue
		}
		out = append(out, w)
	}
	futexQ = out
	return woke
}

func (c *Ctl) waitChild(t *Thread, want int32, status uint64, opts uint64) (int64, int) {
	p := t.p
	try := func() (int64, bool) {
		any := false
		for _, ch := range p.children {
			if ch.reaped || (want > 0 && int32(ch.pid) != want) {
				continue
			}
			any = true
			if ch.exited {
				ch.reaped = true
				if status != 0 {
					st := uint64(ch.exitCode&0xff) << 8
					if ch.termSig != 0 {
						st = uint64(ch.termSig)
					}
					p.as.write(status, putLE(st, 4))
				}
				return int64(ch.pid), true
			}
		}
		if !any {
			return -echild, true
		}
		if opts&1 != 0 {
			return 0, true
		}
		return 0, false
	}
	if r, done := try(); done {
		return r, stRet
	}
	return c.block(t, try)
}

func (c *Ctl) killSelf(p *Proc, sig int) {
	c.exitProc(p, 128+sig, sig)
	pTerminateProcess.Call(p.h, uintptr(128+sig))
}

// syscall implements the Linux system calls of the prototype.
func (c *Ctl) syscall(t *Thread, nr uint64, a [6]uint64) (int64, int) {
	p := t.p
	as := p.as
	switch nr {
	case 0: // read
		if a[0] == 0 {
			return 0, stRet
		}
		return -ebadf, stRet
	case 1: // write
		if a[0] != 1 && a[0] != 2 {
			return -ebadf, stRet
		}
		n := int(min(a[2], 1<<20))
		b, ok := as.read(a[1], n)
		if !ok {
			return -efault, stRet
		}
		if a[0] == 1 {
			c.stdout.Write(b)
		} else {
			c.stderr.Write(b)
		}
		return int64(n), stRet
	case 20: // writev
		var total int64
		for i := uint64(0); i < min(a[2], 1024); i++ {
			iov, ok := as.read(a[1]+16*i, 16)
			if !ok {
				return -efault, stRet
			}
			base, n := le64(iov), int(min(le64(iov[8:]), 1<<20))
			b, ok := as.read(base, n)
			if !ok {
				return -efault, stRet
			}
			if a[0] == 1 {
				c.stdout.Write(b)
			} else if a[0] == 2 {
				c.stderr.Write(b)
			} else {
				return -ebadf, stRet
			}
			total += int64(n)
		}
		return total, stRet
	case 3, 24, 28, 273, 72: // close sched_yield madvise set_robust_list fcntl
		return 0, stRet
	case 2, 21, 89, 257, 267: // open access readlink openat readlinkat
		return -enoent, stRet
	case 5: // fstat
		if a[0] > 2 {
			return -ebadf, stRet
		}
		return p.fileStat(a[1]), stRet
	case 262: // newfstatat
		if a[3]&0x1000 != 0 && a[0] <= 2 {
			return p.fileStat(a[2]), stRet
		}
		return -enoent, stRet
	case 8:
		return -espipe, stRet
	case 16: // ioctl
		return -enotty, stRet
	case 79: // getcwd
		as.write(a[0], []byte{'/', 0})
		return 2, stRet
	case 12: // brk
		if a[0] == 0 || a[0] < p.brkLo {
			return int64(p.brkCur), stRet
		}
		if a[0] > p.brkLo+brkMaxSize {
			return int64(p.brkCur), stRet
		}
		oldEnd, newEnd := pageUp(p.brkCur), pageUp(a[0])
		if newEnd > oldEnd {
			if err := as.addVMA(oldEnd, newEnd, 3, "heap"); err != nil {
				return int64(p.brkCur), stRet
			}
		} else if newEnd < oldEnd {
			as.removeRange(newEnd, oldEnd)
		}
		p.brkCur = a[0]
		return int64(a[0]), stRet
	case 9: // mmap
		if a[3]&0x20 == 0 {
			return -enodev, stRet
		}
		length := pageUp(a[1])
		if length == 0 {
			return -einval, stRet
		}
		prot := int(a[2] & 7)
		fixed := a[3]&0x10 != 0 || a[3]&0x100000 != 0
		var base uint64
		switch {
		case fixed:
			base = a[0]
			if base&(pageSize-1) != 0 || !as.inGap(base, base+length) {
				return -enomem, stRet
			}
			if a[3]&0x100000 != 0 && as.overlaps(base, base+length) {
				return -eexist, stRet
			}
			if v := as.findVMA(base); (v != nil && (v.kind == "ctl" || v.kind == "trap-ptrs" || v.kind == "trap-code" || v.kind == "stub-code")) || as.overlapsKind(base, base+length) {
				return -einval, stRet
			}
			as.removeRange(base, base+length)
		default:
			if a[0] != 0 {
				h := pageDown(a[0])
				if as.inGap(h, h+length) && !as.overlaps(h, h+length) {
					base = h
				}
			}
			if base == 0 {
				b, ok := as.findFree(length, p.mmapTop)
				if !ok {
					return -enomem, stRet
				}
				base = b
			}
		}
		if err := as.addVMA(base, base+length, prot, "mmap"); err != nil {
			return -enomem, stRet
		}
		return int64(base), stRet
	case 11: // munmap
		if a[0]&(pageSize-1) != 0 || a[1] == 0 {
			return -einval, stRet
		}
		lo, hi := a[0], a[0]+pageUp(a[1])
		if as.overlapsKind(lo, hi) {
			return -einval, stRet
		}
		as.removeRange(lo, hi)
		return 0, stRet
	case 10: // mprotect
		if a[0]&(pageSize-1) != 0 {
			return -einval, stRet
		}
		lo, hi := a[0], a[0]+pageUp(a[1])
		prot := int(a[2] & 7)
		if as.overlapsKind(lo, hi) || !as.protect(lo, hi, prot) {
			return -enomem, stRet
		}
		if prot&4 != 0 {
			st, err := p.scanAndPatch(lo, hi)
			if err != nil {
				c.logf("mprotect scan: %v", err)
			}
			p.stats.add(st)
		}
		return 0, stRet
	case 63: // uname
		u := make([]byte, 6*65)
		for i, s := range []string{"Linux", "stubproto", "5.15.0-stubproto", "#1 SMP", "x86_64", ""} {
			copy(u[i*65:], s)
		}
		if !as.write(a[0], u) {
			return -efault, stRet
		}
		return 0, stRet
	case 39:
		return int64(p.pid), stRet
	case 186:
		return int64(t.tid), stRet
	case 110:
		if p.parent != nil {
			return int64(p.parent.pid), stRet
		}
		return 1, stRet
	case 102, 104, 107, 108: // getuid getgid geteuid getegid
		return 1000, stRet
	case 218: // set_tid_address
		t.clearTid = a[0]
		return int64(t.tid), stRet
	case 14: // rt_sigprocmask
		if a[2] != 0 {
			as.write(a[2], make([]byte, min(a[3], 128)))
		}
		return 0, stRet
	case 13: // rt_sigaction
		if a[2] != 0 {
			as.write(a[2], make([]byte, 32))
		}
		return 0, stRet
	case 131: // sigaltstack
		if a[1] != 0 {
			o := make([]byte, 24)
			copy(o[8:], putLE(2, 4)) // SS_DISABLE
			as.write(a[1], o)
		}
		return 0, stRet
	case 97, 302: // getrlimit prlimit64
		old := a[1]
		res := a[0]
		if nr == 302 {
			old, res = a[3], a[1]
		}
		if old != 0 {
			cur := ^uint64(0)
			if res == 3 {
				cur = 8 << 20
			}
			as.write(old, append(putLE(cur, 8), putLE(^uint64(0), 8)...))
		}
		return 0, stRet
	case 204: // sched_getaffinity
		if a[1] < 8 {
			return -einval, stRet
		}
		as.write(a[2], putLE(1, 8))
		return 8, stRet
	case 228: // clock_gettime
		var d time.Duration
		if a[0] == 0 || a[0] == 5 {
			d = time.Duration(time.Now().UnixNano())
		} else {
			d = c.monoNow()
		}
		as.write(a[1], append(putLE(uint64(d/time.Second), 8), putLE(uint64(d%time.Second), 8)...))
		return 0, stRet
	case 96: // gettimeofday
		if a[0] != 0 {
			n := time.Now()
			as.write(a[0], append(putLE(uint64(n.Unix()), 8), putLE(uint64(n.Nanosecond()/1000), 8)...))
		}
		return 0, stRet
	case 201: // time
		s := time.Now().Unix()
		if a[0] != 0 {
			as.write(a[0], putLE(uint64(s), 8))
		}
		return s, stRet
	case 318: // getrandom
		n := min(a[1], 1<<20)
		b := make([]byte, n)
		rand.Read(b)
		if !as.write(a[0], b) {
			return -efault, stRet
		}
		return int64(n), stRet
	case 158: // arch_prctl
		switch a[0] {
		case 0x1002:
			*t.u64(stubasm.PgFS) = a[1]
			return 0, stRet
		case 0x1003:
			as.write(a[1], putLE(*t.u64(stubasm.PgFS), 8))
			return 0, stRet
		}
		return -einval, stRet
	case 60: // exit
		t.dead = true
		p.alive--
		if t.clearTid != 0 {
			as.write(t.clearTid, putLE(0, 4))
			c.wake(p, t.clearTid, 1)
		}
		if p.alive <= 0 {
			c.exitProc(p, int(a[0]&0xff), 0)
			pTerminateProcess.Call(p.h, uintptr(a[0]&0xff))
		} else {
			pTerminateThread.Call(t.h, 0)
		}
		return 0, stNoReply
	case 231: // exit_group
		c.exitProc(p, int(a[0]&0xff), 0)
		pTerminateProcess.Call(p.h, uintptr(a[0]&0xff))
		return 0, stNoReply
	case 62, 200, 234: // kill tkill tgkill
		sig := a[1]
		if nr == 234 {
			sig = a[2]
		}
		switch sig {
		case 0, 17, 23, 28:
			return 0, stRet
		}
		c.killSelf(p, int(sig))
		return 0, stNoReply
	case 57, 58: // fork vfork
		return c.doFork(t, 0x11, 0), stRet
	case 56: // clone
		flags := a[0]
		if flags&0x10000 != 0 {
			return c.doCloneThread(t, flags, a[1], a[2], a[3], a[4]), stRet
		}
		return c.doFork(t, flags, a[3]), stRet
	case 61: // wait4
		return c.waitChild(t, int32(a[0]), a[1], a[2])
	case 202: // futex
		switch a[1] & 0x7F {
		case 0, 9:
			v, ok := as.readU32(a[0])
			if !ok {
				return -efault, stRet
			}
			if v != uint32(a[2]) {
				return -eagain, stRet
			}
			var deadline time.Time
			if a[3] != 0 {
				d, ok := timespecDur(p, a[3])
				if !ok {
					return -efault, stRet
				}
				switch {
				case a[1]&0x7F == 0:
					deadline = time.Now().Add(d)
				case a[1]&256 != 0:
					deadline = time.Unix(0, int64(d))
				default:
					deadline = c.start.Add(d - monoOffset)
				}
			}
			w := &waiter{p: p, addr: a[0]}
			futexQ = append(futexQ, w)
			return c.block(t, func() (int64, bool) {
				if w.woken {
					return 0, true
				}
				if !deadline.IsZero() && time.Now().After(deadline) {
					c.wake(p, a[0], 0)
					w.woken = true
					return -etimedout, true
				}
				return 0, false
			})
		case 1, 10:
			return int64(c.wake(p, a[0], int(int32(a[2])))), stRet
		}
		return -enosys, stRet
	case 35, 230: // nanosleep clock_nanosleep
		req := a[0]
		var abs bool
		if nr == 230 {
			req, abs = a[2], a[1]&1 != 0
		}
		d, ok := timespecDur(p, req)
		if !ok {
			return -efault, stRet
		}
		deadline := time.Now().Add(d)
		if abs {
			deadline = c.start.Add(d - monoOffset)
		}
		var v0 uint64
		if c.probeAddr != 0 {
			v0, _ = as.readU64(c.probeAddr)
		}
		return c.block(t, func() (int64, bool) {
			if !time.Now().After(deadline) {
				return 0, false
			}
			if c.probeAddr != 0 {
				v1, _ := as.readU64(c.probeAddr)
				c.runDelta = append(c.runDelta, v1-v0)
			}
			return 0, true
		})
	}
	c.sysCount[nr]++
	if c.unknown[nr]++; c.unknown[nr] == 1 {
		fmt.Fprintf(c.stderr, "stubproto: pid %d: unimplemented syscall %d -> ENOSYS\n", p.pid, nr)
	}
	return -enosys, stRet
}

func (as *AS) overlapsKind(lo, hi uint64) bool {
	for _, v := range as.vmas {
		if v.lo < hi && v.hi > lo && (v.kind == "ctl" || v.kind == "trap-ptrs" || v.kind == "trap-code" || v.kind == "stub-code") {
			return true
		}
	}
	return false
}
