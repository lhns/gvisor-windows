// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/stallwatch"
)

var (
	siglatBoots = flag.Int("siglat-boots", 20, "siglat: fresh sandboxes running the whole clocks guest")
	siglatLoops = flag.Int("siglat-loops", 5, "siglat: sandboxes running the timeouts and siglat groups -siglat-repeat times")
	siglatRep   = flag.Int("siglat-repeat", 40, "siglat: repetitions per loop sandbox")
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

var (
	withinRe = regexp.MustCompile(`^  (\S.*?)\s+(-?[\d.]+) ms \(want ([\d.]+)\)$`)
	sampleRe = regexp.MustCompile(`^siglat (\w+) (-?[\d.]+) ([\d.]+)$`)
	// the window of a traced signal: from the timer's expiry to the handler's return
	windowRe = regexp.MustCompile(`total ([\d.]+) ms: timer late (-?[\d.]+),.* fire qpc ([\d.]+) ms, qpc ([\d.]+) ms`)
)

// testSiglat (dispatch only, -only siglat) measures timer signal latency: fresh
// sandboxes running the whole clocks guest, then
// sandboxes looping the timeouts and siglat groups, with the Sentry's trace
// (GVISOR_SIGLAT) and stall watches in runsc and in this process. It prints the
// latency distribution per kind and every outlier with what the traces saw around
// it (docs/results/3e-signal-latency.md). Report only: it fails only if a guest
// did not run to its end.
func testSiglat() error {
	// Trace every timer signal later than 5 ms.
	os.Setenv("GVISOR_SIGLAT", "5")
	defer os.Unsetenv("GVISOR_SIGLAT")
	var host lockedBuf
	w := stallwatch.Start("runsctest", &host, 20*time.Millisecond, true)
	type window struct {
		note       string
		start, end float64 // qpc ms
	}
	var windows []window
	samples := map[string][]float64{}
	var notes []string
	type run struct {
		name string
		args []string
	}
	var runs []run
	for i := 0; i < *siglatBoots; i++ {
		runs = append(runs, run{fmt.Sprintf("boot%d", i), []string{"/bin/clocks"}})
	}
	for i := 0; i < *siglatLoops; i++ {
		runs = append(runs, run{fmt.Sprintf("loop%d", i), []string{"/bin/clocks", fmt.Sprintf("repeat=%d", *siglatRep), "timeouts", "siglat"}})
	}
	var bad []string
	for _, rn := range runs {
		start := time.Now()
		r, err := runsc("siglat", rn.args, nil)
		_ = os.WriteFile(filepath.Join(*workDir, "logs", "siglat-"+rn.name+".out"), []byte(r.stdout+"\n----- stderr\n"+r.stderr), 0o644)
		fmt.Printf("siglat %s: exit %d in %v\n", rn.name, r.code, time.Since(start).Round(time.Millisecond))
		if err != nil || !(strings.Contains(r.stdout, "clocks ok") || strings.Contains(r.stdout, "checks failed")) {
			bad = append(bad, fmt.Sprintf("%s: %v exit %d, stderr %q", rn.name, err, r.code, r.stderr))
			continue
		}
		prev := ""
		for _, l := range strings.Split(strings.ReplaceAll(r.stdout, "\r", ""), "\n") {
			if m := withinRe.FindStringSubmatch(l); m != nil {
				got, _ := strconv.ParseFloat(m[2], 64)
				want, _ := strconv.ParseFloat(m[3], 64)
				kind := "waits and sleeps"
				switch n := m[1]; {
				case n == "epoll_wait interrupted by a timer":
					kind = "timeouts: epoll_wait EINTR"
				case strings.Contains(n, "spinning"):
					kind = "cpu-time timers (10ms tick)"
				case strings.Contains(n, "ITIMER_REAL") || strings.Contains(n, "timer_create") || strings.Contains(n, "interrupt") || n == "alarm(1)":
					kind = "other timer signals"
				}
				samples[kind] = append(samples[kind], got-want)
			} else if m := sampleRe.FindStringSubmatch(l); m != nil {
				v, _ := strconv.ParseFloat(m[2], 64)
				samples["siglat "+m[1]] = append(samples["siglat "+m[1]], v)
				if v > 5 {
					notes = append(notes, rn.name+": "+l)
				}
			} else if strings.HasPrefix(l, "FAIL") || strings.HasPrefix(l, "  (late") {
				notes = append(notes, rn.name+": "+strings.TrimSpace(prev)+" "+strings.TrimSpace(l))
			}
			prev = l
		}
		for _, l := range strings.Split(strings.ReplaceAll(r.stderr, "\r", ""), "\n") {
			if strings.HasPrefix(l, "SIGLAT") || strings.HasPrefix(l, "STALL") {
				notes = append(notes, rn.name+": "+l)
			}
			if m := windowRe.FindStringSubmatch(l); m != nil {
				late, _ := strconv.ParseFloat(m[2], 64)
				fire, _ := strconv.ParseFloat(m[3], 64)
				end, _ := strconv.ParseFloat(m[4], 64)
				windows = append(windows, window{fmt.Sprintf("%s: %s ms signal", rn.name, m[1]), fire - max(late, 0), end})
			}
		}
	}
	w.Stop()
	fmt.Println("siglat: latency past the requested time, ms (guest CLOCK_MONOTONIC)")
	fmt.Printf("  %-28s %6s %8s %8s %8s %8s %8s %8s %5s %5s\n", "kind", "n", "min", "p50", "p90", "p99", "p99.9", "max", ">5", ">20")
	var kinds []string
	for k := range samples {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		v := samples[k]
		sort.Float64s(v)
		q := func(p float64) float64 { return v[min(len(v)-1, int(p*float64(len(v))))] }
		n5, n20 := 0, 0
		for _, x := range v {
			if x > 5 {
				n5++
			}
			if x > 20 {
				n20++
			}
		}
		fmt.Printf("  %-28s %6d %8.2f %8.2f %8.2f %8.2f %8.2f %8.2f %5d %5d\n", k, len(v), v[0], q(0.5), q(0.9), q(0.99), q(0.999), v[len(v)-1], n5, n20)
	}
	fmt.Println("siglat: outliers (> 5 ms) and the traces (SIGLAT: Sentry and platform, STALL: heartbeats in runsc and here):")
	for _, n := range notes {
		fmt.Println("  " + n)
	}
	fmt.Print(strings.ReplaceAll(host.b.String(), "STALL", "  host: STALL"))
	// Line every traced late signal up with the host's heartbeats (one per processor,
	// pinned, plus the process's own): a gap on one processor inside the window that
	// matches the delay points at that processor, no gap anywhere at our code.
	gaps := w.Gaps()
	fmt.Printf("siglat: heartbeat gaps (>= %v) of this process overlapping each traced signal's window:\n", stallwatch.KeepGap)
	for _, wd := range windows {
		var hit []string
		for _, g := range gaps {
			a, b := float64(g.Start)/1e6, float64(g.End)/1e6
			if b > wd.start && a < wd.end {
				hit = append(hit, fmt.Sprintf("%s %.2f ms (qpc %.3f..%.3f)", g.Kind, b-a, a, b))
			}
		}
		if len(hit) == 0 {
			hit = []string{"none"}
		}
		fmt.Printf("  %s, qpc %.3f..%.3f: %s\n", wd.note, wd.start, wd.end, strings.Join(hit, ", "))
	}
	if len(bad) > 0 {
		return fmt.Errorf("siglat: %s", strings.Join(bad, "; "))
	}
	return nil
}
