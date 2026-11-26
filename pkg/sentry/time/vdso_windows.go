// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package time

import (
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
	"golang.org/x/sys/windows"
)

// unixEpochFiletime is the number of 100ns ticks between the FILETIME epoch
// (1601) and the Unix epoch.
const unixEpochFiletime = 116444736000000000

var (
	modKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	procQPC     = modKernel32.NewProc("QueryPerformanceCounter")
	procQPF     = modKernel32.NewProc("QueryPerformanceFrequency")
)

var (
	qpcOnce sync.Once
	qpcFreq int64
)

// vdsoClockGettime reads the Sentry's own host clocks: Realtime from the
// precise system time and the monotonic clocks from the performance counter.
// It returns 0 on success or a negative errno value on failure, like the
// Linux implementation.
//
// gvisor-windows: replaces the assembly that calls the Linux VDSO.
func vdsoClockGettime(clockid ClockID, ts *unix.Timespec) int {
	switch clockid {
	case Realtime:
		var ft windows.Filetime
		windows.GetSystemTimePreciseAsFileTime(&ft)
		ticks := (int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)) - unixEpochFiletime
		ts.Sec = ticks / 10_000_000
		ts.Nsec = (ticks % 10_000_000) * 100
		return 0
	case Monotonic, MonotonicRaw:
		qpcOnce.Do(func() {
			if r, _, _ := procQPF.Call(uintptr(unsafe.Pointer(&qpcFreq))); r == 0 || qpcFreq == 0 {
				panic("QueryPerformanceFrequency failed")
			}
		})
		var c int64
		if r, _, _ := procQPC.Call(uintptr(unsafe.Pointer(&c))); r == 0 {
			return -int(unix.EINVAL)
		}
		ts.Sec = c / qpcFreq
		ts.Nsec = (c % qpcFreq) * 1_000_000_000 / qpcFreq
		return 0
	default:
		return -int(unix.EINVAL)
	}
}
