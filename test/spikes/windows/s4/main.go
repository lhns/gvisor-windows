package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

func linuxsys(n, a1, a2, a3, a4, a5, a6 uintptr) uintptr

var (
	k32  = syscall.NewLazyDLL("kernel32.dll")
	adv  = syscall.NewLazyDLL("advapi32.dll")
	kb   = syscall.NewLazyDLL("kernelbase.dll")

	pOpenProcessToken   = adv.NewProc("OpenProcessToken")
	pCreateRestricted   = adv.NewProc("CreateRestrictedToken")
	pSetTokenInfo       = adv.NewProc("SetTokenInformation")
	pGetTokenInfo       = adv.NewProc("GetTokenInformation")
	pStrToSid           = adv.NewProc("ConvertStringSidToSidW")
	pCreateProcessAsUser = adv.NewProc("CreateProcessAsUserW")
	pCreateJob          = k32.NewProc("CreateJobObjectW")
	pSetJobInfo         = k32.NewProc("SetInformationJobObject")
	pAssignJob          = k32.NewProc("AssignProcessToJobObject")
	pVirtualAllocEx     = k32.NewProc("VirtualAllocEx")
	pWPM                = k32.NewProc("WriteProcessMemory")
	pRPM                = k32.NewProc("ReadProcessMemory")
	pGetCtx             = k32.NewProc("GetThreadContext")
	pSetCtx             = k32.NewProc("SetThreadContext")
	pResume             = k32.NewProc("ResumeThread")
	pVirtualAlloc       = k32.NewProc("VirtualAlloc")
	pCreateMapping      = k32.NewProc("CreateFileMappingW")
	pVirtualProtectEx   = k32.NewProc("VirtualProtectEx")
	pTerminate          = k32.NewProc("TerminateProcess")
	pMapViewOfFile3     = kb.NewProc("MapViewOfFile3")
	pVirtualAlloc2      = kb.NewProc("VirtualAlloc2")
)

func errs(e error) string {
	if en, ok := e.(syscall.Errno); ok {
		return fmt.Sprintf("%d(%#x)", uint32(en), uint32(en))
	}
	return fmt.Sprint(e)
}

func line(ok bool, name, detail string) {
	s := "PASS"
	if !ok {
		s = "FAIL"
	}
	fmt.Printf("%s %s: %s\n", s, name, detail)
}

func sid(s string) uintptr {
	var p uintptr
	u, _ := syscall.UTF16PtrFromString(s)
	r, _, e := pStrToSid.Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(&p)))
	if r == 0 {
		panic("sid " + s + errs(e))
	}
	return p
}

type sidAndAttr struct {
	Sid  uintptr
	Attr uint32
	_    uint32
}

func selfToken() uintptr {
	cur, _ := syscall.GetCurrentProcess()
	var t syscall.Token
	if err := syscall.OpenProcessToken(cur, 0x2|0x8|0x80|0x20|0x1, &t); err != nil { // DUPLICATE|QUERY|ADJUST_DEFAULT|ADJUST_PRIVS|ASSIGN_PRIMARY
		panic(err)
	}
	return uintptr(t)
}

// mkToken builds a restricted token. variant bits: lowIL, restrictSids
func mkToken(disableMax bool, denySids []string, restrictSids []string, lowIL bool) (uintptr, string) {
	base := selfToken()
	var deny []sidAndAttr
	for _, s := range denySids {
		deny = append(deny, sidAndAttr{Sid: sid(s)})
	}
	var rest []sidAndAttr
	for _, s := range restrictSids {
		rest = append(rest, sidAndAttr{Sid: sid(s)})
	}
	var flags uintptr
	if disableMax {
		flags |= 1
	}
	var dp, rp uintptr
	if len(deny) > 0 {
		dp = uintptr(unsafe.Pointer(&deny[0]))
	}
	if len(rest) > 0 {
		rp = uintptr(unsafe.Pointer(&rest[0]))
	}
	var nt uintptr
	r, _, e := pCreateRestricted.Call(base, flags, uintptr(len(deny)), dp, 0, 0, uintptr(len(rest)), rp, uintptr(unsafe.Pointer(&nt)))
	if r == 0 {
		return 0, "CreateRestrictedToken err " + errs(e)
	}
	if lowIL {
		low := sid("S-1-16-4096")
		tml := sidAndAttr{Sid: low, Attr: 0x20}
		r, _, e = pSetTokenInfo.Call(nt, 25, uintptr(unsafe.Pointer(&tml)), unsafe.Sizeof(tml)+8)
		if r == 0 {
			return 0, "SetTokenInformation(IL) err " + errs(e)
		}
	}
	return nt, "ok"
}

func mkJob(uiLimits bool, activeLimit1 bool) uintptr {
	j, _, _ := pCreateJob.Call(0, 0)
	buf, _, _ := pVirtualAlloc.Call(0, 4096, 0x3000, 4)
	*(*uint32)(unsafe.Pointer(buf + 16)) = 0x2000 // KILL_ON_JOB_CLOSE
	if activeLimit1 {
		*(*uint32)(unsafe.Pointer(buf + 16)) |= 0x8
		*(*uint32)(unsafe.Pointer(buf + 40)) = 1
	}
	r, _, e := pSetJobInfo.Call(j, 9, buf, 144)
	if r == 0 {
		fmt.Println("INFO SetInformationJobObject(ext limits) err", errs(e))
	}
	if uiLimits {
		ui := uint32(0xFF) // HANDLES|READ/WRITECLIPBOARD|SYSTEMPARAMETERS|DISPLAYSETTINGS|GLOBALATOMS|DESKTOP|EXITWINDOWS
		r, _, e = pSetJobInfo.Call(j, 4, uintptr(unsafe.Pointer(&ui)), 4)
		if r == 0 {
			fmt.Println("INFO SetInformationJobObject(ui limits) err", errs(e))
		}
	}
	return j
}

type spawned struct {
	pi  syscall.ProcessInformation
	out syscall.Handle
}

func spawn(tok uintptr, args string, job uintptr) (*spawned, string) {
	var rd, wr syscall.Handle
	sa := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), InheritHandle: 1}
	if err := syscall.CreatePipe(&rd, &wr, &sa, 0); err != nil {
		return nil, "pipe " + err.Error()
	}
	syscall.SetHandleInformation(rd, 1, 0)
	exe, _ := os.Executable()
	cl, _ := syscall.UTF16PtrFromString(`"` + exe + `" ` + args)
	var si syscall.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = 0x100
	si.StdOutput, si.StdErr = wr, wr
	cur, _ := syscall.GetCurrentProcess()
	_ = cur
	var pi syscall.ProcessInformation
	flags := uintptr(0x4 | 0x08000000 | 0x01000000) // SUSPENDED | NO_WINDOW | BREAKAWAY_FROM_JOB? (0x01000000)
	var r uintptr
	var e error
	for _, f := range []uintptr{flags, 0x4 | 0x08000000} {
		if tok == 0 {
			err := syscall.CreateProcess(nil, cl, nil, nil, true, uint32(f), nil, nil, &si, &pi)
			if err != nil {
				e = err
				r = 0
			} else {
				r = 1
			}
		} else {
			r, _, e = pCreateProcessAsUser.Call(tok, 0, uintptr(unsafe.Pointer(cl)), 0, 0, 1, f, 0, 0, uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&pi)))
		}
		if r != 0 {
			break
		}
	}
	syscall.CloseHandle(wr)
	if r == 0 {
		return nil, "CreateProcess[AsUser] err " + errs(e)
	}
	if job != 0 {
		r, _, e = pAssignJob.Call(job, uintptr(pi.Process))
		if r == 0 {
			pTerminate.Call(uintptr(pi.Process), 1)
			return nil, "AssignProcessToJobObject err " + errs(e)
		}
	}
	return &spawned{pi, rd}, "ok"
}

func (s *spawned) finish() (string, string) {
	pResume.Call(uintptr(s.pi.Thread))
	ev, _ := syscall.WaitForSingleObject(s.pi.Process, 8000)
	status := ""
	if ev == syscall.WAIT_TIMEOUT {
		pTerminate.Call(uintptr(s.pi.Process), 0xDEAD)
		status = "TIMEOUT(killed)"
	}
	var code uint32
	syscall.GetExitCodeProcess(s.pi.Process, &code)
	var b [4096]byte
	var n uint32
	syscall.ReadFile(s.out, b[:], &n, nil)
	syscall.CloseHandle(s.out)
	syscall.CloseHandle(s.pi.Process)
	syscall.CloseHandle(s.pi.Thread)
	return strings.TrimSpace(string(b[:n])), fmt.Sprintf("exit=%#x %s", code, status)
}

func parentOps(s *spawned) {
	h := uintptr(s.pi.Process)
	r, _, e := pVirtualAllocEx.Call(h, 0, 0x10000, 0x3000, 4)
	line(r != 0, "  VirtualAllocEx", errs(e))
	if r != 0 {
		v := uint64(0x1122334455667788)
		var n uintptr
		r2, _, e2 := pWPM.Call(h, r, uintptr(unsafe.Pointer(&v)), 8, uintptr(unsafe.Pointer(&n)))
		var back uint64
		pRPM.Call(h, r, uintptr(unsafe.Pointer(&back)), 8, uintptr(unsafe.Pointer(&n)))
		line(r2 != 0 && back == v, "  WriteProcessMemory/ReadProcessMemory", errs(e2))
		var old uint32
		r3, _, e3 := pVirtualProtectEx.Call(h, r+0x1000, 0x1000, 1, uintptr(unsafe.Pointer(&old)))
		line(r3 != 0, "  VirtualProtectEx(4K)", errs(e3))
	}
	// placeholder + MapViewOfFile3 REPLACE_PLACEHOLDER
	pl, _, e := pVirtualAlloc2.Call(h, 0, 0x20000, 0x2000|0x40000, 1, 0, 0)
	if pl == 0 {
		line(false, "  VirtualAlloc2 placeholder", errs(e))
	} else {
		sec, _, _ := pCreateMapping.Call(^uintptr(0), 0, 4, 0, 0x10000, 0)
		syscall := k32.NewProc("VirtualFreeEx")
		syscall.Call(h, pl, 0x10000, 0x8000|0x2)
		a, _, e := pMapViewOfFile3.Call(sec, h, pl, 0, 0x10000, 0x4000, 4, 0, 0)
		line(a != 0, "  MapViewOfFile3(REPLACE_PLACEHOLDER) section into child", errs(e))
	}
	// Get/SetThreadContext on the initial thread
	buf, _, _ := pVirtualAlloc.Call(0, 4096, 0x3000, 4)
	*(*uint32)(unsafe.Pointer(buf + 0x30)) = 0x10001F // CONTEXT_FULL
	r, _, e = pGetCtx.Call(uintptr(s.pi.Thread), buf)
	rip := *(*uint64)(unsafe.Pointer(buf + 0xF8))
	line(r != 0, "  GetThreadContext", fmt.Sprintf("%s rip=%#x", errs(e), rip))
	r, _, e = pSetCtx.Call(uintptr(s.pi.Thread), buf)
	line(r != 0, "  SetThreadContext", errs(e))
}

func child(args []string) {
	switch args[0] {
	case "sys":
		n, _ := strconv.Atoi(args[1])
		msg := []byte("hi\n")
		fmt.Printf("child: about to issue raw syscall nr=%d (rdi=1 rsi=&buf rdx=3 r10=0)\n", n)
		ret := linuxsys(uintptr(n), 1, uintptr(unsafe.Pointer(&msg[0])), 3, 0, 0, 0)
		fmt.Printf("child: syscall returned rax=%#x\n", ret)
	case "id":
		// report integrity level, privileges are visible from parent; just show we ran
		describeSelf()
	}
}

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "sys" || os.Args[1] == "id") {
		child(os.Args[1:])
		return
	}

	type variant struct {
		name  string
		mk    func() (uintptr, string)
		job   func() uintptr
	}
	vars := []variant{
		{"V0 baseline (normal token, no job)", func() (uintptr, string) { return 0, "ok" }, nil},
		{"V1 restricted: DISABLE_MAX_PRIVILEGE + deny-only Administrators + low IL", func() (uintptr, string) {
			return mkToken(true, []string{"S-1-5-32-544"}, nil, true)
		}, nil},
		{"V2 V1 + job (KILL_ON_CLOSE, 1 active proc, all UI limits)", func() (uintptr, string) {
			return mkToken(true, []string{"S-1-5-32-544"}, nil, true)
		}, func() uintptr { return mkJob(true, true) }},
		{"V3 restricting SIDs {RESTRICTED S-1-5-12, Everyone} + DISABLE_MAX_PRIVILEGE + low IL + job", func() (uintptr, string) {
			return mkToken(true, []string{"S-1-5-32-544"}, []string{"S-1-5-12", "S-1-1-0"}, true)
		}, func() uintptr { return mkJob(true, false) }},
		{"V4 job only (normal token, UI limits)", func() (uintptr, string) { return 0, "ok" }, func() uintptr { return mkJob(true, false) }},
	}
	for _, v := range vars {
		fmt.Println("=====", v.name)
		tok, msg := v.mk()
		if tok == 0 && msg != "ok" {
			line(false, "token creation", msg)
			continue
		}
		line(true, "token creation", msg)
		var job uintptr
		if v.job != nil {
			job = v.job()
		}
		s, m := spawn(tok, "id", job)
		if s == nil {
			line(false, "spawn suspended child", m)
			continue
		}
		line(true, "spawn suspended child", fmt.Sprintf("pid=%d", s.pi.ProcessId))
		parentOps(s)
		out, st := s.finish()
		line(strings.Contains(out, "ran OK"), "child runs (Go runtime starts)", fmt.Sprintf("%q %s", out, st))
		for _, nr := range []int{0, 1, 9, 12, 39, 60, 102, 231} {
			s, m := spawn(tok, "sys "+strconv.Itoa(nr), job)
			if s == nil {
				fmt.Printf("FAIL sys %d spawn: %s\n", nr, m)
				continue
			}
			out, st := s.finish()
			fmt.Printf("RESULT sys nr=%d: %s | %s\n", nr, strings.ReplaceAll(out, "\n", " / "), st)
		}
	}
}

func describeSelf() {
	cur, _ := syscall.GetCurrentProcess()
	var t syscall.Token
	syscall.OpenProcessToken(cur, 0x8, &t)
	var b [1024]byte
	var n uint32
	r, _, _ := pGetTokenInfo.Call(uintptr(t), 25, uintptr(unsafe.Pointer(&b[0])), 1024, uintptr(unsafe.Pointer(&n)))
	il := uint32(0)
	if r != 0 {
		sidp := *(*uintptr)(unsafe.Pointer(&b[0]))
		cnt := *(*byte)(unsafe.Pointer(sidp + 1))
		il = *(*uint32)(unsafe.Pointer(sidp + 8 + uintptr(cnt-1)*4))
	}
	r, _, _ = pGetTokenInfo.Call(uintptr(t), 3, uintptr(unsafe.Pointer(&b[0])), 1024, uintptr(unsafe.Pointer(&n)))
	np := *(*uint32)(unsafe.Pointer(&b[0]))
	enabled := 0
	for i := uint32(0); i < np; i++ {
		attr := *(*uint32)(unsafe.Pointer(&b[4+i*12+8]))
		if attr&2 != 0 {
			enabled++
		}
	}
	f, err := os.Create(os.Getenv("TEMP") + `\s4_write_probe.txt`)
	w := "write to %TEMP%: ok"
	if err != nil {
		w = "write to %TEMP%: " + err.Error()
	} else {
		f.Close()
		os.Remove(os.Getenv("TEMP") + `\s4_write_probe.txt`)
	}
	p, _, e := k32.NewProc("OpenProcess").Call(0x1FFFFF, 0, uintptr(os.Getppid()))
	op := "OpenProcess(parent, ALL_ACCESS): ok"
	if p == 0 {
		op = "OpenProcess(parent, ALL_ACCESS): err " + errs(e)
	}
	fmt.Printf("child: ran OK; integrity RID=%#x privileges=%d (enabled %d); %s; %s\n", il, np, enabled, w, op)
}
