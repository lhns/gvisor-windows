// stubproto: Phase 1a prototype of the Windows stub/controller design (ADR 0010, 0011).
//
//	stubproto mkstub <out.exe>
//	stubproto run [-v] <static-linux-elf> [args...]
//	stubproto suite -dir <guest dir>
//	stubproto probe layout|unmap|interrupt|threads -dir <guest dir>
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"stubproto/pe"
	"stubproto/stubasm"
)

func init() { runtime.LockOSThread() } // the debugger must stay on the thread that created the stubs

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: stubproto mkstub|run|suite|probe ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "mkstub":
		img := pe.Stub(stubasm.CtlBase, stubasm.StubCode+stubasm.OffThreadEnt)
		check(os.WriteFile(os.Args[2], img, 0o755))
	case "run":
		os.Exit(cmdRun(os.Args[2:]))
	case "suite":
		os.Exit(cmdSuite(os.Args[2:]))
	case "probe":
		os.Exit(cmdProbe(os.Args[2:]))
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "stubproto:", err)
		os.Exit(1)
	}
}

func tempStub() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("stubproto-stub-%d.exe", os.Getpid()))
}

const memFileSize = 512 << 20

func cmdRun(args []string) int {
	verbose := false
	if len(args) > 0 && args[0] == "-v" {
		verbose, args = true, args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "run: need a guest")
		return 2
	}
	stub := tempStub()
	defer os.Remove(stub)
	c, err := newCtl(stub, memFileSize)
	check(err)
	c.verbose = verbose
	c.trace = os.Getenv("STUBPROTO_TRACE") != ""
	exit, _, err := c.runGuest(args[0], args[1:], nil, 2*time.Minute)
	check(err)
	if verbose {
		c.report(os.Stderr)
	}
	return exit
}

// runGuest runs one guest to completion and returns its exit status and stdout.
func (c *Ctl) runGuest(path string, args []string, tee io.Writer, limit time.Duration) (int, string, error) {
	var buf bytes.Buffer
	c.stdout = &buf
	if tee != nil {
		c.stdout = io.MultiWriter(&buf, tee)
	}
	c.resetRun()
	p, err := c.spawnMain(path, append([]string{filepath.Base(path)}, args...))
	if err != nil {
		return 0, "", err
	}
	err = c.run(p, limit)
	exit := p.exitCode
	if p.termSig != 0 {
		exit = 128 + p.termSig
	}
	c.mainDur = p.firstDur
	c.mainStats = p.stats
	c.cleanupAll()
	return exit, buf.String(), err
}

func (c *Ctl) resetRun() {
	c.ev = map[string]int{}
	c.stamps = map[string][]int64{}
	c.times = map[string]time.Duration{}
	c.sysCount = map[uint64]int{}
	c.forkDurs = nil
	c.udServed, c.patchedServed = 0, 0
	c.pendings = nil
	futexQ = nil
	c.probeAddr = 0
	c.holdSamples = nil
	c.runDelta = nil
}

func (c *Ctl) report(w io.Writer) {
	var ks []string
	for k := range c.ev {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var parts []string
	total := 0
	for _, k := range ks {
		parts = append(parts, fmt.Sprintf("%s=%d", k, c.ev[k]))
		total += c.ev[k]
	}
	fmt.Fprintf(w, "INFO debug events: total=%d %s\n", total, strings.Join(parts, " "))
	s := c.mainStats
	fmt.Fprintf(w, "INFO sites (main exe): syscall insts=%d patched=%d (imm32=%d imm64=%d reloc=%d of which xor=%d) ud2=%d (no window=%d, branch target=%d, non-relocatable=%d, no veneer=%d), 0f05 byte pairs=%d, pairs not decoded as syscall=%d\n",
		s.syscalls, s.patched, s.imm32, s.imm64, s.reloc, s.relocXor, s.ud2, s.noWindow, s.onTarget, s.nonReloc, s.noVeneer, s.pairs, s.nonSyscal)
	tot := c.patchedServed + c.udServed
	fmt.Fprintf(w, "INFO syscalls served: via patched jump=%d via ud2=%d (ud2 share %.2f%%); start to first syscall=%v\n", c.patchedServed, c.udServed, 100*float64(c.udServed)/float64(max(tot, 1)), c.mainDur)
}

func (c *Ctl) cleanupAll() {
	c.mu.Lock()
	for _, p := range c.procs {
		for _, t := range p.threads {
			t.die()
		}
	}
	c.mu.Unlock()
	c.wg.Wait()
	for _, p := range c.procs {
		if !p.exited {
			p.exited = true
		}
		if !p.dead {
			p.kill()
		}
		p.cleanup()
	}
	c.procs = map[uint32]*Proc{}
	c.threads = map[uint32]*Thread{}
	c.live = nil
}

// ---------------------------------------------------------------- suite

type guestSpec struct {
	name     string
	file     string
	args     []string
	out      string
	exit     int
	required bool
}

var guests = []guestSpec{
	{"a-asm-hello", "hello_asm", nil, "hello from asm\nud2 path ok\n", 0, true},
	{"b-glibc-static", "hello_glibc", nil, "hello from glibc: argc=1 tv=5 work=10 heap=ok\n", 7, true},
	{"b2-glibc-static-pie", "hello_glibc_pie", nil, "hello from glibc: argc=1 tv=5 work=10 heap=ok\n", 7, true},
	{"c-glibc-fork-wait", "fork_glibc", nil, "child\nparent: child exited 3\n", 0, true},
	{"f-jit-wx", "jit_glibc", nil, "jit1 ok\njit2 ok\njit3 ok\n", 0, true},
	{"d-go-hello", "hello_go", nil, "hello from go\n", 0, false},
	{"e-asm-threads", "threads_asm", nil, "threads done\n", 0, false},
}

func stats(v []int64) (med, p99, mean float64) {
	var d []float64
	for i := 1; i < len(v); i++ {
		if x := float64(v[i] - v[i-1]); x < 5e6 {
			d = append(d, x)
		}
	}
	if len(d) == 0 {
		return
	}
	sort.Float64s(d)
	var sum float64
	for _, x := range d {
		sum += x
	}
	return d[len(d)/2], d[len(d)*99/100], sum / float64(len(d))
}

func cmdSuite(args []string) int {
	dir := "."
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-dir" {
			dir = args[i+1]
		}
	}
	stub := tempStub()
	defer os.Remove(stub)
	c, err := newCtl(stub, memFileSize)
	check(err)
	fmt.Printf("INFO cpu FSGSBASE=%v GOMAXPROCS=%d cpus=%d\n", stubasm.HasFSGSBASE(), runtime.GOMAXPROCS(0), runtime.NumCPU())
	failed := 0
	var sites siteStats
	for _, g := range guests {
		path := filepath.Join(dir, g.file)
		if _, err := os.Stat(path); err != nil {
			fmt.Printf("SKIP %s: %s missing\n", g.name, g.file)
			continue
		}
		t0 := time.Now()
		exit, out, err := c.runGuest(path, g.args, nil, 60*time.Second)
		ok := err == nil && exit == g.exit && out == g.out
		status := "PASS"
		if !ok {
			status = "FAIL"
			if g.required {
				failed++
			}
		}
		fmt.Printf("%s %s: exit=%d (want %d) out=%q (want %q) err=%v elapsed=%v required=%v\n", status, g.name, exit, g.exit, out, g.out, err, time.Since(t0).Round(time.Millisecond), g.required)
		c.report(os.Stdout)
		sites.add(c.mainStats)
	}
	fmt.Printf("INFO sites over all guests: syscall insts=%d patched=%d ud2=%d, 0f05 byte pairs=%d, mid-instruction pairs=%d\n",
		sites.syscalls, sites.patched, sites.ud2, sites.pairs, sites.nonSyscal)

	// measurements
	if p := filepath.Join(dir, "bench_asm"); fileExists(p) {
		c.benchOn = true
		exit, out, err := c.runGuest(p, nil, nil, 120*time.Second)
		c.benchOn = false
		fmt.Printf("INFO bench exit=%d out=%q err=%v\n", exit, out, err)
		m, p99, mean := stats(c.stamps["patched"])
		fmt.Printf("MEASURE patched-syscall round trip (getpid x%d): median=%.2f us p99=%.2f us mean=%.2f us\n", len(c.stamps["patched"]), m/1e3, p99/1e3, mean/1e3)
		m, p99, mean = stats(c.stamps["ud2"])
		fmt.Printf("MEASURE ud2/debugger-path syscall (getppid x%d): median=%.1f us p99=%.1f us mean=%.1f us\n", len(c.stamps["ud2"]), m/1e3, p99/1e3, mean/1e3)
		c.report(os.Stdout)
	}
	if p := filepath.Join(dir, "hello_asm"); fileExists(p) {
		var d []float64
		for i := 0; i < 20; i++ {
			c.runGuest(p, nil, nil, 30*time.Second)
			d = append(d, float64(c.mainDur.Microseconds()))
		}
		sort.Float64s(d)
		fmt.Printf("MEASURE process start (CreateProcess to first syscall served), 20 runs: median=%.1f ms min=%.1f ms max=%.1f ms\n", d[len(d)/2]/1e3, d[0]/1e3, d[len(d)-1]/1e3)
	}
	if p := filepath.Join(dir, "fork_glibc"); fileExists(p) {
		var d, f []float64
		for i := 0; i < 10; i++ {
			c.runGuest(p, nil, nil, 30*time.Second)
			for _, x := range c.forkDurs {
				d = append(d, float64(x.Microseconds()))
			}
			f = append(f, float64(c.times["fork-call"].Microseconds()))
		}
		sort.Float64s(d)
		sort.Float64s(f)
		if len(d) > 0 {
			fmt.Printf("MEASURE fork (request to child's first syscall), %d runs: median=%.1f ms min=%.1f ms max=%.1f ms; controller time inside fork: median=%.1f ms\n",
				len(d), d[len(d)/2]/1e3, d[0]/1e3, d[len(d)-1]/1e3, f[len(f)/2]/1e3)
		}
	}
	if p := filepath.Join(dir, "hello_go"); fileExists(p) {
		var rep, lazy, tot []int
		ok := 0
		for i := 0; i < 10; i++ {
			exit, out, _ := c.runGuest(p, nil, nil, 30*time.Second)
			if exit == 0 && out == "hello from go\n" {
				ok++
			}
			rep = append(rep, c.ev["EXCEPTION:av-fs-repair"])
			lazy = append(lazy, c.ev["EXCEPTION:av-lazy-map"])
			n := 0
			for _, v := range c.ev {
				n += v
			}
			tot = append(tot, n)
		}
		fmt.Printf("MEASURE go guest x10: ok=%d, %%fs repairs per run %v, lazy-map events per run %v, all debug events per run %v\n", ok, rep, lazy, tot)
	}
	fmt.Printf("INFO memory file peak pages=%d (%d MiB)\n", c.mf.peak, c.mf.peak*4096>>20)
	if failed > 0 {
		fmt.Printf("RESULT FAIL: %d required guests failed\n", failed)
		return 1
	}
	fmt.Println("RESULT PASS: all required guests passed")
	return 0
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
