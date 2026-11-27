// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build windows

package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procGetProcessHandleCount = kernel32.NewProc("GetProcessHandleCount")
	psapi                     = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo  = psapi.NewProc("GetProcessMemoryInfo")
)

type processMemoryCounters struct {
	cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// dumpGoroutines writes the stacks of all goroutines to stderr.
func dumpGoroutines() {
	buf := make([]byte, 8<<20)
	n := runtime.Stack(buf, true)
	nl := string(rune(10))
	fmt.Fprint(os.Stderr, "STALL goroutine dump ("+fmt.Sprint(runtime.NumGoroutine())+" goroutines):"+nl+string(buf[:n])+nl+"STALL end"+nl)
}

// monitorHandles prints, every interval, the number of handles of this
// process (and of goroutines, and the memory in use) to stderr. A count that
// grows with the number of guest processes is a leak.
func monitorHandles(interval time.Duration) {
	self := windows.CurrentProcess()
	start := time.Now()
	for {
		var n uint32
		procGetProcessHandleCount.Call(uintptr(self), uintptr(unsafe.Pointer(&n)))
		var m processMemoryCounters
		m.cb = uint32(unsafe.Sizeof(m))
		procGetProcessMemoryInfo.Call(uintptr(self), uintptr(unsafe.Pointer(&m)), uintptr(m.cb))
		fmt.Fprintf(os.Stderr, "HANDLES t=%ds handles=%d goroutines=%d working_set_mb=%d commit_mb=%d\n",
			int(time.Since(start).Seconds()), n, runtime.NumGoroutine(), m.WorkingSetSize>>20, m.PagefileUsage>>20)
		time.Sleep(interval)
	}
}

var procNtQuerySystemInformation = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQuerySystemInformation")

type childProc struct {
	pid     uint32
	image   string
	created int64 // 100ns since 1601
	threads []string
}

// childProcesses lists the processes this process started (the stubs of the
// guest processes) with the state of their threads, from
// NtQuerySystemInformation(SystemProcessInformation).
func childProcesses() []childProc {
	buf := make([]byte, 1<<20)
	for {
		var need uint32
		st, _, _ := procNtQuerySystemInformation.Call(5, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&need)))
		if st == 0xC0000004 { // STATUS_INFO_LENGTH_MISMATCH
			buf = make([]byte, need+(1<<16))
			continue
		}
		if st != 0 {
			return nil
		}
		break
	}
	self := uint64(windows.GetCurrentProcessId())
	var out []childProc
	for off := 0; ; {
		p := buf[off:]
		next := *(*uint32)(unsafe.Pointer(&p[0]))
		nthreads := *(*uint32)(unsafe.Pointer(&p[4]))
		created := *(*int64)(unsafe.Pointer(&p[32]))
		parent := *(*uint64)(unsafe.Pointer(&p[88]))
		if parent == self {
			c := childProc{pid: uint32(*(*uint64)(unsafe.Pointer(&p[80]))), created: created}
			nameLen := int(*(*uint16)(unsafe.Pointer(&p[56])))
			if nameLen > 0 {
				w := unsafe.Slice(*(**uint16)(unsafe.Pointer(&p[64])), nameLen/2)
				c.image = windows.UTF16ToString(w)
			}
			for t := 0; t < int(nthreads); t++ {
				th := p[256+80*t:]
				state := *(*uint32)(unsafe.Pointer(&th[68]))
				reason := *(*uint32)(unsafe.Pointer(&th[72]))
				c.threads = append(c.threads, fmt.Sprintf("%d/%d", state, reason))
			}
			out = append(out, c)
		}
		if next == 0 {
			break
		}
		off += int(next)
	}
	return out
}

// dumpChildren prints the stub processes: a thread state of "5/5" is waiting
// while suspended, "5/0" waiting on an object (states: 2 running, 5 waiting).
func dumpChildren() {
	now := time.Now().UnixNano()/100 + 116444736000000000
	nl := string(rune(10))
	var sb strings.Builder
	for _, c := range childProcesses() {
		sb.WriteString(fmt.Sprintf("STALL child pid=%d image=%s age=%ds threads=%v%s", c.pid, c.image, (now-c.created)/10_000_000, c.threads, nl))
	}
	fmt.Fprint(os.Stderr, sb.String())
}

// killYoungestChild terminates the newest child process: after a stall it is
// the stub of the guest process that never started, and ending it lets the
// Sentry's address space release finish. A workaround for finding out what
// else would stall, not a fix.
func killYoungestChild() {
	var y *childProc
	cs := childProcesses()
	for i := range cs {
		if y == nil || cs[i].created > y.created {
			y = &cs[i]
		}
	}
	if y == nil {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, y.pid)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	windows.TerminateProcess(h, 1)
	fmt.Fprintf(os.Stderr, "STALL killed child pid=%d%s", y.pid, string(rune(10)))
}
