// S7: where does Windows exception dispatch put its data, and what does that do to a
// guest that keeps live data in the red zone or runs on an unusable stack?
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func trap(kind, rsp uintptr)
func vehAddr() uintptr

// Read by the raw assembly handler.
var vehMark, vehSkip, vehArmed uintptr

var (
	k32     = syscall.NewLazyDLL("kernel32.dll")
	adv     = syscall.NewLazyDLL("advapi32.dll")
	ntdll   = syscall.NewLazyDLL("ntdll.dll")
	pAddVEH = k32.NewProc("AddVectoredExceptionHandler")
	pVAlloc = k32.NewProc("VirtualAlloc")
	pCreMap = k32.NewProc("CreateFileMappingW")
	pMapVw  = k32.NewProc("MapViewOfFile")
	pSetEM  = k32.NewProc("SetErrorMode")
	pWaitDE = k32.NewProc("WaitForDebugEvent")
	pContDE = k32.NewProc("ContinueDebugEvent")
	pOpenTh = k32.NewProc("OpenThread")
	pGetCtx = k32.NewProc("GetThreadContext")
	pSetCtx = k32.NewProc("SetThreadContext")
	pTermP  = k32.NewProc("TerminateProcess")
	pQPC    = k32.NewProc("QueryPerformanceCounter")
	pQPF    = k32.NewProc("QueryPerformanceFrequency")

	pCreateRestricted    = adv.NewProc("CreateRestrictedToken")
	pSetTokenInfo        = adv.NewProc("SetTokenInformation")
	pStrToSid            = adv.NewProc("ConvertStringSidToSidW")
	pCreateProcessAsUser = adv.NewProc("CreateProcessAsUserW")
	pRtlGetVersion       = ntdll.NewProc("RtlGetVersion")
)

const (
	kUD2, kLoad, kInt3, kDiv0, kStore = 0, 1, 2, 3, 4
	rzSpan                            = 16384
)

var kindName = map[int]string{kUD2: "ud2", kLoad: "load", kInt3: "int3", kDiv0: "div0", kStore: "store"}
var kindSkip = map[int]uintptr{kUD2: 2, kLoad: 3, kInt3: 1, kDiv0: 3, kStore: 5}
var kindCode = map[int]uint32{kUD2: 0xC000001D, kLoad: 0xC0000005, kInt3: 0x80000003, kDiv0: 0xC0000094, kStore: 0xC0000005}

func kindByName(s string) int {
	for k, n := range kindName {
		if n == s {
			return k
		}
	}
	panic("kind " + s)
}

func utf16p(s string) uintptr {
	p, _ := syscall.UTF16PtrFromString(s)
	return uintptr(unsafe.Pointer(p))
}

func valloc(addr, size, typ, prot uintptr) uintptr {
	r, _, _ := pVAlloc.Call(addr, size, typ, prot)
	return r
}

func marks(name string) *[8]uint64 {
	m, _, _ := pCreMap.Call(^uintptr(0), 0, 4, 0, 4096, utf16p("Local\\s7m_"+name))
	v, _, _ := pMapVw.Call(m, 0xF001F, 0, 0, 4096)
	return (*[8]uint64)(unsafe.Pointer(v))
}

func line(ok bool, name, detail string) {
	s := "PASS"
	if !ok {
		s = "FAIL"
	}
	fmt.Printf("%s %s: %s\n", s, name, detail)
}

// mkStack builds a guest-like stack. Returns the RSP to switch to and whether memory around
// it is fully mapped read/write (so the sentinel can be checked).
func mkStack(variant string, align uintptr) (rsp uintptr, valid bool) {
	switch {
	case variant == "valid":
		b := valloc(0, 1<<20, 0x3000, 4)
		return b + 0x80000 + align, true
	case variant == "noaccess": // committed PAGE_NOACCESS
		b := valloc(0, 65536, 0x3000, 1)
		return b + 0x8000, false
	case variant == "reserved": // MEM_RESERVE only
		b := valloc(0, 65536, 0x2000, 1)
		return b + 0x8000, false
	case variant == "zero":
		return 0, false
	case variant == "kernel":
		return 0xFFFF800000001000, false
	case strings.HasPrefix(variant, "tiny"): // N bytes mapped below RSP, nothing mapped below that
		n, _ := strconv.Atoi(variant[4:])
		base := valloc(0, 1<<18, 0x2000, 1)
		b := base + 0x20000
		size := uintptr((n+0xFFF)&^0xFFF) + 0x1000
		if valloc(b, size, 0x1000, 4) == 0 {
			panic("commit")
		}
		return b + uintptr(n), false
	case variant == "guard256": // 256 bytes below RSP, then a PAGE_GUARD page
		base := valloc(0, 1<<18, 0x2000, 1)
		b := base + 0x20000
		valloc(b-0x1000, 0x1000, 0x1000, 4|0x100)
		valloc(b, 0x2000, 0x1000, 4)
		return b + 256, false
	}
	panic("variant " + variant)
}

// pat is never 0; pass 1 uses the complement so a coincidental equal write shows in one pass.
func pat(i, pass int) byte {
	b := byte(0x80 | (i*37)&0x7F)
	if pass == 1 {
		b = ^b
	}
	return b
}

// rz: case 1 and case 4 (handler stack location), in-process VEH, valid stack.
func childRZ(name string) {
	mk := marks(name)
	vehMark = uintptr(unsafe.Pointer(mk))
	pAddVEH.Call(1, vehAddr())
	for _, k := range []int{kUD2, kLoad, kDiv0, kInt3} {
		for _, align := range []uintptr{0, 8} {
			rsp, _ := mkStack("valid", align)
			vehSkip = kindSkip[k]
			changed := make([]bool, rzSpan+1) // index d = byte at rsp-d
			above := 0
			for pass := 0; pass < 2; pass++ {
				for d := 1; d <= rzSpan; d++ {
					*(*byte)(unsafe.Pointer(rsp - uintptr(d))) = pat(d, pass)
				}
				for d := 0; d < 256; d++ {
					*(*byte)(unsafe.Pointer(rsp + uintptr(d))) = pat(d+rzSpan, pass)
				}
				mk[0] = 0
				vehArmed = 1
				trap(uintptr(k), rsp)
				vehArmed = 0
				for d := 1; d <= rzSpan; d++ {
					if *(*byte)(unsafe.Pointer(rsp - uintptr(d))) != pat(d, pass) {
						changed[d] = true
					}
				}
				for d := 0; d < 256; d++ {
					if *(*byte)(unsafe.Pointer(rsp + uintptr(d))) != pat(d+rzSpan, pass) {
						above++
					}
				}
			}
			dmin, dmax, c128, c512, ctot := 0, 0, 0, 0, 0
			for d := 1; d <= rzSpan; d++ {
				if changed[d] {
					if dmin == 0 {
						dmin = d
					}
					dmax = d
					ctot++
					if d <= 128 {
						c128++
					}
					if d <= 512 {
						c512++
					}
				}
			}
			r := uint64(rsp)
			line(c128 == 0, fmt.Sprintf("S7.1.redzone.%s.rsp%%16=%d", kindName[k], align),
				fmt.Sprintf("clobbered in 128B below RSP=%d, in 512B=%d, in %dB=%d, nearest changed byte=RSP-%d, farthest=RSP-%d, written above RSP=%d; vehRuns=%d code=%#x",
					c128, c512, rzSpan, ctot, dmin, dmax, above, mk[0], mk[1]))
			fmt.Printf("INFO S7.4.%s.rsp%%16=%d: handler SP at entry = RSP-%d, CONTEXT* = RSP-%d, EXCEPTION_RECORD* = RSP-%d (handler ran on guest stack: %v)\n",
				kindName[k], align, int64(r-mk[2]), int64(r-mk[3]), int64(r-mk[4]), mk[2] < r && mk[2] > r-rzSpan)
		}
	}
}

// stk: case 2, one variant+kind with the in-process VEH; may die.
func childStk(name, variant, kindS string) {
	pSetEM.Call(0x8003)
	mk := marks(name)
	vehMark = uintptr(unsafe.Pointer(mk))
	pAddVEH.Call(1, vehAddr())
	k := kindByName(kindS)
	rsp, _ := mkStack(variant, 0)
	vehSkip = kindSkip[k]
	vehArmed = 1
	trap(uintptr(k), rsp)
	vehArmed = 0
	fmt.Println("survived")
}

// dbg: child of the debugger (case 3). No VEH. Exit code 1000+clobbered bytes (of 512 below RSP).
func childDbg(variant, kindS string, n int) {
	k := kindByName(kindS)
	rsp, valid := mkStack(variant, 0)
	if valid {
		for d := 1; d <= 512; d++ {
			*(*byte)(unsafe.Pointer(rsp - uintptr(d))) = pat(d, 0)
		}
	}
	for i := 0; i < n; i++ {
		trap(uintptr(k), rsp)
	}
	c := 0
	if valid {
		for d := 1; d <= 512; d++ {
			if *(*byte)(unsafe.Pointer(rsp - uintptr(d))) != pat(d, 0) {
				c++
			}
		}
	}
	os.Exit(1000 + c)
}

func runChild(timeout time.Duration, args ...string) (out string, code int, mk [8]uint64) {
	name := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	pm := marks(name)
	c, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(c, os.Args[0], append(args, name)...)
	b, err := cmd.CombinedOutput()
	code = cmd.ProcessState.ExitCode()
	if err != nil && c.Err() != nil {
		code = -1
	}
	return strings.TrimSpace(string(b)), code, *pm
}

func ntstatus(code int) string {
	if code < 0 {
		return "TIMEOUT"
	}
	return fmt.Sprintf("%#x", uint32(code))
}

func case1and4() {
	fmt.Println("===== S7.1 red zone and S7.4 handler stack (in-process VEH, valid stack)")
	out, code, _ := runChild(60*time.Second, "rz")
	fmt.Println(out)
	if code != 0 {
		line(false, "S7.1 child", "exit "+ntstatus(code))
	}
}

func case2() {
	fmt.Println("===== S7.2 unusable stack (in-process VEH; each case in its own child process)")
	variants := []string{"valid", "noaccess", "reserved", "zero", "kernel", "tiny0", "tiny256", "tiny512", "tiny1024",
		"tiny2048", "tiny3072", "tiny4096", "tiny6144", "tiny8192", "guard256"}
	for _, v := range variants {
		for _, k := range []string{"ud2", "load", "store"} {
			if k == "store" && v != "tiny0" && v != "zero" {
				continue
			}
			out, code, mk := runChild(30*time.Second, "stk", v, k)
			survived := strings.Contains(out, "survived")
			where := ", handler did NOT run"
			if mk[0] > 0 {
				where = fmt.Sprintf(", handler ran (code=%#x)", mk[1])
			}
			ex := ""
			if !survived {
				ex = ", process died with exit " + ntstatus(code)
				if len(out) > 0 {
					ex += fmt.Sprintf(" out=%q", firstLine(out))
				}
			}
			line(survived && mk[0] > 0, fmt.Sprintf("S7.2.%s.%s", v, k), fmt.Sprintf("survived=%v%s%s", survived, where, ex))
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

// ---- case 3: debugger path ----

func sidOf(s string) uintptr {
	var p uintptr
	r, _, e := pStrToSid.Call(utf16p(s), uintptr(unsafe.Pointer(&p)))
	if r == 0 {
		panic("sid " + s + e.Error())
	}
	return p
}

type sidAndAttr struct {
	Sid  uintptr
	Attr uint32
	_    uint32
}

// restrictedToken: as s4 V1: DISABLE_MAX_PRIVILEGE, deny-only Administrators, low integrity.
func restrictedToken() (uintptr, error) {
	cur, _ := syscall.GetCurrentProcess()
	var t syscall.Token
	if err := syscall.OpenProcessToken(cur, 0x2|0x8|0x80|0x20|0x1, &t); err != nil {
		return 0, err
	}
	deny := []sidAndAttr{{Sid: sidOf("S-1-5-32-544")}}
	var nt uintptr
	r, _, e := pCreateRestricted.Call(uintptr(t), 1, 1, uintptr(unsafe.Pointer(&deny[0])), 0, 0, 0, 0, uintptr(unsafe.Pointer(&nt)))
	if r == 0 {
		return 0, fmt.Errorf("CreateRestrictedToken %v", e)
	}
	tml := sidAndAttr{Sid: sidOf("S-1-16-4096"), Attr: 0x20}
	r, _, e = pSetTokenInfo.Call(nt, 25, uintptr(unsafe.Pointer(&tml)), unsafe.Sizeof(tml)+8)
	if r == 0 {
		return 0, fmt.Errorf("SetTokenInformation(IL) %v", e)
	}
	return nt, nil
}

type dbgResult struct {
	err      string
	exit     uint32
	events   int
	deltas   []float64 // ns between consecutive exception events
	getset   []float64 // ns spent in Get+SetThreadContext per event
	firstRsp uint64
}

func debugRun(restricted bool, variant, kind string, n int) (res dbgResult) {
	runtime.LockOSThread() // debug events go to the thread that created the process
	defer runtime.UnlockOSThread()
	k := kindByName(kind)
	exe, _ := os.Executable()
	cl, _ := syscall.UTF16PtrFromString(fmt.Sprintf(`"%s" dbg %s %s %d x`, exe, variant, kind, n))
	var si syscall.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi syscall.ProcessInformation
	flags := uint32(0x2 | 0x08000000) // DEBUG_ONLY_THIS_PROCESS | CREATE_NO_WINDOW
	if restricted {
		tok, err := restrictedToken()
		if err != nil {
			res.err = err.Error()
			return
		}
		r, _, e := pCreateProcessAsUser.Call(tok, 0, uintptr(unsafe.Pointer(cl)), 0, 0, 0, uintptr(flags), 0, 0,
			uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&pi)))
		if r == 0 {
			res.err = "CreateProcessAsUser " + e.Error()
			return
		}
	} else if err := syscall.CreateProcess(nil, cl, nil, nil, false, flags, nil, nil, &si, &pi); err != nil {
		res.err = "CreateProcess " + err.Error()
		return
	}
	defer syscall.CloseHandle(pi.Process)
	defer syscall.CloseHandle(pi.Thread)

	ctx := valloc(0, 4096, 0x3000, 4) // 16-byte aligned CONTEXT buffer
	var ev [64]uint64
	evp := uintptr(unsafe.Pointer(&ev[0]))
	threads := map[uint32]uintptr{}
	seenBP := false
	var last float64
	for {
		r, _, _ := pWaitDE.Call(evp, 30000)
		now := qpcNs()
		if r == 0 {
			res.err = "WaitForDebugEvent failed/timeout"
			pTermP.Call(uintptr(pi.Process), 0xDEAD)
			return
		}
		code := *(*uint32)(unsafe.Pointer(evp))
		pid := *(*uint32)(unsafe.Pointer(evp + 4))
		tid := *(*uint32)(unsafe.Pointer(evp + 8))
		status := uintptr(0x00010002) // DBG_CONTINUE
		switch code {
		case 5: // EXIT_PROCESS
			res.exit = *(*uint32)(unsafe.Pointer(evp + 16))
			pContDE.Call(uintptr(pid), uintptr(tid), status)
			return
		case 1: // EXCEPTION
			ecode := *(*uint32)(unsafe.Pointer(evp + 16))
			switch {
			case ecode == 0x80000003 && !seenBP:
				seenBP = true // loader breakpoint
			case ecode == kindCode[k] && seenBP:
				h := threads[tid]
				if h == 0 {
					h, _, _ = pOpenTh.Call(0x58, 0, uintptr(tid))
					threads[tid] = h
				}
				t0 := qpcNs()
				*(*uint32)(unsafe.Pointer(ctx + 0x30)) = 0x100001 // CONTEXT_CONTROL
				pGetCtx.Call(h, ctx)
				if res.events == 0 {
					res.firstRsp = *(*uint64)(unsafe.Pointer(ctx + 0x98))
				}
				*(*uint64)(unsafe.Pointer(ctx + 0xF8)) += uint64(kindSkip[k])
				pSetCtx.Call(h, ctx)
				res.getset = append(res.getset, qpcNs()-t0)
				if res.events > 0 {
					res.deltas = append(res.deltas, now-last)
				}
				last = now
				res.events++
			default:
				status = 0x80010001 // DBG_EXCEPTION_NOT_HANDLED
			}
		}
		pContDE.Call(uintptr(pid), uintptr(tid), status)
	}
}

func med(v []float64) (m, p99 float64) {
	if len(v) == 0 {
		return
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2], s[len(s)*99/100]
}

func case3() {
	fmt.Println("===== S7.3 debugger path (parent = debugger, DEBUG_ONLY_THIS_PROCESS, child has no VEH)")
	tok, err := restrictedToken()
	fmt.Printf("INFO restricted token (DISABLE_MAX_PRIVILEGE, deny Administrators, low IL): err=%v ok=%v\n", err, tok != 0)
	for _, restricted := range []bool{false, true} {
		tag := map[bool]string{false: "normal", true: "lowIL"}[restricted]
		for _, v := range []string{"valid", "noaccess", "reserved", "zero", "kernel", "tiny0", "tiny256"} {
			for _, k := range []string{"ud2", "load"} {
				name := fmt.Sprintf("S7.3.%s.%s.%s", tag, v, k)
				r := debugRun(restricted, v, k, 1)
				switch {
				case r.err != "":
					line(false, name, "error: "+r.err)
				case r.exit >= 1000 && r.exit <= 1512:
					c := int(r.exit) - 1000
					d := fmt.Sprintf("child resumed after %d event(s), exit %d", r.events, r.exit)
					if v == "valid" {
						d += fmt.Sprintf(", bytes clobbered in 512B below RSP = %d", c)
					}
					d += fmt.Sprintf(", RSP in event context=%#x", r.firstRsp)
					line(r.events == 1 && c == 0, name, d)
				default:
					line(false, name, fmt.Sprintf("child died, exit %#x, events=%d", r.exit, r.events))
				}
			}
		}
		const N = 20000
		r := debugRun(restricted, "valid", "ud2", N)
		if r.err != "" || len(r.deltas) < 1000 {
			line(false, "S7.3.cost."+tag, fmt.Sprintf("err=%q events=%d exit=%#x", r.err, r.events, r.exit))
			continue
		}
		d := r.deltas[500:]
		m, p := med(d)
		gm, gp := med(r.getset)
		var sum float64
		for _, x := range d {
			sum += x
		}
		line(true, "S7.3.cost."+tag, fmt.Sprintf("n=%d traps, round trip mean=%.0f ns median=%.0f ns p99=%.0f ns (Get+SetThreadContext median=%.0f ns p99=%.0f ns); exit=%d",
			len(d), sum/float64(len(d)), m, p, gm, gp, r.exit))
	}
}

func buildInfo() {
	var v [72]uint32 // OSVERSIONINFOEXW is 284 bytes
	v[0] = 284
	pRtlGetVersion.Call(uintptr(unsafe.Pointer(&v[0])))
	fmt.Printf("INFO Windows build: %d.%d.%d, GOARCH=%s, cpus=%d\n", v[1], v[2], v[3], runtime.GOARCH, runtime.NumCPU())
}

func main() {
	if len(os.Args) >= 2 {
		a := os.Args
		switch a[1] {
		case "rz":
			childRZ(a[len(a)-1])
			return
		case "stk":
			childStk(a[len(a)-1], a[2], a[3])
			return
		case "dbg":
			n, _ := strconv.Atoi(a[4])
			childDbg(a[2], a[3], n)
			return
		}
	}
	buildInfo()
	case1and4()
	case2()
	case3()
}

var qpf float64

// qpcNs reads the performance counter in ns (time.Now is too coarse on some hosts).
func qpcNs() float64 {
	var c, f int64
	if qpf == 0 {
		pQPF.Call(uintptr(unsafe.Pointer(&f)))
		qpf = float64(f)
	}
	pQPC.Call(uintptr(unsafe.Pointer(&c)))
	return float64(c) / qpf * 1e9
}
