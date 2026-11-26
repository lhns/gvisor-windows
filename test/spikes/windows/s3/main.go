package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	vehSkip, vehCount, vehCode, vehSegFs, vehReadFs, vehFsBase uint64
)

func vehStub()
func vehAddr() uintptr
func rdfsbase() uintptr
func wrfsbase(v uintptr)
func fs0() uint64
func movfs(sel uint64)
func ud2()
func cpuid(leaf, sub uint32) (a, b, c, d uint32)
func spinfs(stop *uint32, expect uint64) (iters, bad uint64)

var (
	k32 = syscall.NewLazyDLL("kernel32.dll")
	nt  = syscall.NewLazyDLL("ntdll.dll")

	pAddVEH       = k32.NewProc("AddVectoredExceptionHandler")
	pVirtualAlloc = k32.NewProc("VirtualAlloc")
	pOpenThread   = k32.NewProc("OpenThread")
	pSuspend      = k32.NewProc("SuspendThread")
	pResume       = k32.NewProc("ResumeThread")
	pGetCtx       = k32.NewProc("GetThreadContext")
	pSetCtx       = k32.NewProc("SetThreadContext")
	pGetTid       = k32.NewProc("GetCurrentThreadId")
	pSleep        = k32.NewProc("Sleep")
	pNtSetInfo    = nt.NewProc("NtSetInformationThread")
)

const magic = 0xA5A5_1234_5678_9ABC

func report(ok bool, name, detail string) {
	s := "PASS"
	if !ok {
		s = "FAIL"
	}
	fmt.Printf("%s %s: %s\n", s, name, detail)
}

// guarded runs f with the VEH armed to skip `skip` bytes on an exception; returns the exception code (0 = none).
func guarded(skip uint64, readFs bool, f func()) (code uint64, segfs uint64, fsInHandler uint64) {
	atomic.StoreUint64(&vehCount, 0)
	atomic.StoreUint64(&vehCode, 0)
	if readFs {
		vehReadFs = 1
	} else {
		vehReadFs = 0
	}
	atomic.StoreUint64(&vehSkip, skip)
	f()
	atomic.StoreUint64(&vehSkip, 0)
	if vehCount > 0 {
		return vehCode, vehSegFs, vehFsBase
	}
	return 0, 0, 0
}

func alloc(n uintptr) uintptr {
	p, _, _ := pVirtualAlloc.Call(0, n, 0x3000, 0x04)
	return p
}

type ctxBuf struct{ p uintptr }

func newCtx() ctxBuf                      { return ctxBuf{alloc(4096)} }
func (c ctxBuf) u32(off uintptr) *uint32 { return (*uint32)(unsafe.Pointer(c.p + off)) }
func (c ctxBuf) u16(off uintptr) *uint16 { return (*uint16)(unsafe.Pointer(c.p + off)) }

const ctxFlagsOff, segFsOff = 0x30, 0x3E
const ctxCtlIntSeg = 0x100007

func main() {
	runtime.LockOSThread()
	h, _, _ := pAddVEH.Call(1, vehAddr())
	fmt.Printf("INFO VEH handle=%#x os=windows/amd64 cpus=%d\n", h, runtime.NumCPU())

	_, b7, _, _ := cpuid(7, 0)
	fmt.Printf("INFO CPUID.7.0:EBX=%#x FSGSBASE(bit0)=%d\n", b7, b7&1)
	var base uintptr
	code, _, _ := guarded(5, false, func() { base = rdfsbase() })
	if code != 0 {
		report(false, "S3.rdfsbase", fmt.Sprintf("exception %#x (CR4.FSGSBASE not enabled for user mode?)", code))
	} else {
		report(true, "S3.rdfsbase", fmt.Sprintf("executes, initial FS base=%#x", base))
	}
	page := alloc(1 << 16)
	*(*uint64)(unsafe.Pointer(page)) = magic
	code, _, _ = guarded(5, false, func() { wrfsbase(page) })
	if code != 0 {
		report(false, "S3.wrfsbase", fmt.Sprintf("exception %#x", code))
		fmt.Println("INFO wrfsbase unavailable; skipping dependent tests")
		tryOtherAPIs()
		fsProbe()
		return
	}
	got := rdfsbase()
	report(got == page && fs0() == magic, "S3.wrfsbase", fmt.Sprintf("set FS base=%#x readback=%#x fs:[0]=%#x", page, got, fs0()))

	bad := 0
	for i := 0; i < 200; i++ {
		pSleep.Call(1)
		if fs0() != magic {
			bad++
		}
	}
	report(bad == 0, "S3.survive_Sleep", fmt.Sprintf("200 x Sleep(1): %d mismatches", bad))

	var stopSpin uint32
	var wg sync.WaitGroup
	n := runtime.NumCPU() * 2
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x := 0
			for atomic.LoadUint32(&stopSpin) == 0 {
				x++
			}
		}()
	}
	var stop uint32
	go func() { time.Sleep(3 * time.Second); atomic.StoreUint32(&stop, 1) }()
	iters, badn := spinfs(&stop, magic)
	atomic.StoreUint32(&stopSpin, 1)
	wg.Wait()
	report(badn == 0, "S3.survive_preemption", fmt.Sprintf("3s spin vs %d busy threads (%d cpus): %d reads, %d mismatches", n, runtime.NumCPU(), iters, badn))

	code, segfs, fsh := guarded(2, true, ud2)
	after := fs0()
	report(code == 0xC000001D && fsh == uint64(page) && after == magic, "S3.survive_exception",
		fmt.Sprintf("ud2 -> VEH code=%#x; FS base seen inside handler=%#x (expect %#x); CONTEXT.SegFs=%#x; fs:[0] after resume=%#x", code, fsh, page, segfs, after))

	ctxTest(page)
	tryOtherAPIs()
	fsProbe()
}

func runTarget(page uintptr, stop *uint32) (chan uint32, chan [2]uint64) {
	ready := make(chan uint32)
	res := make(chan [2]uint64)
	go func() {
		runtime.LockOSThread()
		tid, _, _ := pGetTid.Call()
		wrfsbase(page)
		ready <- uint32(tid)
		it, bad := spinfs(stop, magic)
		res <- [2]uint64{it, bad}
	}()
	return ready, res
}

func ctxTest(page uintptr) {
	c := newCtx()
	names := []string{"identical CONTEXT written back", "SegFs=0x53 written", "SegFs=0 (null) written"}
	for round := 0; round < 3; round++ {
		var stop uint32
		ready, res := runTarget(page, &stop)
		tid := <-ready
		time.Sleep(30 * time.Millisecond)
		h, _, _ := pOpenThread.Call(0x1FFFFF, 0, uintptr(tid))
		pSuspend.Call(h)
		*c.u32(ctxFlagsOff) = ctxCtlIntSeg
		r, _, e := pGetCtx.Call(h, c.p)
		seg := *c.u16(segFsOff)
		switch round {
		case 1:
			*c.u16(segFsOff) = 0x53
		case 2:
			*c.u16(segFsOff) = 0
		}
		r2, _, _ := pSetCtx.Call(h, c.p)
		pResume.Call(h)
		time.Sleep(200 * time.Millisecond)
		atomic.StoreUint32(&stop, 1)
		rr := <-res
		report(rr[1] == 0, "S3.ctx["+names[round]+"]", fmt.Sprintf("GetThreadContext ok=%d(%v) SegFs read=%#x; SetThreadContext ok=%d; %d mismatches of %d reads (mismatch => FS base lost/changed)", r, e, seg, r2, rr[1], rr[0]))
	}
}

func tryOtherAPIs() {
	for _, sel := range []uint64{0x53, 0x2B, 0x23, 0x0} {
		var b uintptr
		code, _, _ := guarded(2, false, func() { movfs(sel) })
		c2, _, _ := guarded(5, false, func() { b = rdfsbase() })
		fmt.Printf("INFO mov fs,%#x -> exception=%#x; FS base now %#x (rdfsbase exc=%#x)\n", sel, code, b, c2)
	}
	var ldt [16]byte
	cur := ^uintptr(1) // pseudo handle -2 = current thread
	for _, cls := range []uintptr{6, 29, 26, 8} {
		st, _, _ := pNtSetInfo.Call(cur, cls, uintptr(unsafe.Pointer(&ldt[0])), 16)
		fmt.Printf("INFO NtSetInformationThread class %d -> NTSTATUS %#x\n", cls, uint32(st))
	}
}

func fsProbe() {
	for _, sel := range []uint64{0x53, 0x2B, 0x0} {
		guarded(2, false, func() { movfs(sel) })
		var v uint64
		code, _, _ := guarded(9, false, func() { v = fs0() })
		fmt.Printf("INFO after mov fs,%#x: fs:[0] read exception=%#x value=%#x\n", sel, code, v)
	}
}
