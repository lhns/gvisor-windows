package main

// S3: can a Linux guest's %fs base be set from user mode on Windows, and what keeps it?
// A raw-assembly vectored handler (asm_amd64.s) detects and repairs FS-base loss:
// every repair is one observed loss. Run with GODEBUG=asyncpreemptoff=1.

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Shared with the assembly handler.
var (
	fsTable                                                   [256]uint64
	repairs, lastFaultAddr, lastBaseAtLoss                    uint64
	lastSelCtx, lastSelHW, selExpected, selMismatch           uint64
	fsUsable, vehSkip, vehCount, vehFsSeen, vehLostAtDispatch uint64
)

func vehStub()
func vehAddr() uintptr
func tid() uint64
func rdfsbase() uintptr
func wrfsbase(v uintptr)
func selfs() uint64
func fs0() uint64
func ud2()
func rdtsc() uint64
func cpuid(leaf, sub uint32) (a, b, c, d uint32)
func guestloop(stop *uint32, exp *[3]uint64) (iters, bad uint64)
func loseRead(n, off uint64)

var (
	k32           = syscall.NewLazyDLL("kernel32.dll")
	pAddVEH       = k32.NewProc("AddVectoredExceptionHandler")
	pVirtualAlloc = k32.NewProc("VirtualAlloc")
	pOpenThread   = k32.NewProc("OpenThread")
	pSuspend      = k32.NewProc("SuspendThread")
	pResume       = k32.NewProc("ResumeThread")
	pGetCtx       = k32.NewProc("GetThreadContext")
	pSetCtx       = k32.NewProc("SetThreadContext")
	pSleep        = k32.NewProc("Sleep")
	pSwitch       = k32.NewProc("SwitchToThread")
)

const (
	magic0  = 0xA5A5_1234_5678_9ABC
	magic28 = 0x1111_2222_3333_4444
	magicM8 = 0x5555_6666_7777_8888
)

func report(ok bool, name, detail string) {
	s := "PASS"
	if !ok {
		s = "FAIL"
	}
	fmt.Printf("%s %s: %s\n", s, name, detail)
}

func info(f string, a ...any) { fmt.Printf("INFO "+f+"\n", a...) }

func alloc(n uintptr) uintptr {
	p, _, _ := pVirtualAlloc.Call(0, n, 0x3000, 0x04)
	return p
}

func nrepairs() uint64 { return atomic.LoadUint64(&repairs) }

// guestMem returns an FS base with magic values at [0], [0x28] and [-8].
func guestMem() (base uintptr, exp *[3]uint64) {
	region := alloc(1 << 16)
	base = region + 0x1000
	*(*uint64)(unsafe.Pointer(base)) = magic0
	*(*uint64)(unsafe.Pointer(base + 0x28)) = magic28
	*(*uint64)(unsafe.Pointer(base - 8)) = magicM8
	return base, &[3]uint64{magic0, magic28, magicM8}
}

// register sets the calling (locked) thread's FS base and the expected-base slot.
func register(base uintptr) {
	wrfsbase(base)
	atomic.StoreUint64(&fsTable[(tid()>>2)&255], uint64(base))
}

func main() {
	// Spinners never yield (async preemption is off): keep more Ps than spinning goroutines so the
	// runtime itself cannot stall; CPU contention then comes from the OS scheduler only.
	runtime.GOMAXPROCS(runtime.NumCPU()*2 + 16)
	time.AfterFunc(5*time.Minute, func() { fmt.Println("FAIL watchdog: spike hung"); os.Exit(3) })
	runtime.LockOSThread()
	h, _, _ := pAddVEH.Call(1, vehAddr())
	_, b7, _, _ := cpuid(7, 0)
	info("VEH handle=%#x cpus=%d CPUID.7.0:EBX=%#x FSGSBASE(bit0)=%d", h, runtime.NumCPU(), b7, b7&1)

	// 0. feasibility
	base, exp := guestMem()
	atomic.StoreUint64(&vehSkip, 5)
	n0 := vehCount
	_ = rdfsbase()
	atomic.StoreUint64(&vehSkip, 0)
	if vehCount != n0 {
		report(false, "S3.rdfsbase", "raised #UD (0xC000001D): FSGSBASE not usable from user mode on this CPU/OS")
		return
	}
	fsUsable = 1
	orig := rdfsbase()
	register(base)
	selExpected = selfs()
	report(rdfsbase() == base && fs0() == magic0, "S3.wrfsbase", fmt.Sprintf("initial base=%#x; set base=%#x readback=%#x fs:[0]=%#x; FS selector=%#x; thread slot=%d", orig, base, rdfsbase(), fs0(), selExpected, (tid()>>2)&255))

	// 1. voluntary context switches
	for _, c := range []struct {
		name string
		n    int
		f    func()
	}{
		{"Sleep(1)", 2000, func() { pSleep.Call(1) }},
		{"Sleep(0)", 20000, func() { pSleep.Call(0) }},
		{"SwitchToThread", 20000, func() { pSwitch.Call() }},
	} {
		r0 := nrepairs()
		for i := 0; i < c.n; i++ {
			c.f()
			_ = fs0() // faults and is repaired if the base was lost
		}
		lost := nrepairs() - r0
		report(true, "S3.loss_after_"+c.name, fmt.Sprintf("%d losses in %d calls (%.1f%%)", lost, c.n, 100*float64(lost)/float64(c.n)))
	}

	// 2. involuntary preemption: guest-like loop vs 2x NumCPU spinners
	spinners := runtime.NumCPU() * 2
	for _, withSpin := range []bool{false, true} {
		var stopSpin, stop uint32
		var wg sync.WaitGroup
		if withSpin {
			for i := 0; i < spinners; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					x := 0
					for atomic.LoadUint32(&stopSpin) == 0 {
						x++
					}
				}()
			}
		}
		const secs = 5
		go func() { time.Sleep(secs * time.Second); atomic.StoreUint32(&stop, 1) }()
		r0, rp0 := nrepairs(), uint64(0)
		_ = rp0
		it, bad := guestloop(&stop, exp)
		lost := nrepairs() - r0
		atomic.StoreUint32(&stopSpin, 1)
		wg.Wait()
		report(bad == 0, fmt.Sprintf("S3.loss_under_preemption[spinners=%d]", map[bool]int{false: 0, true: spinners}[withSpin]),
			fmt.Sprintf("%d losses in %d s (%.1f/s), %d loop iterations, %d wrong values read", lost, secs, float64(lost)/secs, it, bad))
	}

	// 3. exception dispatch (ud2 + VEH)
	{
		const N = 2000
		r0, l0 := nrepairs(), vehLostAtDispatch
		atomic.StoreUint64(&vehSkip, 2)
		for i := 0; i < N; i++ {
			ud2()
			_ = fs0()
		}
		atomic.StoreUint64(&vehSkip, 0)
		report(true, "S3.loss_across_exception_dispatch", fmt.Sprintf("%d ud2 traps: base already lost when VEH ran: %d; lost by the time the thread resumed (repairs): %d", N, vehLostAtDispatch-l0, nrepairs()-r0))
	}

	// 4. Suspend/Get/SetThreadContext/Resume from another thread
	ctxTest(base, exp)

	// 5. guest-like loops with self-healing, 10 s, 4 guest threads + spinners
	selfHeal(spinners)

	// 6. repair cost and negative offsets
	for _, off := range []uint64{0, 1} {
		const N = 20000
		r0 := nrepairs()
		t0 := time.Now()
		c0 := rdtsc()
		loseRead(N, off)
		c1 := rdtsc()
		d := time.Since(t0)
		_ = c0
		_ = c1
		name := map[uint64]string{0: "fs:[0]", 1: "fs:[-8]"}[off]
		report(nrepairs()-r0 == N, "S3.repair_"+name, fmt.Sprintf("%d/%d faults repaired, %.0f ns per (wrfsbase 0 + faulting read + repair + retry); last fault addr=%#x base-at-loss=%#x", nrepairs()-r0, N, float64(d.Nanoseconds())/N, lastFaultAddr, lastBaseAtLoss))
	}
	report(rdfsbase() == base, "S3.base_intact_at_end", fmt.Sprintf("base=%#x", rdfsbase()))

	// 7. selector
	info("FS selector: expected(at set)=%#x, mismatches seen at loss time=%d, last selector at loss (mov eax,fs)=%#x, last CONTEXT.SegFs at loss=%#x, total losses=%d", selExpected, selMismatch, lastSelHW, lastSelCtx, nrepairs())
}

func selfHeal(spinners int) {
	const secs, guests = 10, 4
	var stop, stopSpin uint32
	var wg, gw sync.WaitGroup
	for i := 0; i < spinners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x := 0
			for atomic.LoadUint32(&stopSpin) == 0 {
				x++
			}
		}()
	}
	var iters, bad uint64
	r0 := nrepairs()
	t0 := time.Now()
	for g := 0; g < guests; g++ {
		gw.Add(1)
		go func() {
			defer gw.Done()
			runtime.LockOSThread()
			base, exp := guestMem()
			register(base)
			it, b := guestloop(&stop, exp)
			atomic.AddUint64(&iters, it)
			atomic.AddUint64(&bad, b)
		}()
	}
	time.Sleep(secs * time.Second)
	atomic.StoreUint32(&stop, 1)
	gw.Wait()
	atomic.StoreUint32(&stopSpin, 1)
	wg.Wait()
	el := time.Since(t0).Seconds()
	rep := nrepairs() - r0
	report(bad == 0, "S3.selfheal_guest_loops",
		fmt.Sprintf("%d guest threads (fs:[0], fs:[0x28], fs:[-8]) + %d spinners, %.0f s: %d iterations, %d wrong values, %d repairs (%.1f/s)", guests, spinners, el, iters, bad, rep, float64(rep)/el))
}

func ctxTest(base uintptr, exp *[3]uint64) {
	const rounds = 300
	type variant struct {
		name     string
		suspend  bool
		get, set bool
	}
	ctxbuf := alloc(4096)
	for _, v := range []variant{
		{"idle (no suspend)", false, false, false},
		{"Suspend+Resume", true, false, false},
		{"Suspend+Get+Resume", true, true, false},
		{"Suspend+Get+Set(same ctx)+Resume", true, true, true},
	} {
		var stop uint32
		ready := make(chan uint32)
		done := make(chan [2]uint64)
		go func() {
			runtime.LockOSThread()
			b, e := guestMem()
			register(b)
			ready <- uint32(tid())
			it, bad := guestloop(&stop, e)
			done <- [2]uint64{it, bad}
		}()
		t := <-ready
		time.Sleep(50 * time.Millisecond)
		hd, _, _ := pOpenThread.Call(0x1FFFFF, 0, uintptr(t))
		r0 := nrepairs()
		for i := 0; i < rounds; i++ {
			if v.suspend {
				pSuspend.Call(hd)
				if v.get {
					*(*uint32)(unsafe.Pointer(ctxbuf + 0x30)) = 0x100007
					pGetCtx.Call(hd, ctxbuf)
				}
				if v.set {
					pSetCtx.Call(hd, ctxbuf)
				}
				pResume.Call(hd)
			}
			pSleep.Call(3)
		}
		lost := nrepairs() - r0
		atomic.StoreUint32(&stop, 1)
		rr := <-done
		report(rr[1] == 0, "S3.loss_ctx["+v.name+"]", fmt.Sprintf("%d losses in %d rounds (%.1f%%), %d wrong values read", lost, rounds, 100*float64(lost)/rounds, rr[1]))
	}
}
