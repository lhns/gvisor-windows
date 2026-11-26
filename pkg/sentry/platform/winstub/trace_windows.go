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

//go:build windows && amd64

package winstub

import (
	"fmt"
	"os"
	"sync"
	"unsafe"
)

var (
	traceOn = os.Getenv("WINSTUB_TRACE") != ""
	trace0  = qpc()
	traceMu sync.Mutex
)

// Trace prints a timestamped line (milliseconds since the process started) to
// stderr when WINSTUB_TRACE is set. The Sentry harness uses it too, so platform
// and boot events share one timeline.
func Trace(format string, a ...any) {
	if !traceOn {
		return
	}
	traceMu.Lock()
	fmt.Fprintf(os.Stderr, "trace %9.3fms %s\n", float64(qpc()-trace0)/1e6, fmt.Sprintf(format, a...))
	traceMu.Unlock()
}

var (
	pQPC     = k32.NewProc("QueryPerformanceCounter")
	pQPF     = k32.NewProc("QueryPerformanceFrequency")
	qpcFreq  float64
)

// qpc returns a high-resolution monotonic time in ns (Go's own clock ticks only
// every 0.5-15 ms on Windows).
func qpc() int64 {
	if qpcFreq == 0 {
		var f int64
		pQPF.Call(uintptr(unsafe.Pointer(&f)))
		qpcFreq = float64(f)
	}
	var c int64
	pQPC.Call(uintptr(unsafe.Pointer(&c)))
	return int64(float64(c) / qpcFreq * 1e9)
}
