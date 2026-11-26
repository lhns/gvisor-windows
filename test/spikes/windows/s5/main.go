package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

func rawcopy(dst, src, n uintptr)
func safecopy(dst, src, n uintptr) uintptr
func safecopyFault(dst, src, n uintptr) uintptr
func vehAddr() uintptr

var (
	k32     = syscall.NewLazyDLL("kernel32.dll")
	pAlloc  = k32.NewProc("VirtualAlloc")
	pAddVEH = k32.NewProc("AddVectoredExceptionHandler")
)

func report(ok bool, name, detail string) {
	s := "PASS"
	if !ok {
		s = "FAIL"
	}
	fmt.Printf("%s %s: %s\n", s, name, detail)
}

// try runs f with panic-on-fault; returns the recovered value (nil if f returned normally)
func try(f func()) (rec any) {
	defer func() { rec = recover() }()
	debug.SetPanicOnFault(true)
	f()
	return nil
}

func isFault(r any) bool {
	e, ok := r.(runtime.Error)
	return ok && (len(e.Error()) > 0)
}

func main() {
	bad, _, _ := pAlloc.Call(0, 0x10000, 0x3000, 0x01) // PAGE_NOACCESS
	good, _, _ := pAlloc.Call(0, 0x10000, 0x3000, 0x04)
	fmt.Printf("INFO NOACCESS page %#x, RW page %#x, GOMAXPROCS=%d\n", bad, good, runtime.GOMAXPROCS(0))
	if len(os.Args) > 1 && os.Args[1] == "nofault" {
		// control: shows what happens without SetPanicOnFault (process dies)
		_ = *(*byte)(unsafe.Pointer(bad))
		return
	}

	// 1. plain Go read
	var sink byte
	r := try(func() { sink = *(*byte)(unsafe.Pointer(bad)) })
	report(r != nil, "S5.go_read_fault_panicOnFault", fmt.Sprintf("recovered: %v", r))
	// 1b. plain Go write
	r = try(func() { *(*byte)(unsafe.Pointer(bad + 8)) = 1 })
	report(r != nil, "S5.go_write_fault_panicOnFault", fmt.Sprintf("recovered: %v", r))
	// 2. builtin copy (runtime memmove)
	dst := make([]byte, 4096)
	src := unsafe.Slice((*byte)(unsafe.Pointer(bad)), 4096)
	r = try(func() { copy(dst, src) })
	report(r != nil, "S5.builtin_copy_runtime_memmove_fault", fmt.Sprintf("recovered: %v", r))
	// 3. fault inside our own assembly routine, panicOnFault only
	r = try(func() { rawcopy(uintptr(unsafe.Pointer(&dst[0])), bad, 64) })
	report(r != nil, "S5.asm_rep_movsb_fault_panicOnFault", fmt.Sprintf("recovered: %v", r))
	// 3b. fault in write direction in asm
	r = try(func() { rawcopy(bad, uintptr(unsafe.Pointer(&dst[0])), 64) })
	report(r != nil, "S5.asm_write_fault_panicOnFault", fmt.Sprintf("recovered: %v", r))
	// 3c. fault in the middle of a long copy (second page bad)
	mid, _, _ := pAlloc.Call(0, 0x2000, 0x3000, 0x04)
	var old uint32
	k32.NewProc("VirtualProtect").Call(mid+0x1000, 0x1000, 0x01, uintptr(unsafe.Pointer(&old)))
	big := make([]byte, 0x2000)
	r = try(func() { rawcopy(uintptr(unsafe.Pointer(&big[0])), mid, 0x2000) })
	report(r != nil, "S5.asm_fault_mid_copy_panicOnFault", fmt.Sprintf("recovered: %v", r))

	// 4. repeated faults, timing, goroutines
	const N = 20000
	t0 := time.Now()
	okc := 0
	for i := 0; i < N; i++ {
		if try(func() { sink = *(*byte)(unsafe.Pointer(bad)) }) != nil {
			okc++
		}
	}
	d := time.Since(t0)
	report(okc == N, "S5.repeated_faults_panicOnFault", fmt.Sprintf("%d/%d recovered, %.1f us per fault+recover", okc, N, float64(d.Microseconds())/N))
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := 0
			for i := 0; i < 2000; i++ {
				if try(func() { rawcopy(uintptr(unsafe.Pointer(&dst[0])), bad, 64) }) != nil {
					c++
				}
			}
			mu.Lock()
			total += c
			mu.Unlock()
		}()
	}
	wg.Wait()
	report(total == 16000, "S5.concurrent_asm_faults_8_goroutines", fmt.Sprintf("%d/16000 recovered", total))
	// good path still fine
	for i := range dst {
		dst[i] = 0
	}
	rawcopy(good, uintptr(unsafe.Pointer(&dst[0])), 16)
	_ = sink

	// 5. raw VEH + RIP fixup (safecopy style), no panic, no Go runtime involvement
	h, _, _ := pAddVEH.Call(1, vehAddr())
	report(h != 0, "S5.install_asm_VEH", fmt.Sprintf("handle=%#x", h))
	ok1 := safecopy(uintptr(unsafe.Pointer(&dst[0])), good, 64)
	ok2 := safecopy(uintptr(unsafe.Pointer(&dst[0])), bad, 64)
	ok3 := safecopy(bad, uintptr(unsafe.Pointer(&dst[0])), 64)
	big2 := make([]byte, 0x2000)
	ok4 := safecopy(uintptr(unsafe.Pointer(&big2[0])), mid, 0x2000)
	report(ok1 == 1 && ok2 == 0 && ok3 == 0 && ok4 == 0, "S5.asm_VEH_RIP_fixup_safecopy", fmt.Sprintf("good=%d bad_read=%d bad_write=%d mid_copy_fault=%d (1=copied, 0=fault recovered)", ok1, ok2, ok3, ok4))
	t0 = time.Now()
	cnt := 0
	for i := 0; i < N; i++ {
		if safecopy(uintptr(unsafe.Pointer(&dst[0])), bad, 64) == 0 {
			cnt++
		}
	}
	report(cnt == N, "S5.asm_VEH_repeated", fmt.Sprintf("%d/%d recovered, %.1f us per fault", cnt, N, float64(time.Since(t0).Microseconds())/N))
	var wg2 sync.WaitGroup
	total = 0
	for g := 0; g < 8; g++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			c := 0
			for i := 0; i < 2000; i++ {
				if safecopy(uintptr(unsafe.Pointer(&dst[0])), bad, 64) == 0 {
					c++
				}
			}
			mu.Lock()
			total += c
			mu.Unlock()
		}()
	}
	wg2.Wait()
	report(total == 16000, "S5.asm_VEH_concurrent_8_goroutines", fmt.Sprintf("%d/16000 recovered", total))
	runtime.GC()
	report(true, "S5.runtime_healthy_after", "GC ran, process alive")
}
