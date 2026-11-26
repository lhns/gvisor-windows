// Goroutines over GOMAXPROCS=4: channels, timers and CPU-bound loops that can
// only be stopped by asynchronous preemption (SIGURG).
package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	runtime.GOMAXPROCS(4)
	var stop atomic.Bool
	var spun atomic.Int64
	var spinners sync.WaitGroup
	for i := 0; i < 6; i++ { // more than the 4 Ps, no calls in the loop
		spinners.Add(1)
		go func() {
			defer spinners.Done()
			n := int64(0)
			for !stop.Load() {
				n++
			}
			spun.Add(n)
		}()
	}

	// Timers must fire although every P is busy: needs preemption.
	ticks := 0
	tk := time.NewTicker(5 * time.Millisecond)
	for ticks < 20 {
		<-tk.C
		ticks++
	}
	tk.Stop()
	<-time.After(50 * time.Millisecond)

	// Channels: a pipeline of 8 stages.
	src := make(chan int)
	var in <-chan int = src
	for s := 0; s < 8; s++ {
		out := make(chan int)
		go func(in <-chan int, out chan<- int) {
			for v := range in {
				out <- v + 1
			}
			close(out)
		}(in, out)
		in = out
	}
	go func() {
		for i := 0; i < 1000; i++ {
			src <- i
		}
		close(src)
	}()
	sum := 0
	for v := range in {
		sum += v
	}

	stop.Store(true)
	spinners.Wait()
	fmt.Printf("gostress ok: ticks=%d sum=%d spun=%v procs=%d\n", ticks, sum, spun.Load() > 0, runtime.GOMAXPROCS(0))
}
