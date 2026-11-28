// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

// Package stallwatch tells a stalled process or machine from a slow code path:
// two heartbeats wake every 2 ms, one a goroutine (Go's scheduler and timers),
// one a locked OS thread on a high-resolution waitable timer (Windows only), and
// report every gap longer than a threshold with the system-wide QPC time, so
// gaps in different processes can be lined up (docs/results/3e-signal-latency.md).
// A gap in the goroutine only points at the Go runtime; in both, at the process
// or the machine; in every process at once, at the machine. With perCPU, one more
// heartbeat per processor runs pinned to it at THREAD_PRIORITY_TIME_CRITICAL
// (above any boost a normal thread can get): a gap there alone
// means that processor (a vCPU the hypervisor did not run, or a storm of
// interrupts) stopped, while the rest of the machine went on.
package stallwatch

import (
	"fmt"
	"io"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	k32       = syscall.NewLazyDLL("kernel32.dll")
	pQPC      = k32.NewProc("QueryPerformanceCounter")
	pQPF      = k32.NewProc("QueryPerformanceFrequency")
	pCreateWT = k32.NewProc("CreateWaitableTimerExW")
	pSetWT    = k32.NewProc("SetWaitableTimer")
	pWait     = k32.NewProc("WaitForSingleObject")
	pSleep    = k32.NewProc("Sleep")
	pAffinity = k32.NewProc("SetThreadAffinityMask")
	pPriority = k32.NewProc("SetThreadPriority")
	freq      float64
	freqOnce  sync.Once
)

// Now returns the QPC time in ns (system-wide, the same in every process).
func Now() int64 {
	freqOnce.Do(func() {
		var f int64
		pQPF.Call(uintptr(unsafe.Pointer(&f)))
		freq = float64(f)
	})
	var c int64
	pQPC.Call(uintptr(unsafe.Pointer(&c)))
	return int64(float64(c) / freq * 1e9)
}

// Watch runs the heartbeats until Stop.
type Watch struct {
	label string
	w     io.Writer
	min   int64
	mu    sync.Mutex
	stop  bool
	kinds []string
	beats []int64
	gaps  []int64
	max   []int64
	list  []Gap
	wg    sync.WaitGroup
}

// Gap is a heartbeat gap of at least KeepGap (QPC ns).
type Gap struct {
	Kind       string
	Start, End int64
}

// KeepGap is the shortest gap Gaps returns.
const KeepGap = 5 * time.Millisecond

// Start starts the heartbeats; gaps of at least minGap are written to w as they end.
func Start(label string, w io.Writer, minGap time.Duration, perCPU bool) *Watch {
	s := &Watch{label: label, w: w, min: int64(minGap), kinds: []string{"goroutine", "os-thread"}}
	n := 0
	if perCPU {
		n = min(runtime.NumCPU(), 64)
		for i := 0; i < n; i++ {
			s.kinds = append(s.kinds, fmt.Sprintf("cpu%d", i))
		}
	}
	s.beats, s.gaps, s.max = make([]int64, len(s.kinds)), make([]int64, len(s.kinds)), make([]int64, len(s.kinds))
	s.wg.Add(len(s.kinds))
	go s.loop(0, func() { time.Sleep(2 * time.Millisecond) })
	go s.thread(1, -1)
	for i := 0; i < n; i++ {
		go s.thread(2+i, i)
	}
	return s
}

// thread runs heartbeat k on a locked OS thread, pinned to processor cpu at
// THREAD_PRIORITY_TIME_CRITICAL if cpu >= 0.
func (s *Watch) thread(k, cpu int) {
	runtime.LockOSThread()
	if cpu >= 0 {
		const self = ^uintptr(1) // GetCurrentThread()
		pAffinity.Call(self, uintptr(1)<<cpu)
		pPriority.Call(self, 15)
	}
	// CREATE_WAITABLE_TIMER_HIGH_RESOLUTION, TIMER_ALL_ACCESS
	h, _, _ := pCreateWT.Call(0, 0, 2, 0x1F0003)
	sleep := func() { pSleep.Call(1) }
	if h != 0 {
		sleep = func() {
			due := int64(-20000) // 2 ms, relative, in 100 ns
			pSetWT.Call(h, uintptr(unsafe.Pointer(&due)), 0, 0, 0, 0)
			pWait.Call(h, 0xFFFFFFFF)
		}
	}
	s.loop(k, sleep)
}

func (s *Watch) loop(k int, sleep func()) {
	defer s.wg.Done()
	last := Now()
	for {
		sleep()
		now := Now()
		gap := now - last
		last = now
		s.mu.Lock()
		if s.stop {
			s.mu.Unlock()
			return
		}
		s.beats[k]++
		s.max[k] = max(s.max[k], gap)
		if gap >= int64(KeepGap) && len(s.list) < 1e6 {
			s.list = append(s.list, Gap{s.kinds[k], now - gap, now})
		}
		if gap >= s.min {
			s.gaps[k]++
			fmt.Fprintf(s.w, "STALL %s %s gap %.2f ms ending at qpc %.3f ms, %s UTC\n", s.label, s.kinds[k], float64(gap)/1e6, float64(now)/1e6, time.Now().UTC().Format("15:04:05.000"))
		}
		s.mu.Unlock()
	}
}

// Stop ends the heartbeats and writes a summary line.
func (s *Watch) Stop() {
	s.mu.Lock()
	s.stop = true
	s.mu.Unlock()
	s.wg.Wait()
	for k := range s.kinds {
		fmt.Fprintf(s.w, "STALL %s %s summary: %d beats, %d gaps over %.0f ms, max %.2f ms\n", s.label, s.kinds[k], s.beats[k], s.gaps[k], float64(s.min)/1e6, float64(s.max[k])/1e6)
	}
}

// Gaps returns the gaps of at least KeepGap seen so far, in the order they ended.
func (s *Watch) Gaps() []Gap {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Gap(nil), s.list...)
}
