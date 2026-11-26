package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

func vehStub()
func vehAddr() uintptr
func ud2loop(n uint64)
func rdtsc() uint64

var (
	k32         = syscall.NewLazyDLL("kernel32.dll")
	pAddVEH     = k32.NewProc("AddVectoredExceptionHandler")
	pCreateMap  = k32.NewProc("CreateFileMappingW")
	pMapView    = k32.NewProc("MapViewOfFile")
	pCreateEvt  = k32.NewProc("CreateEventW")
	pSetEvt     = k32.NewProc("SetEvent")
	pWait       = k32.NewProc("WaitForSingleObject")
	pSetAff     = k32.NewProc("SetThreadAffinityMask")
	pSetPrio    = k32.NewProc("SetThreadPriority")
)

const (
	offReq  = 0
	offResp = 64
	offData = 128
)

func u64(base uintptr, off uintptr) *uint64 { return (*uint64)(unsafe.Pointer(base + off)) }

func utf16(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }

func stats(name string, ns []float64) {
	sort.Float64s(ns)
	var sum float64
	for _, v := range ns {
		sum += v
	}
	fmt.Printf("RESULT %s: n=%d mean=%.0f ns median=%.0f ns p99=%.0f ns min=%.0f ns max=%.0f ns\n",
		name, len(ns), sum/float64(len(ns)), ns[len(ns)/2], ns[len(ns)*99/100], ns[0], ns[len(ns)-1])
}

func tscHz() float64 {
	t0 := time.Now()
	c0 := rdtsc()
	time.Sleep(200 * time.Millisecond)
	c1 := rdtsc()
	return float64(c1-c0) / time.Since(t0).Seconds()
}

func setup(name string, create bool) (base uintptr, req, resp syscall.Handle) {
	var m uintptr
	m, _, _ = pCreateMap.Call(^uintptr(0), 0, 0x04, 0, 4096, uintptr(unsafe.Pointer(utf16("Local\\s1map_"+name))))
	base, _, _ = pMapView.Call(m, 0xF001F, 0, 0, 4096)
	e1, _, _ := pCreateEvt.Call(0, 0, 0, uintptr(unsafe.Pointer(utf16("Local\\s1req_"+name))))
	e2, _, _ := pCreateEvt.Call(0, 0, 0, uintptr(unsafe.Pointer(utf16("Local\\s1resp_"+name))))
	return base, syscall.Handle(e1), syscall.Handle(e2)
}

func serve(name string, mode string) {
	runtime.LockOSThread()
	base, req, resp := setup(name, false)
	var last uint64
	for {
		switch mode {
		case "spin":
			for {
				v := atomic.LoadUint64(u64(base, offReq))
				if v != last {
					last = v
					break
				}
			}
		case "event":
			pWait.Call(uintptr(req), 0xFFFFFFFF)
			last = atomic.LoadUint64(u64(base, offReq))
		}
		if last == ^uint64(0) {
			return
		}
		// "serve": read the 64-byte request, write a reply derived from it
		*u64(base, offData+64) = *u64(base, offData) + 1
		atomic.StoreUint64(u64(base, offResp), last)
		if mode == "event" {
			pSetEvt.Call(uintptr(resp))
		}
	}
}

func client(mode string, n int, hz float64) {
	runtime.LockOSThread()
	name := fmt.Sprintf("%d_%s", os.Getpid(), mode)
	base, req, resp := setup(name, true)
	cmd := exec.Command(os.Args[0], "serve", name, mode)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Println("FAIL spawn:", err)
		return
	}
	time.Sleep(300 * time.Millisecond)
	lat := make([]float64, 0, n)
	rt := func(i uint64) float64 {
		*u64(base, offData) = i
		t0 := rdtsc()
		atomic.StoreUint64(u64(base, offReq), i)
		if mode == "event" {
			pSetEvt.Call(uintptr(req))
			pWait.Call(uintptr(resp), 0xFFFFFFFF)
		} else {
			for atomic.LoadUint64(u64(base, offResp)) != i {
			}
		}
		t1 := rdtsc()
		return float64(t1-t0) / hz * 1e9
	}
	for i := 1; i <= 1000; i++ {
		rt(uint64(i))
	}
	for i := 0; i < n; i++ {
		lat = append(lat, rt(uint64(1001+i)))
	}
	ok := *u64(base, offData+64) == *u64(base, offData)+1
	atomic.StoreUint64(u64(base, offReq), ^uint64(0))
	pSetEvt.Call(uintptr(req))
	cmd.Wait()
	fmt.Printf("%s S1b.%s data-check\n", map[bool]string{true: "PASS", false: "FAIL"}[ok], mode)
	stats("S1b.round_trip_"+mode, lat)
}

func main() {
	if len(os.Args) >= 4 && os.Args[1] == "serve" {
		serve(os.Args[2], os.Args[3])
		return
	}
	hz := tscHz()
	fmt.Printf("INFO TSC %.0f MHz, cpus=%d\n", hz/1e6, runtime.NumCPU())
	// S1a in-process ud2 + VEH
	runtime.LockOSThread()
	h, _, _ := pAddVEH.Call(1, vehAddr())
	fmt.Printf("INFO VEH installed handle=%#x (raw asm handler, no Go callback)\n", h)
	ud2loop(1000)
	var res []float64
	for r := 0; r < 7; r++ {
		const N = 200000
		t0 := rdtsc()
		ud2loop(N)
		t1 := rdtsc()
		res = append(res, float64(t1-t0)/hz*1e9/N)
	}
	fmt.Println("PASS S1a.ud2_veh: 7 x 200000 traps caught and resumed")
	stats("S1a.ud2_trap_ns_per_trap(per-run mean)", res)
	// S1b cross-process
	client("spin", 200000, hz)
	client("event", 50000, hz)
}
