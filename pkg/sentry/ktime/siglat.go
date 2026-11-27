// gvisor-windows: timer lateness for the signal latency trace (GVISOR_SIGLAT,
// pkg/sentry/kernel/siglat.go, docs/results/3e-signal-latency.md).

package ktime

import "sync/atomic"

// TraceLate makes SampledTimer.tick record how late it ran, in ns of the timer's
// clock (now minus the expiry it ran for), before it calls the listener.
var TraceLate bool

var lastLate atomic.Int64

// LastLate returns the lateness of the most recent SampledTimer tick, for a
// listener to read from NotifyTimer.
func LastLate() int64 { return lastLate.Load() }
