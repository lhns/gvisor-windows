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
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// The platform's part of the signal latency trace (GVISOR_SIGLAT, see
// pkg/sentry/kernel/siglat.go and docs/results/3e-signal-latency.md): how each
// Switch went (did either side sleep, how long the round trip took) for the
// kernel's trace, and every interrupt that took longer than the threshold, with
// interrupt's refusals by reason.
var sigLatOn, sigLatMin = func() (bool, int64) {
	v := os.Getenv("GVISOR_SIGLAT")
	if v == "" || v == "0" {
		return false, 0
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms <= 1 {
		ms = 20
	}
	return true, int64(ms) * 1e6
}()

// sigLatSwitch is one context's account of its current or last Switch.
type sigLatSwitch struct {
	t0, t1    int64 // Switch resumed the thread; the request arrived (qpc ns)
	stubSlept bool  // the stub slept on its reply event (SetEvent needed)
	ctlSlept  bool  // the Sentry side slept on the request event
	stale     bool  // an interrupt was still pending from the last Switch
	early     bool  // Switch returned at once: the interrupt came before it
	intrAt    int64 // when the interrupt this Switch answered was raised (qpc ns)
	refusals  [nRefuse]int
	kind      int
}

var sigLatStats struct {
	interrupts, over, stale atomic.Int64
	maxNs                   atomic.Int64
	refusals                [nRefuse]atomic.Int64
}

// SigLatInfo implements kernel.SigLatPlatformInfo.
func (c *winContext) SigLatInfo() string {
	s := &c.sl
	r := ""
	for i, n := range s.refusals {
		if n > 0 {
			r += fmt.Sprintf(" %s:%d", refuseNames[i], n)
		}
	}
	if r == "" {
		r = " none"
	}
	if s.early {
		return fmt.Sprintf("switch refused at entry (interrupt already forwarded), qpc %.3f ms", float64(s.t0)/1e6)
	}
	return fmt.Sprintf("switch from qpc %.3f to %.3f ms (request kind %d), interrupt raised at qpc %.3f ms, stub slept %v, sentry slept %v, stale interrupt %v, refusals%s",
		float64(s.t0)/1e6, float64(s.t1)/1e6, s.kind, float64(s.intrAt)/1e6, s.stubSlept, s.ctlSlept, s.stale, r)
}

// sigLatInterrupted records an interrupt request that arrived at now.
func (c *winContext) sigLatInterrupted(now int64) {
	at := c.intrAt.Swap(0)
	if at == 0 {
		return
	}
	c.sl.intrAt = at
	d := now - at
	sigLatStats.interrupts.Add(1)
	for {
		m := sigLatStats.maxNs.Load()
		if d <= m || sigLatStats.maxNs.CompareAndSwap(m, d) {
			break
		}
	}
	if d >= sigLatMin {
		sigLatStats.over.Add(1)
		fmt.Fprintf(os.Stderr, "SIGLAT winstub interrupt took %.2f ms: %s; qpc %.3f ms, %s UTC\n",
			float64(d)/1e6, c.SigLatInfo(), float64(now)/1e6, time.Now().UTC().Format("15:04:05.000"))
	}
}

// DumpSigLat prints the platform's interrupt counters.
func DumpSigLat(w io.Writer) {
	fmt.Fprintf(w, "SIGLAT winstub: %d interrupts, %d over %d ms, max %.2f ms, %d stale at Switch entry, refusals:",
		sigLatStats.interrupts.Load(), sigLatStats.over.Load(), sigLatMin/1e6, float64(sigLatStats.maxNs.Load())/1e6, sigLatStats.stale.Load())
	for i := 1; i < nRefuse; i++ {
		fmt.Fprintf(w, " %s:%d", refuseNames[i], sigLatStats.refusals[i].Load())
	}
	fmt.Fprintln(w)
}
