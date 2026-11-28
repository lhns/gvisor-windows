// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package winrunsc

import (
	"io"
	"os"
	"sync"
)

// asyncLog writes the debug log from its own goroutine. The Sentry logs while it
// holds locks (a timer signal's send logs "Notified of signal" under the signal
// mutex; the timekeeper logs while it rewrites the vDSO's clock parameters), and
// a write to the log file on a CI runner stalled for up to 2.6 s, which delayed
// timer signals by as much (docs/results/3e-signal-latency.md). Write queues the
// line and returns; it waits only while asyncLogMax bytes are queued. Close
// writes out what is queued. Lines still queued when the process dies without
// Close (a panic) are lost.
type asyncLog struct {
	w    io.Writer
	mu   sync.Mutex
	more sync.Cond // lines were queued, or the log was closed
	room sync.Cond // the writer took lines off the queue
	q    [][]byte
	n    int // bytes in q and being written
	shut bool
	done chan struct{}
}

const asyncLogMax = 8 << 20

func newAsyncLog(w io.Writer) *asyncLog {
	a := &asyncLog{w: w, done: make(chan struct{})}
	a.more.L, a.room.L = &a.mu, &a.mu
	go a.run()
	return a
}

// Write implements io.Writer. log.Writer passes one whole line per call, which
// stays one write to the file (sandboxes may share a log file).
func (a *asyncLog) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for a.n >= asyncLogMax && !a.shut {
		a.room.Wait()
	}
	if a.shut {
		return 0, os.ErrClosed
	}
	a.q = append(a.q, append([]byte(nil), p...))
	a.n += len(p)
	a.more.Signal()
	return len(p), nil
}

func (a *asyncLog) run() {
	defer close(a.done)
	a.mu.Lock()
	for {
		for len(a.q) == 0 && !a.shut {
			a.more.Wait()
		}
		if len(a.q) == 0 {
			a.mu.Unlock()
			return
		}
		q := a.q
		a.q = nil
		a.mu.Unlock()
		for _, p := range q {
			a.w.Write(p) // the inner reportingWriter reports a failing log
		}
		a.mu.Lock()
		for _, p := range q {
			a.n -= len(p)
		}
		a.room.Broadcast()
	}
}

// Close writes out the queued lines; later writes fail.
func (a *asyncLog) Close() {
	a.mu.Lock()
	a.shut = true
	a.more.Signal()
	a.room.Broadcast()
	a.mu.Unlock()
	<-a.done
}
