// S9: how far below RSP does Windows exception dispatch write, depending on the vector state
// the faulting thread has used (clean, ymm, zmm0-31 + k1-k7), and how does that compare with
// the headroom Go leaves below SP on a goroutine stack (stackGuard = 800 + stackSystem 4096 + 128)?
package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func doFault(mode, newsp, lo, pat, drop uintptr, res *[4]uintptr)
func vehAddr() uintptr
func stackLo() uintptr
func cpuid(eax, ecx uint32) (a, b, c, d uint32)
func xgetbv0() uint64

// Shared with the raw assembly handler.
var vehMarks [8]uint64
var vehArmed uintptr
var res [4]uintptr // global: must not live on the goroutine stack being filled

var modeName = []string{"clean", "ymm", "zmm"}

var (
	k32     = syscall.NewLazyDLL("kernel32.dll")
	pAddVEH = k32.NewProc("AddVectoredExceptionHandler")
	pVAlloc = k32.NewProc("VirtualAlloc")
)

type features struct {
	avx, avx512, ok512  bool
	xcr0                uint64
	brand               string
	xsaveSize, xsaveAll uint32
}

func detect() (f features) {
	_, _, c1, _ := cpuid(1, 0)
	f.avx = c1&(1<<28) != 0 && c1&(1<<27) != 0
	if f.avx {
		f.xcr0 = xgetbv0()
	}
	_, b7, _, _ := cpuid(7, 0)
	f.avx512 = b7&(1<<16) != 0
	f.ok512 = f.avx512 && f.xcr0&0xE6 == 0xE6
	_, bx, cx, _ := cpuid(0xD, 0)
	f.xsaveSize, f.xsaveAll = bx, cx
	var b []byte
	for l := uint32(0x80000002); l <= 0x80000004; l++ {
		a, bb, c, d := cpuid(l, 0)
		for _, v := range []uint32{a, bb, c, d} {
			b = append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
		}
	}
	f.brand = strings.TrimSpace(strings.Trim(string(b), "\x00"))
	return
}

func installVEH() {
	r, _, e := pAddVEH.Call(1, vehAddr())
	if r == 0 {
		panic(e)
	}
}

// measure runs doFault with both patterns, a few times; returns the max depth below SP (bytes)
// and SP-lo. clamped means the changed region reaches lo, so the true depth is at least depth.
func measure(mode, newsp, lo, drop uintptr) (depth, avail uintptr, clamped bool) {
	var minFirst uintptr
	for rep := 0; rep < 5; rep++ {
		for _, pat := range []uintptr{0xA5, 0x5A} {
			vehMarks[0] = 0
			if newsp == 0 {
				lo = stackLo() // the stack may have grown in this function's prologue
			}
			doFault(mode, newsp, lo, pat, drop, &res)
			if vehMarks[0] != 1 {
				fmt.Printf("INFO unexpected handler count %d\n", vehMarks[0])
			}
			if res[1] != 0 && (minFirst == 0 || res[1] < minFirst) {
				minFirst = res[1]
			}
		}
	}
	avail = res[0] - lo
	if minFirst == 0 {
		return 0, avail, false
	}
	return res[0] - minFirst, avail, minFirst == lo
}

func report(kind string, mode int, drop uintptr, depth, avail uintptr, clamped bool) {
	m := &vehMarks
	ge := ""
	if clamped {
		ge = " (at least: the changed region reaches the start of the filled area)"
	}
	fmt.Printf("RESULT %s mode=%s drop=%d: depth below RSP=%d%s, available below SP=%d, handlerSP=RSP-%d CONTEXT*=RSP-%d EXCEPTION_RECORD*=RSP-%d, CONTEXT_EX All={off %d len %d} XState={off %d len %d}\n",
		kind, modeName[mode], drop, depth, ge, avail, int64(res[0])-int64(m[2]), int64(res[0])-int64(m[3]), int64(res[0])-int64(m[4]),
		int32(m[6]), uint32(m[6]>>32), int32(m[7]), uint32(m[7]>>32))
}

// childBig: valid 1 MiB stack, so the whole dispatch fits; gives the true depth.
func childBig(mode int) {
	runtime.LockOSThread()
	installVEH()
	b, _, _ := pVAlloc.Call(0, 1<<20, 0x3000, 4)
	newsp := b + 0x80000
	depth, avail, clamped := measure(uintptr(mode), newsp, newsp-32768, 0)
	report("big", mode, 0, depth, avail, clamped)
}

const nearGuard = 4096 + 800 + 128 + 400 // just above stackguard0 = lo + 5024, with room for measure's frame

//go:noinline
func descend(mode int, drop uintptr, n int) uintptr {
	var pad [48]byte
	pad[n&31] = byte(n)
	lo := stackLo()
	sp := uintptr(unsafe.Pointer(&pad))
	if sp < lo || sp-lo > 1<<20 {
		fmt.Printf("INFO stackLo looks wrong: lo=%#x sp=%#x\n", lo, sp)
		os.Exit(3)
	}
	if sp-lo < nearGuard {
		depth, avail, clamped := measure(uintptr(mode), 0, lo, drop)
		report("goroutine-tight", mode, drop, depth, avail, clamped)
		return 0
	}
	r := descend(mode, drop, n+1)
	return r + uintptr(pad[(n+1)&31])
}

// childG: the faulting asm runs on a goroutine stack at the lowest SP Go's prologue check
// allows (just above lo+stackGuard), so SP-lo is about 5 KB; drop lowers SP further like a
// nosplit chain would.
func childG(mode int, drop uintptr) {
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		installVEH()
		descend(mode, drop, 0)
		close(done)
	}()
	select {
	case <-done:
		fmt.Println("survived")
	case <-time.After(20 * time.Second):
		fmt.Println("hang")
		os.Exit(4)
	}
}

func runChild(args ...string) {
	cmd := exec.Command(os.Args[0], args...)
	b, err := cmd.CombinedOutput()
	fmt.Printf("%s\n", strings.TrimSpace(string(b)))
	if code := cmd.ProcessState.ExitCode(); err != nil || code != 0 {
		fmt.Printf("CHILD %v exit %#x (%v)\n", args, uint32(code), err)
	}
}

func main() {
	if len(os.Args) >= 3 {
		m, _ := strconv.Atoi(os.Args[2])
		switch os.Args[1] {
		case "big":
			childBig(m)
			return
		case "gor":
			d, _ := strconv.Atoi(os.Args[3])
			childG(m, uintptr(d))
			return
		}
	}
	f := detect()
	fmt.Printf("INFO cpu=%q avx=%v avx512f=%v xcr0=%#x usable512=%v xsave size enabled=%d supported=%d GOMAXPROCS=%d go=%s\n",
		f.brand, f.avx, f.avx512, f.xcr0, f.ok512, f.xsaveSize, f.xsaveAll, runtime.GOMAXPROCS(0), runtime.Version())
	fmt.Printf("INFO Go stackGuard on windows/amd64 = 800 + 4096 (stackSystem) + 128 = 5024; stackSystem alone = 4096\n")
	supported := func(m int) bool { return !(m >= 1 && !f.avx) && !(m == 2 && !f.ok512) }
	for m := 0; m < 3; m++ {
		if !supported(m) {
			fmt.Printf("SKIP mode=%s: not supported by this CPU/OS\n", modeName[m])
			continue
		}
		runChild("big", strconv.Itoa(m))
	}
	for m := 0; m < 3; m++ {
		if !supported(m) {
			continue
		}
		for _, drop := range []int{0, 800} {
			runChild("gor", strconv.Itoa(m), strconv.Itoa(drop))
		}
	}
}
