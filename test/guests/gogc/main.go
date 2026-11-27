// A Go program that runs the garbage collector hard on several threads: workers
// allocate and drop buffers of random sizes with a low GC percentage, and the heap
// grows and shrinks (page faults and madvise are debug events and mapping changes
// for the platform). Every GC worker thread reloads its g from %fs:-8 after calls,
// so a lost %fs base shows up as SIGSEGV at 0xfffffffffffffff8 (docs/results/1m).
package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"time"
)

type node struct {
	next *node
	buf  []byte
}

func main() {
	secs := 20
	if len(os.Args) > 1 {
		secs, _ = strconv.Atoi(os.Args[1])
	}
	runtime.GOMAXPROCS(8)
	debug.SetGCPercent(10)
	end := time.Now().Add(time.Duration(secs) * time.Second)
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for w := 0; w < 12; w++ {
		wg.Add(1)
		go func(seed uint32) {
			defer wg.Done()
			var head *node
			n, live := 0, 0
			for time.Now().Before(end) {
				seed = seed*1664525 + 1013904223
				size := 64 << (seed >> 28 & 0xF) // 64 B .. 2 MiB
				head = &node{next: head, buf: make([]byte, size)}
				head.buf[size-1] = byte(seed)
				live++
				n++
				if live > 40 { // drop the list: pointer-rich garbage for the scanner
					head, live = nil, 0
				}
			}
			mu.Lock()
			total += n
			mu.Unlock()
		}(uint32(w) + 1)
	}
	go func() { // heap shrinks: the scavenger and forced collections
		for time.Now().Before(end) {
			time.Sleep(50 * time.Millisecond)
			debug.FreeOSMemory()
		}
	}()
	wg.Wait()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("gogc ok: allocs>0=%v gcs=%d\n", total > 0, ms.NumGC)
}
