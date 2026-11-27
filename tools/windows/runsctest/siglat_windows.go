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
	siglatBoots = flag.Int("siglat-boots", 10, "siglat: fresh sandboxes running the whole clocks guest")
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
	// a try of the timeouts group's EINTR check that missed (the check prints no within line then)
	missRe = regexp.MustCompile(`^  epoll_wait interrupted by a timer: try \d+ took (-?[\d.]+) ms`)
)

// testSiglat (dispatch only, -only siglat) measures timer signal latency without the
// retry of the timeouts group: fresh sandboxes running the whole clocks guest, then
// sandboxes looping the timeouts and siglat groups, with the Sentry's trace
// (GVISOR_SIGLAT) and stall watches in runsc and in this process. It prints the
// latency distribution per kind and every outlier with what the traces saw around
// it (docs/results/3e-signal-latency.md). Report only: it fails only if a guest
// did not run to its end.
func testSiglat() error {
	os.Setenv("GVISOR_SIGLAT", "1")
	defer os.Unsetenv("GVISOR_SIGLAT")
	var host lockedBuf
	w := stallwatch.Start("runsctest", &host, 20*time.Millisecond)
	samples := map[string][]float64{}
	var notes []string
	type run struct {
		name string
		args []string
	}
	var runs []run
	for i := 0; i < *siglatBoots; i++ {
		runs = append(runs, run{fmt.Sprintf("boot%d", i), []string{"/bin/clocks", "noretry"}})
	}
	for i := 0; i < *siglatLoops; i++ {
		runs = append(runs, run{fmt.Sprintf("loop%d", i), []string{"/bin/clocks", "noretry", fmt.Sprintf("repeat=%d", *siglatRep), "timeouts", "siglat"}})
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
			} else if m := missRe.FindStringSubmatch(l); m != nil {
				v, _ := strconv.ParseFloat(m[1], 64)
				samples["timeouts: epoll_wait EINTR"] = append(samples["timeouts: epoll_wait EINTR"], v-30)
				notes = append(notes, rn.name+": "+l)
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
	if len(bad) > 0 {
		return fmt.Errorf("siglat: %s", strings.Join(bad, "; "))
	}
	return nil
}
