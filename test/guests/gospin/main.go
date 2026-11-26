// Static Go guest that keeps several OS threads busy in loops without function
// calls (only asynchronous preemption, i.e. signals, can stop them) while the
// main goroutine forces garbage collections.
package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	var stop atomic.Bool
	var wg sync.WaitGroup
	sums := make([]uint64, 4)
	for i := range sums {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			x := uint64(i + 1)
			for !stop.Load() {
				for j := 0; j < 1000; j++ {
					x = x*6364136223846793005 + 1442695040888963407
				}
			}
			sums[i] = x
		}(i)
	}
	end := time.Now().Add(1500 * time.Millisecond)
	gcs := 0
	for time.Now().Before(end) {
		runtime.GC()
		gcs++
	}
	stop.Store(true)
	wg.Wait()
	if gcs < 3 {
		fmt.Println("too few collections:", gcs)
		return
	}
	fmt.Println("gospin ok")
}
