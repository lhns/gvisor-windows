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
