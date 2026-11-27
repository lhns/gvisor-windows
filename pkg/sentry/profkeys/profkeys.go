// Package profkeys counts events by a free-form key for profiling (GVISOR_PROFSTATS=1).
package profkeys

import (
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
)

// On is true if GVISOR_PROFSTATS is set.
var On = os.Getenv("GVISOR_PROFSTATS") != ""

type stat struct{ n, ns atomic.Int64 }

var (
	mu   sync.Mutex
	keys = map[string]*stat{}
)

// Count counts one event under key, with the time it took in ns.
func Count(key string, ns int64) {
	mu.Lock()
	st := keys[key]
	if st == nil {
		st = &stat{}
		keys[key] = st
	}
	mu.Unlock()
	st.n.Add(1)
	st.ns.Add(ns)
}

// Now returns a high-resolution clock in ns.
func Now() int64 { return now() }

// Dump writes the counters sorted by key.
func Dump(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(w, "PROFSTATS key %-60s %9d %10.1f ms\n", k, keys[k].n.Load(), float64(keys[k].ns.Load())/1e6)
	}
}
