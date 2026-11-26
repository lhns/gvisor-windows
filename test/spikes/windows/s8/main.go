// S8: what a debugger-per-stub costs: faults fixed from outside, event counts, concurrency,
// pass-through to in-process handlers, and %fs repair via a trampoline.
package main

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

func trap(kind, rsp uintptr)
func vehAddr() uintptr
func marker()
func thrEntryAddr() uintptr
func cpuid7() uint32
func fsloop(n, base, magic uintptr) uintptr

var vehMark, vehSkip, vehArmed uintptr

var (
	k32      = syscall.NewLazyDLL("kernel32.dll")
	kb       = syscall.NewLazyDLL("kernelbase.dll")
	adv      = syscall.NewLazyDLL("advapi32.dll")
	ntdll    = syscall.NewLazyDLL("ntdll.dll")
	pAddVEH  = k32.NewProc("AddVectoredExceptionHandler")
	pVAlloc  = k32.NewProc("VirtualAlloc")
	pVProt   = k32.NewProc("VirtualProtect")
	pVAllEx  = k32.NewProc("VirtualAllocEx")
	pVProEx  = k32.NewProc("VirtualProtectEx")
	pVFreEx  = k32.NewProc("VirtualFreeEx")
	pWPM     = k32.NewProc("WriteProcessMemory")
	pCreMap  = k32.NewProc("CreateFileMappingW")
	pWaitDE  = k32.NewProc("WaitForDebugEvent")
	pContDE  = k32.NewProc("ContinueDebugEvent")
	pOpenTh  = k32.NewProc("OpenThread")
	pGetCtx  = k32.NewProc("GetThreadContext")
	pSetCtx  = k32.NewProc("SetThreadContext")
	pTermP   = k32.NewProc("TerminateProcess")
	pQPC     = k32.NewProc("QueryPerformanceCounter")
	pQPF     = k32.NewProc("QueryPerformanceFrequency")
	pCreThr  = k32.NewProc("CreateThread")
	pWaitO   = k32.NewProc("WaitForSingleObject")
	pMapV3   = kb.NewProc("MapViewOfFile3")
	pVAlloc2 = kb.NewProc("VirtualAlloc2")

	pCreateRestricted    = adv.NewProc("CreateRestrictedToken")
	pSetTokenInfo        = adv.NewProc("SetTokenInformation")
	pStrToSid            = adv.NewProc("ConvertStringSidToSidW")
	pCreateProcessAsUser = adv.NewProc("CreateProcessAsUserW")
	pRtlGetVersion       = ntdll.NewProc("RtlGetVersion")
)

const (
	dbgContinue   = 0x00010002
	dbgNotHandled = 0x80010001
	fsT           = 0x2A0000000 // per-thread TLS block, allocated in the child by the debugger
	fsTramp       = 0x2A0010000 // trampoline page
	fsMagic       = 0x1122334455667788
)

var evNames = []string{"?", "EXCEPTION", "CREATE_THREAD", "CREATE_PROCESS", "EXIT_THREAD", "EXIT_PROCESS", "LOAD_DLL", "UNLOAD_DLL", "OUTPUT_DEBUG_STRING", "RIP"}

var qpf float64

func qpcNs() float64 {
	var c, f int64
	if qpf == 0 {
		pQPF.Call(uintptr(unsafe.Pointer(&f)))
		qpf = float64(f)
	}
	pQPC.Call(uintptr(unsafe.Pointer(&c)))
	return float64(c) / qpf * 1e9
}

func utf16p(s string) uintptr {
	p, _ := syscall.UTF16PtrFromString(s)
	return uintptr(unsafe.Pointer(p))
}

func valloc(addr, size, typ, prot uintptr) uintptr {
	r, _, _ := pVAlloc.Call(addr, size, typ, prot)
	return r
}

func line(ok bool, name, detail string) {
	s := "PASS"
	if !ok {
		s = "FAIL"
	}
	fmt.Printf("%s %s: %s\n", s, name, detail)
}

func stat(v []float64) (med, p99, mean float64) {
	if len(v) == 0 {
		return
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	var sum float64
	for _, x := range s {
		sum += x
	}
	return s[len(s)/2], s[len(s)*99/100], sum / float64(len(s))
}

// ---------------- children ----------------

// fault: touch K PAGE_NOACCESS pages R times (re-protecting between rounds), or in "map"
// mode touch K pages of a placeholder region once.
func childFault(mode string, k, r int) {
	var base uintptr
	if mode == "map" {
		base, _, _ = pVAlloc2.Call(0, 0, uintptr(k)*4096, 0x2000|0x40000, 1, 0, 0)
	} else {
		base = valloc(0, uintptr(k)*4096, 0x3000, 1)
	}
	if base == 0 {
		os.Exit(2)
	}
	for round := 0; round < r; round++ {
		for i := 0; i < k; i++ {
			*(*byte)(unsafe.Pointer(base + uintptr(i)*4096)) = 1
		}
		if mode != "map" {
			var old uint32
			pVProt.Call(base, uintptr(k)*4096, 1, uintptr(unsafe.Pointer(&old)))
		}
	}
	os.Exit(1000)
}

// pass: ud2 on a guest stack with an in-process VEH installed.
func childPass(stack string) {
	mk := valloc(0, 4096, 0x3000, 4)
	vehMark = mk
	pAddVEH.Call(1, vehAddr())
	var rsp uintptr
	if stack == "valid" {
		rsp = valloc(0, 1<<20, 0x3000, 4) + 0x80000
	} else {
		rsp = valloc(0, 1<<16, 0x3000, 1) + 0x8000 // PAGE_NOACCESS
	}
	vehSkip = 2
	vehArmed = 1
	trap(0, rsp)
	vehArmed = 0
	m := (*[8]uint64)(unsafe.Pointer(mk))
	if m[0] == 0 {
		os.Exit(1000)
	}
	d := int64(rsp) - int64(m[2]) + 30000 // handler SP below guest RSP, offset so it is positive
	if d < 0 || d > 60000 {
		d = 0
	}
	os.Exit(100000 + int(d))
}

func childEvt() {
	marker() // phase 1 -> 2
	h, _, _ := pCreThr.Call(0, 0, thrEntryAddr(), 0, 0, 0)
	pWaitO.Call(h, 0xFFFFFFFF)
	marker() // 2 -> 3
	os.Exit(1000)
}

func childFS(n int) {
	if cpuid7()&1 == 0 {
		os.Exit(7)
	}
	bad := fsloop(uintptr(n), fsT, fsMagic)
	if bad > 500 {
		bad = 500
	}
	os.Exit(1000 + int(bad))
}

// ---------------- debugger ----------------

type sidAndAttr struct {
	Sid  uintptr
	Attr uint32
	_    uint32
}

func sidOf(s string) uintptr {
	var p uintptr
	r, _, e := pStrToSid.Call(utf16p(s), uintptr(unsafe.Pointer(&p)))
	if r == 0 {
		panic("sid " + s + e.Error())
	}
	return p
}

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

type proc struct {
	pid         uint32
	h           uintptr
	seenBP      bool
	phase       int
	done        bool
	exit        uint32
	faults      int
	first       float64
	lastT       float64
	last        float64
	deltas, fix []float64
	counts      [10]int
	phaseCounts [4][10]int
	fcFirst     int
	fcSecond    int
	err         string
}

// spawn creates a debuggee of the calling thread (debug events go to the creating thread).
func spawn(restricted bool, cmdline string) (*proc, error) {
	cl, _ := syscall.UTF16PtrFromString(cmdline)
	var si syscall.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi syscall.ProcessInformation
	flags := uint32(0x2 | 0x08000000) // DEBUG_ONLY_THIS_PROCESS | CREATE_NO_WINDOW
	if restricted {
		tok, err := restrictedToken()
		if err != nil {
			return nil, err
		}
		r, _, e := pCreateProcessAsUser.Call(tok, 0, uintptr(unsafe.Pointer(cl)), 0, 0, 0, uintptr(flags), 0, 0,
			uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&pi)))
		if r == 0 {
			return nil, fmt.Errorf("CreateProcessAsUser %v", e)
		}
	} else if err := syscall.CreateProcess(nil, cl, nil, nil, false, flags, nil, nil, &si, &pi); err != nil {
		return nil, err
	}
	syscall.CloseHandle(pi.Thread)
	return &proc{pid: pi.ProcessId, h: uintptr(pi.Process)}, nil
}

func self(args string) string {
	exe, _ := os.Executable()
	return fmt.Sprintf(`"%s" %s`, exe, args)
}

type session struct {
	variant string
	section uintptr
	ctx     uintptr
	ths     map[uint32]uintptr
	tramp   uintptr
}

func (s *session) thread(tid uint32) uintptr {
	h := s.ths[tid]
	if h == 0 {
		h, _, _ = pOpenTh.Call(0x58, 0, uintptr(tid))
		s.ths[tid] = h
	}
	return h
}

func (s *session) get(tid uint32, flags uint32) {
	*(*uint32)(unsafe.Pointer(s.ctx + 0x30)) = flags
	pGetCtx.Call(s.thread(tid), s.ctx)
}

func (s *session) set(tid uint32) { pSetCtx.Call(s.thread(tid), s.ctx) }

func (s *session) rip() *uint64 { return (*uint64)(unsafe.Pointer(s.ctx + 0xF8)) }

func wpm(h, addr uintptr, b []byte) {
	var n uintptr
	pWPM.Call(h, addr, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(unsafe.Pointer(&n)))
}

func u64b(v uint64) []byte {
	b := make([]byte, 8)
	for i := range b {
		b[i] = byte(v >> (8 * i))
	}
	return b
}

// setupFS runs at the loader breakpoint: TLS block, trampoline and its slots in the child.
//
//	slot S = tramp+0x100: [0]=fs base, [8]=scratch, [16]=return RIP
//	code at tramp+0x200: mov [S+8],rax; mov rax,[S]; wrfsbase rax; mov rax,[S+8]; jmp [S+16]
func (s *session) setupFS(p *proc) {
	t, _, _ := pVAllEx.Call(p.h, fsT, 0x10000, 0x3000, 4)
	tp, _, _ := pVAllEx.Call(p.h, fsTramp, 0x1000, 0x3000, 0x40)
	if t != fsT || tp != fsTramp {
		p.err = fmt.Sprintf("VirtualAllocEx fixed addresses failed (%#x %#x)", t, tp)
		return
	}
	wpm(p.h, fsT, u64b(fsMagic))
	S := uint64(fsTramp + 0x100)
	wpm(p.h, uintptr(S), u64b(fsT))
	var code []byte
	code = append(code, 0x48, 0xA3)
	code = append(code, u64b(S+8)...)
	code = append(code, 0x48, 0xA1)
	code = append(code, u64b(S)...)
	code = append(code, 0xF3, 0x48, 0x0F, 0xAE, 0xD0)
	code = append(code, 0x48, 0xA1)
	code = append(code, u64b(S+8)...)
	next := uint64(fsTramp+0x200) + uint64(len(code)) + 6
	rel := uint32(int32(int64(S+16) - int64(next)))
	code = append(code, 0xFF, 0x25, byte(rel), byte(rel>>8), byte(rel>>16), byte(rel>>24))
	wpm(p.h, fsTramp+0x200, code)
}

var ctxFlags = map[string]uint32{"ctl": 0x100001, "int": 0x100002, "ctlint": 0x100003, "full": 0x10000B, "ctlset": 0x100001}

// onException decides the status for one exception event.
func (s *session) onException(p *proc, tid uint32, evp uintptr, now float64) uintptr {
	ecode := *(*uint32)(unsafe.Pointer(evp + 16))
	first := *(*uint32)(unsafe.Pointer(evp + 168))
	if first != 0 {
		p.fcFirst++
	} else {
		p.fcSecond++
	}
	if ecode == 0x80000003 && !p.seenBP {
		p.seenBP = true
		p.phase = 1
		if s.variant == "fs" {
			s.setupFS(p)
		}
		return dbgContinue
	}
	v := s.variant
	switch {
	case v == "pass-handled" || v == "evt":
		if ecode == 0xC000001D {
			s.get(tid, 0x100001)
			*s.rip() += 2
			s.set(tid)
			if v == "evt" && p.phase < 3 {
				p.phase++
			}
			return dbgContinue
		}
	case v == "pass-nothandled":
		return dbgNotHandled
	case v == "fs":
		if ecode == 0xC0000005 {
			t0 := qpcNs()
			s.get(tid, 0x100001)
			rip := *s.rip()
			wpm(p.h, fsTramp+0x110, u64b(rip))
			*s.rip() = fsTramp + 0x200
			s.set(tid)
			s.record(p, now, qpcNs()-t0)
			return dbgContinue
		}
	case ecode == 0xC0000005 && !strings.HasPrefix(v, "pass") && v != "evt" && v != "native":
		addr := uintptr(*(*uint64)(unsafe.Pointer(evp + 56)))
		page := addr &^ 0xFFF
		t0 := qpcNs()
		base, extra, _ := strings.Cut(v, "+")
		ok := true
		if base == "map" {
			pVFreEx.Call(p.h, page, 4096, 0x8000|0x2)
			r, _, e := pMapV3.Call(s.section, p.h, page, 0, 4096, 0x4000, 4, 0, 0)
			if r == 0 {
				ok = false
				p.err = "MapViewOfFile3 " + e.Error()
			}
		} else {
			var old uint32
			r, _, e := pVProEx.Call(p.h, page, 4096, 4, uintptr(unsafe.Pointer(&old)))
			if r == 0 {
				ok = false
				p.err = "VirtualProtectEx " + e.Error()
			}
		}
		if extra != "" {
			s.get(tid, ctxFlags[extra])
			if extra == "ctlset" {
				s.set(tid)
			}
		}
		if !ok {
			return dbgNotHandled
		}
		s.record(p, now, qpcNs()-t0)
		return dbgContinue
	}
	return dbgNotHandled
}

func (s *session) record(p *proc, now, fix float64) {
	if p.faults == 0 {
		p.first = now
	} else {
		p.deltas = append(p.deltas, now-p.last)
	}
	p.last = now
	p.lastT = now
	p.fix = append(p.fix, fix)
	p.faults++
}

// run services all debuggees of the calling thread until each has exited.
func run(ps []*proc, variant string, section uintptr) {
	s := &session{variant: variant, section: section, ctx: valloc(0, 4096, 0x3000, 4), ths: map[uint32]uintptr{}}
	by := map[uint32]*proc{}
	for _, p := range ps {
		by[p.pid] = p
	}
	pending := len(ps)
	evp := valloc(0, 4096, 0x3000, 4) // not a Go stack address: the stack may move
	for pending > 0 {
		r, _, _ := pWaitDE.Call(evp, 60000)
		now := qpcNs()
		if r == 0 {
			for _, p := range ps {
				if !p.done {
					p.err = "timeout"
					pTermP.Call(p.h, 0xDEAD)
				}
			}
			return
		}
		code := *(*uint32)(unsafe.Pointer(evp))
		pid := *(*uint32)(unsafe.Pointer(evp + 4))
		tid := *(*uint32)(unsafe.Pointer(evp + 8))
		p := by[pid]
		status := uintptr(dbgContinue)
		if p != nil {
			if code < 10 {
				p.counts[code]++
				p.phaseCounts[p.phase][code]++
			}
			switch code {
			case 5:
				p.exit = *(*uint32)(unsafe.Pointer(evp + 16))
				p.done = true
				pending--
			case 1:
				status = s.onException(p, tid, evp, now)
			}
		}
		pContDE.Call(uintptr(pid), uintptr(tid), status)
	}
}

func counts(c [10]int) string {
	var parts []string
	total := 0
	for i := 1; i < 10; i++ {
		if c[i] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", evNames[i], c[i]))
			total += c[i]
		}
	}
	return fmt.Sprintf("total=%d (%s)", total, strings.Join(parts, " "))
}

// ---------------- parts ----------------

func faultRun(restricted bool, variant string, k, r int) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	mode := "std"
	var sect uintptr
	if strings.HasPrefix(variant, "map") {
		mode = "map"
		r = 1
		sect, _, _ = pCreMap.Call(^uintptr(0), 0, 4, 0, 4096, 0)
	}
	tag := map[bool]string{false: "normal", true: "lowIL"}[restricted]
	name := fmt.Sprintf("S8.1.%s.%s", tag, variant)
	p, err := spawn(restricted, self(fmt.Sprintf("fault %s %d %d", mode, k, r)))
	if err != nil {
		line(false, name, "spawn error: "+err.Error())
		return
	}
	run([]*proc{p}, variant, sect)
	if p.exit != 1000 || len(p.deltas) < 500 {
		line(false, name, fmt.Sprintf("exit=%#x faults=%d err=%q", p.exit, p.faults, p.err))
		return
	}
	d := p.deltas[100:]
	m, p99, mean := stat(d)
	fm, fp, _ := stat(p.fix)
	line(true, name, fmt.Sprintf("n=%d faults, per fault (event to next event) median=%.1f us p99=%.1f us mean=%.1f us; debugger-side fix+context calls median=%.1f us p99=%.1f us",
		len(d), m/1e3, p99/1e3, mean/1e3, fm/1e3, fp/1e3))
}

func passRun() {
	for _, c := range []struct{ variant, stack string }{
		{"pass-handled", "valid"}, {"pass-nothandled", "valid"}, {"pass-nothandled", "noaccess"},
	} {
		runtime.LockOSThread()
		name := fmt.Sprintf("S8.2.%s.%s", c.variant, c.stack)
		p, err := spawn(false, self("pass "+c.stack))
		if err != nil {
			line(false, name, err.Error())
			runtime.UnlockOSThread()
			continue
		}
		run([]*proc{p}, c.variant, 0)
		runtime.UnlockOSThread()
		d := fmt.Sprintf("debugger saw %d first-chance and %d second-chance exception events (incl. loader breakpoint); child exit %#x", p.fcFirst, p.fcSecond, p.exit)
		if p.exit == 1000 {
			d += " => child VEH did not run"
		} else if p.exit >= 100000 && p.exit <= 160000 {
			d += fmt.Sprintf(" => child VEH ran; its SP at entry = guest RSP-%d (0 = unknown/out of range)", int(p.exit)-100000-30000)
		} else {
			d += " => child died"
		}
		line(true, name, d)
	}
}

func evtRun() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	phases := []string{"create..loader breakpoint", "loader bp..main start (Go runtime init)", "CreateThread + thread exit", "marker..os.Exit"}
	p, err := spawn(false, self("evt"))
	if err != nil {
		line(false, "S8.3.go", err.Error())
		return
	}
	run([]*proc{p}, "evt", 0)
	line(p.exit == 1000, "S8.3.go-child", fmt.Sprintf("exit=%d, whole run %s", p.exit, counts(p.counts)))
	for i, n := range phases {
		fmt.Printf("INFO S8.3.go-child phase %d (%s): %s\n", i, n, counts(p.phaseCounts[i]))
	}
	windir := os.Getenv("SystemRoot")
	q, err := spawn(false, `"`+windir+`\System32\cmd.exe" /c exit 0`)
	if err != nil {
		line(false, "S8.3.native", err.Error())
		return
	}
	run([]*proc{q}, "native", 0)
	line(true, "S8.3.native-cmd", fmt.Sprintf("exit=%d, whole run %s", q.exit, counts(q.counts)))
	fmt.Printf("INFO S8.3.native-cmd before loader breakpoint: %s\n", counts(q.phaseCounts[0]))
}

func conc(threads, per int) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var all []*proc
	for t := 0; t < threads; t++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			var ps []*proc
			for i := 0; i < per; i++ {
				p, err := spawn(false, self("fault std 2048 4"))
				if err != nil {
					fmt.Println("spawn error:", err)
					return
				}
				ps = append(ps, p)
			}
			run(ps, "vp", 0)
			mu.Lock()
			all = append(all, ps...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	faults, bad := 0, 0
	minF, maxL := 1e30, 0.0
	for _, p := range all {
		if p.exit != 1000 {
			bad++
		}
		faults += p.faults
		if p.first < minF {
			minF = p.first
		}
		if p.lastT > maxL {
			maxL = p.lastT
		}
	}
	secs := (maxL - minF) / 1e9
	line(bad == 0 && len(all) == threads*per, fmt.Sprintf("S8.4.threads=%d.stubs_per_thread=%d", threads, per),
		fmt.Sprintf("%d stubs, %d faults in %.2f s = %.0f faults/s total (%.0f per stub/s), failed children=%d",
			len(all), faults, secs, float64(faults)/secs, float64(faults)/secs/float64(len(all)), bad))
}

func fsRun() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if cpuid7()&1 == 0 {
		fmt.Println("INFO S8.5: no FSGSBASE on this CPU, skipped")
		return
	}
	const n = 20000
	p, err := spawn(false, self(fmt.Sprintf("fs %d", n)))
	if err != nil {
		line(false, "S8.5", err.Error())
		return
	}
	run([]*proc{p}, "fs", 0)
	if p.exit < 1000 || len(p.deltas) < 1000 {
		line(false, "S8.5.trampoline", fmt.Sprintf("exit=%d faults=%d err=%q", p.exit, p.faults, p.err))
		return
	}
	d := p.deltas[200:]
	m, p99, mean := stat(d)
	fm, fp, _ := stat(p.fix)
	line(p.exit == 1000, "S8.5.trampoline", fmt.Sprintf("%d fs-base losses (wrfsbase 0; read fs:[0]) repaired, wrong reads=%d; per repair (event to next event) median=%.1f us p99=%.1f us mean=%.1f us; debugger-side Get+WriteProcessMemory+Set median=%.1f us p99=%.1f us",
		p.faults, p.exit-1000, m/1e3, p99/1e3, mean/1e3, fm/1e3, fp/1e3))
}

func main() {
	if len(os.Args) >= 2 {
		a := os.Args
		n := func(i int) int { v, _ := strconv.Atoi(a[i]); return v }
		switch a[1] {
		case "fault":
			childFault(a[2], n(3), n(4))
		case "pass":
			childPass(a[2])
		case "evt":
			childEvt()
		case "fs":
			childFS(n(2))
		}
	}
	var v [72]uint32
	v[0] = 284
	pRtlGetVersion.Call(uintptr(unsafe.Pointer(&v[0])))
	fmt.Printf("INFO Windows build: %d.%d.%d, cpus=%d, FSGSBASE=%v\n", v[1], v[2], v[3], runtime.NumCPU(), cpuid7()&1 == 1)
	fmt.Println("===== S8.1 faults fixed from outside")
	for _, v := range []string{"vp", "vp+ctl", "vp+int", "vp+ctlint", "vp+full", "vp+ctlset", "map"} {
		faultRun(false, v, 2048, 10)
	}
	for _, v := range []string{"vp", "vp+ctl"} {
		faultRun(true, v, 2048, 10)
	}
	fmt.Println("===== S8.2 pass-through")
	passRun()
	fmt.Println("===== S8.3 event counts")
	evtRun()
	fmt.Println("===== S8.4 concurrency")
	for _, c := range [][2]int{{1, 1}, {1, 4}, {1, 16}, {2, 4}, {4, 4}, {4, 1}} {
		conc(c[0], c[1])
	}
	fmt.Println("===== S8.5 FS-base repair")
	fsRun()
}
