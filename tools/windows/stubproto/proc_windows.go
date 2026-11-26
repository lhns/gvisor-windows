package main

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"stubproto/pe"
	"stubproto/stubasm"
)

// Ctl is the controller: one memory file, all stub processes, and the single event
// loop that serves both shared-memory syscall requests and debug events.
type Ctl struct {
	mf                      *memFile
	stubPath                string
	procs                   map[uint32]*Proc   // by Windows pid
	threads                 map[uint32]*Thread // by Windows tid
	live                    []*Proc
	nextPid                 int
	nextTid                 int
	ev                      map[string]int // debug events by kind
	pendings                []*pending
	ctxBuf                  uintptr
	evBuf                   uintptr
	verbose                 bool
	trace                   bool
	stdout                  io.Writer
	stderr                  io.Writer
	ntDelay                 uint64
	imgCode                 []byte
	unknown                 map[uint64]int
	sysCount                map[uint64]int
	stamps                  map[string][]int64 // bench: arrival times (ns) by path
	benchOn                 bool
	times                   map[string]time.Duration // named measurements
	start                   time.Time
	mainProc                *Proc
	udServed, patchedServed int
	probeAddr               uint64
	holdProbe               bool
	holdSamples             []uint64
	lastEvent               time.Time
	forkDurs                []time.Duration
	runDelta                []uint64
	mainDur                 time.Duration
	mainStats               siteStats
}

type Proc struct {
	c         *Ctl
	pid       int
	wpid      uint32
	h         uintptr
	hMain     uintptr
	mainWtid  uint32
	as        *AS
	ctlView   unsafe.Pointer
	threads   []*Thread
	nsites    int
	parent    *Proc
	children  []*Proc
	exited    bool
	exitCode  int
	termSig   int
	reaped    bool
	brkLo     uint64
	brkCur    uint64
	created   time.Time
	gotFirst  bool
	forkStart time.Time
	isFork    bool
	stats     siteStats
	used      []rng
	reserved  uint64
	name      string
	alive     int
	mmapTop   uint64
	dead      bool
	seenBP    bool
	firstDur  time.Duration
	forkDur   time.Duration
	ctlSec    uintptr
	cleaned   bool
}

type Thread struct {
	p          *Proc
	idx        int
	tid        int
	wtid       uint32
	h          uintptr
	page       unsafe.Pointer
	addr       uint64 // address of the page in the stub
	clearTid   uint64
	dead       bool
	waiting    bool
	viaUD2     bool
	lastRip    uint64
	repairN    int
	lastRepair time.Time
}

type pending struct {
	t    *Thread
	poll func() (int64, bool)
}

func (t *Thread) u64(off int) *uint64 { return (*uint64)(unsafe.Add(t.page, off)) }
func (t *Thread) state() uint32       { return atomic.LoadUint32((*uint32)(t.page)) }
func (t *Thread) setState(v uint32)   { atomic.StoreUint32((*uint32)(t.page), v) }
func (t *Thread) reg(i int) uint64    { return *t.u64(stubasm.PgRegs + 8*i) }
func (t *Thread) setReg(i int, v uint64) {
	*t.u64(stubasm.PgRegs + 8*i) = v
}

// register numbers in hardware order
const (
	rAX = 0
	rCX = 1
	rDX = 2
	rBX = 3
	rSP = 4
	rBP = 5
	rSI = 6
	rDI = 7
	r8  = 8
	r9  = 9
	r10 = 10
)

func newCtl(stubPath string, memSize uint64) (*Ctl, error) {
	mf, err := newMemFile(memSize)
	if err != nil {
		return nil, err
	}
	c := &Ctl{
		mf: mf, stubPath: stubPath, procs: map[uint32]*Proc{}, threads: map[uint32]*Thread{},
		nextPid: 100, nextTid: 100, ev: map[string]int{}, unknown: map[uint64]int{},
		sysCount: map[uint64]int{}, stamps: map[string][]int64{}, times: map[string]time.Duration{},
		stdout: os.Stdout, stderr: os.Stderr, start: time.Now(),
	}
	c.ctxBuf = valloc(4096)
	c.evBuf = valloc(4096)
	if err := pNtDelay.Find(); err != nil {
		return nil, err
	}
	c.ntDelay = uint64(pNtDelay.Addr())
	c.imgCode = stubasm.Image()
	img := pe.Stub(stubasm.CtlBase, stubasm.StubCode+stubasm.OffThreadEnt)
	if err := os.WriteFile(stubPath, img, 0o755); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Ctl) logf(format string, a ...any) {
	if c.verbose {
		fmt.Fprintf(c.stderr, "stubproto: "+format+"\n", a...)
	}
}

// newProc starts a suspended stub, reserves its guest range and maps its control area.
func (c *Ctl) newProc(name string) (*Proc, error) {
	t0 := time.Now()
	cl, _ := syscall.UTF16PtrFromString(`"` + c.stubPath + `"`)
	var si syscall.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi syscall.ProcessInformation
	const createSuspended, debugOnlyThis, noWindow = 0x4, 0x2, 0x08000000
	if err := syscall.CreateProcess(nil, cl, nil, nil, false, createSuspended|debugOnlyThis|noWindow, nil, nil, &si, &pi); err != nil {
		return nil, fmt.Errorf("CreateProcess: %w", err)
	}
	p := &Proc{c: c, wpid: pi.ProcessId, h: uintptr(pi.Process), hMain: uintptr(pi.Thread), mainWtid: pi.ThreadId,
		pid: c.nextPid, name: name, created: t0, mmapTop: mmapTop}
	c.nextPid++
	p.as = newAS(c.mf, p.h)
	p.used = queryUsed(p.h, stubGuestMin, stubGuestMax)
	var err error
	if p.reserved, err = p.as.reserve(p.used); err != nil {
		p.kill()
		return nil, err
	}
	sec, _, e := pCreateMapping.Call(^uintptr(0), 0, pageExecRW, uintptr(stubasm.CtlSize>>32), uintptr(stubasm.CtlSize), 0)
	if sec == 0 {
		p.kill()
		return nil, fmt.Errorf("control section: %v", e)
	}
	v, _, e := pMapViewOfFile.Call(sec, fileMapAllExec, 0, 0, 0)
	if v == 0 {
		p.kill()
		return nil, fmt.Errorf("control view: %v", e)
	}
	p.ctlView, p.ctlSec = ptr(v), sec
	if r, _, e := pVirtualFreeEx.Call(p.h, uintptr(stubasm.CtlBase), uintptr(stubasm.CtlSize), memRelease|memPreservePH); r == 0 {
		p.kill()
		return nil, fmt.Errorf("split control range: %v", e)
	}
	if r, _, e := pMapViewOfFile3.Call(sec, p.h, uintptr(stubasm.CtlBase), 0, uintptr(stubasm.CtlSize), memReplacePH, pageExecRW, 0, 0); r == 0 {
		p.kill()
		return nil, fmt.Errorf("map control range: %v", e)
	}
	p.as.insertVMA(vma{stubasm.CtlBase, stubasm.CtlBase + stubasm.CtlSize, 7, "ctl"})
	c.procs[p.wpid] = p
	c.live = append(c.live, p)
	return p, nil
}

func (p *Proc) kill() { pTerminateProcess.Call(p.h, 0xDEAD) }

func (p *Proc) setupStubArea() error {
	as := p.as
	if err := as.addMapped(stubasm.TrapPtrs, stubasm.TrapPtrs+stubasm.TrapPtrsSize, 1, "trap-ptrs"); err != nil {
		return err
	}
	if err := as.addMapped(stubasm.TrapCode, stubasm.TrapCode+stubasm.TrapCodeSize, 5, "trap-code"); err != nil {
		return err
	}
	if err := as.addMapped(stubasm.StubCode, stubasm.StubCode+stubasm.StubCodeSize, 5, "stub-code"); err != nil {
		return err
	}
	if !as.write(stubasm.StubCode, p.c.imgCode) {
		return fmt.Errorf("cannot write stub code")
	}
	return nil
}

func (p *Proc) newThread() (*Thread, error) {
	idx := len(p.threads)
	if idx >= stubasm.MaxThreads {
		return nil, fmt.Errorf("too many threads")
	}
	addr := stubasm.CtlBase + uint64(idx)*stubasm.ThreadRegion
	t := &Thread{p: p, idx: idx, tid: p.c.nextTid, addr: addr,
		page: unsafe.Add(p.ctlView, uint64(idx)*stubasm.ThreadRegion)}
	p.c.nextTid++
	*t.u64(stubasm.PgNtDelay) = p.c.ntDelay
	*t.u64(stubasm.PgStkTop) = addr + stubasm.ThreadRegion - 0x100
	*t.u64(stubasm.PgSpins) = 30000
	*t.u64(stubasm.PgDelay) = uint64(0xFFFFFFFFFFFFEC78) // -5000 = 500 us
	code := stubasm.RepairCode(addr)
	copy(unsafe.Slice((*byte)(unsafe.Add(t.page, stubasm.PgRepair)), len(code)), code)
	p.threads = append(p.threads, t)
	p.alive++
	return t, nil
}

func (t *Thread) register(wtid uint32, h uintptr) {
	t.wtid, t.h = wtid, h
	t.p.c.threads[wtid] = t
}

// spawnMain loads the ELF and starts the first thread.
func (c *Ctl) spawnMain(path string, args []string) (*Proc, error) {
	p, err := c.newProc(path)
	if err != nil {
		return nil, err
	}
	if err := p.setupStubArea(); err != nil {
		return nil, err
	}
	ld, err := loadELF(p.as, path)
	if err != nil {
		return nil, err
	}
	for _, r := range ld.exec {
		st, err := p.scanAndPatch(r.lo, r.hi)
		if err != nil {
			return nil, err
		}
		p.stats.add(st)
	}
	sp, err := buildStack(p.as, ld, args, []string{"HOME=/", "PATH=/bin", "TERM=dumb"})
	if err != nil {
		return nil, err
	}
	p.brkLo, p.brkCur = ld.brk, ld.brk
	t, err := p.newThread()
	if err != nil {
		return nil, err
	}
	t.setReg(rSP, sp)
	*t.u64(stubasm.PgRIP) = ld.entry
	t.register(p.mainWtid, p.hMain)
	c.logf("pid %d loaded %s entry=%#x sites: %+v, reserved %d GiB, windows holes %v", p.pid, path, ld.entry, p.stats, p.reserved>>30, p.used)
	if r, _, e := pResumeThread.Call(p.hMain); int32(r) == -1 {
		return nil, fmt.Errorf("ResumeThread: %v", e)
	}
	return p, nil
}

func (c *Ctl) exitProc(p *Proc, code, sig int) {
	if p.exited {
		return
	}
	p.exited, p.exitCode, p.termSig = true, code, sig
	for _, t := range p.threads {
		t.dead = true
	}
}

// doFork clones the calling thread's process into a new stub (copy, not COW).
func (c *Ctl) doFork(t *Thread, flags uint64, ctid uint64) int64 {
	parent := t.p
	t0 := time.Now()
	var child *Proc
	for try := 0; try < 8; try++ {
		cp, err := c.newProc(parent.name)
		if err != nil {
			c.logf("fork: %v", err)
			return -12
		}
		if err := parent.as.cloneInto(cp.as); err != nil {
			c.logf("fork attempt %d: %v", try, err)
			cp.kill()
			cp.exited = true
			continue
		}
		child = cp
		break
	}
	if child == nil {
		return -12
	}
	child.parent, child.isFork, child.forkStart = parent, true, t0
	parent.children = append(parent.children, child)
	child.nsites, child.brkLo, child.brkCur, child.mmapTop = parent.nsites, parent.brkLo, parent.brkCur, parent.mmapTop
	ct, err := child.newThread()
	if err != nil {
		return -12
	}
	for i := 0; i < 16; i++ {
		ct.setReg(i, t.reg(i))
	}
	ct.setReg(rAX, 0)
	*ct.u64(stubasm.PgRIP) = *t.u64(stubasm.PgRIP)
	*ct.u64(stubasm.PgFS) = *t.u64(stubasm.PgFS)
	ct.register(child.mainWtid, child.hMain)
	if flags&0x01000000 != 0 && ctid != 0 { // CLONE_CHILD_SETTID
		child.as.write(ctid, putLE(uint64(ct.tid), 4))
	}
	if flags&0x00200000 != 0 { // CLONE_CHILD_CLEARTID
		ct.clearTid = ctid
	}
	pResumeThread.Call(child.hMain)
	c.times["fork-call"] = time.Since(t0)
	return int64(child.pid)
}

// doCloneThread creates a new thread of t's process.
func (c *Ctl) doCloneThread(t *Thread, flags, stack, ptid, ctid, tls uint64) int64 {
	p := t.p
	nt, err := p.newThread()
	if err != nil {
		return -11
	}
	for i := 0; i < 16; i++ {
		nt.setReg(i, t.reg(i))
	}
	nt.setReg(rAX, 0)
	if stack != 0 {
		nt.setReg(rSP, stack)
	}
	*nt.u64(stubasm.PgRIP) = *t.u64(stubasm.PgRIP)
	*nt.u64(stubasm.PgFS) = *t.u64(stubasm.PgFS)
	if flags&0x80000 != 0 { // CLONE_SETTLS
		*nt.u64(stubasm.PgFS) = tls
	}
	if flags&0x100000 != 0 && ptid != 0 {
		p.as.write(ptid, putLE(uint64(nt.tid), 4))
	}
	if flags&0x01000000 != 0 && ctid != 0 {
		p.as.write(ctid, putLE(uint64(nt.tid), 4))
	}
	if flags&0x00200000 != 0 {
		nt.clearTid = ctid
	}
	var wtid uint32
	h, _, e := pCreateRemoteThr.Call(p.h, 0, 0, uintptr(stubasm.StubCode+stubasm.OffThreadEnt), uintptr(nt.addr), 0, uintptr(unsafe.Pointer(&wtid)))
	if h == 0 {
		c.logf("CreateRemoteThread: %v", e)
		nt.dead = true
		p.alive--
		return -11
	}
	nt.register(wtid, h)
	return int64(nt.tid)
}

// cleanup returns the process's memory-file pages and control area.
func (p *Proc) cleanup() {
	if p.cleaned {
		return
	}
	p.cleaned = true
	for _, pgs := range p.as.pages {
		if pgs.state == 2 {
			p.as.mf.release(pgs.off)
			pgs.state = 1
		}
	}
	pUnmapViewOfFile.Call(uintptr(p.ctlView))
	pCloseHandle.Call(p.ctlSec)
	pCloseHandle.Call(p.h)
	pCloseHandle.Call(p.hMain)
}
