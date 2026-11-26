// A Podman-shaped load: GOMAXPROCS=8, 200 goroutines doing file I/O, pipes,
// futex-backed channels and mutexes and timers, while 50 short-lived children
// are started. It checks the data it moves and reports throughput and latency.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	workers  = 200
	rounds   = 40
	children = 50
)

func main() {
	runtime.GOMAXPROCS(8)
	dir := "/t/podman"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Println("mkdir:", err)
		os.Exit(1)
	}
	var (
		ops    atomic.Int64
		bad    atomic.Int64
		mu     sync.Mutex
		lats   []time.Duration
		shared int
	)
	record := func(d time.Duration) {
		mu.Lock()
		lats = append(lats, d)
		mu.Unlock()
	}
	start := time.Now()
	var wg sync.WaitGroup

	// Children, started while the workers run: at most 5 at a time.
	var childMu sync.Mutex
	var childDur []time.Duration
	var childFail atomic.Int64
	sem := make(chan struct{}, 5)
	for i := 0; i < children; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			t0 := time.Now()
			if err := exec.Command("/bin/true").Run(); err != nil {
				if childFail.Add(1) == 1 {
					fmt.Println("gopodman: first child error:", err)
				}
				return
			}
			childMu.Lock()
			childDur = append(childDur, time.Since(t0))
			childMu.Unlock()
		}()
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			pr, pw, err := os.Pipe()
			if err != nil {
				bad.Add(1)
				return
			}
			defer pr.Close()
			defer pw.Close()
			payload := bytes.Repeat([]byte{byte(w)}, 100+w)
			buf := make([]byte, len(payload))
			for r := 0; r < rounds; r++ {
				t0 := time.Now()
				// file on tmpfs: create, write, read back, remove
				name := fmt.Sprintf("%s/f%d", dir, w)
				if err := os.WriteFile(name, payload, 0o644); err != nil {
					bad.Add(1)
					continue
				}
				got, err := os.ReadFile(name)
				if err != nil || !bytes.Equal(got, payload) {
					bad.Add(1)
				}
				os.Remove(name)
				// pipe: write and read through the kernel
				if _, err := pw.Write(payload); err != nil {
					bad.Add(1)
				}
				if n, err := pr.Read(buf); err != nil || n != len(payload) || !bytes.Equal(buf, payload) {
					bad.Add(1)
				}
				// mutex (futex) and a timer
				mu.Lock()
				shared++
				mu.Unlock()
				if r%10 == 0 {
					time.Sleep(time.Millisecond)
				}
				ops.Add(5)
				record(time.Since(t0))
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start)

	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	sort.Slice(childDur, func(i, j int) bool { return childDur[i] < childDur[j] })
	n := len(lats)
	fmt.Printf("gopodman: %d syscall-level ops by %d goroutines in %v (%.0f ops/s); round latency p50 %v p99 %v max %v\n",
		ops.Load(), workers, elapsed.Round(time.Millisecond), float64(ops.Load())/elapsed.Seconds(), lats[n/2], lats[n*99/100], lats[n-1])
	if len(childDur) > 0 {
		fmt.Printf("gopodman: %d children (%d failed): p50 %v max %v\n", len(childDur), childFail.Load(), childDur[len(childDur)/2], childDur[len(childDur)-1])
	}
	if bad.Load() != 0 || shared != workers*rounds || childFail.Load() != 0 || len(childDur) != children {
		fmt.Printf("gopodman: FAILED: bad=%d shared=%d children ok=%d failed=%d\n", bad.Load(), shared, len(childDur), childFail.Load())
		os.Exit(1)
	}
	fmt.Println("gopodman ok")
}
