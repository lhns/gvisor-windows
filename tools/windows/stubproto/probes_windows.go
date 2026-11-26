package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
	"unsafe"
)

func cmdProbe(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "probe: layout|unmap|interrupt|threads")
		return 2
	}
	dir := "."
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "-dir" {
			dir = args[i+1]
		}
	}
	stub := tempStub()
	defer os.Remove(stub)
	c, err := newCtl(stub, 64<<20)
	check(err)
	switch args[0] {
	case "layout":
		probeLayout(c, 50)
	case "unmap":
		probeUnmap(c)
	case "interrupt":
		probeInterrupt(c, filepath.Join(dir, "spin_asm"))
	case "threads":
		probeThreads(c, filepath.Join(dir, "threads_asm"))
	}
	return 0
}

// probeLayout starts n stubs and reports what Windows maps inside the guest range.
func probeLayout(c *Ctl, n int) {
	type key struct{ lo, hi uint64 }
	count := map[key]int{}
	var lows []uint64
	t60, t400, tCtl, win := 0, 0, 0, 0
	for i := 0; i < n; i++ {
		p, err := c.newProc("layout")
		if err != nil {
			fmt.Printf("FAIL layout: start %d: %v\n", i, err)
			return
		}
		for _, r := range p.used {
			count[key{r.lo, r.hi}]++
			if r.lo < 0x7000_0000_0000 {
				lows = append(lows, r.lo)
			}
			hit := func(lo, hi uint64) bool { return r.lo < hi && r.hi > lo }
			if hit(0xB0000, 0xE1000) {
				t60++
			}
			if hit(0x400000, 0x10000000) {
				t400++
			}
			if hit(0x6FFF_8000_0000, 0x6FFF_8020_0000) {
				tCtl++
			}
			if hit(winWindow.lo, winWindow.hi) {
				win++
			}
		}
		if i == 0 {
			fmt.Printf("INFO layout start 0: reserved %d GiB of the guest range in %d gaps\n", p.reserved>>30, len(p.as.gaps))
		}
		p.kill()
		p.cleanup()
	}
	fmt.Printf("INFO layout over %d stub starts: Windows regions hitting the stub area 0xB0000-0xE1000: %d, 0x400000-0x10000000: %d, control area: %d, the unreserved Windows window: %d\n", n, t60, t400, tCtl, win)
	var fixed, varying []key
	for k, v := range count {
		if v == n {
			fixed = append(fixed, k)
		} else {
			varying = append(varying, k)
		}
	}
	sort.Slice(fixed, func(i, j int) bool { return fixed[i].lo < fixed[j].lo })
	for _, k := range fixed {
		fmt.Printf("INFO layout fixed region in all %d starts: [%#x,%#x) %d KiB\n", n, k.lo, k.hi, (k.hi-k.lo)>>10)
	}
	sort.Slice(lows, func(i, j int) bool { return lows[i] < lows[j] })
	if len(varying) > 0 {
		sort.Slice(varying, func(i, j int) bool { return varying[i].lo < varying[j].lo })
		fmt.Printf("INFO layout %d distinct varying regions (randomised); lowest start %#x, highest start %#x\n", len(varying), varying[0].lo, varying[len(varying)-1].lo)
		for i, k := range varying {
			if i >= 12 {
				break
			}
			fmt.Printf("INFO layout sample varying region [%#x,%#x) seen %d times\n", k.lo, k.hi, count[k])
		}
	}
}

// probeUnmap answers whether a 4 KB view can be removed from the middle of a larger view.
func probeUnmap(c *Ctl) {
	p, err := c.newProc("unmap")
	if err != nil {
		fmt.Println("FAIL unmap:", err)
		return
	}
	defer func() { p.kill(); p.cleanup() }()
	var base uint64
	for _, g := range p.as.gaps {
		if g.hi-g.lo >= 1<<20 {
			base = g.lo
			break
		}
	}
	r, _, e := pVirtualFreeEx.Call(p.h, uintptr(base), 0x10000, memRelease|memPreservePH)
	fmt.Printf("INFO unmap: split 64 KB placeholder: ok=%v %v\n", r != 0, e)
	r, _, e = pMapViewOfFile3.Call(c.mf.sec, p.h, uintptr(base), 0, 0x10000, memReplacePH, pageReadWrite, 0, 0)
	fmt.Printf("INFO unmap: 64 KB view mapped: ok=%v %v\n", r != 0, e)
	r, _, e = pUnmapViewOfFile2.Call(p.h, uintptr(base+0x4000), memPreservePH)
	fmt.Printf("RESULT unmap: UnmapViewOfFile2(view+0x4000, PRESERVE_PLACEHOLDER) in the middle of a 64 KB view: ok=%v %v\n", r != 0, e)
	r, _, e = pVirtualFreeEx.Call(p.h, uintptr(base+0x4000), 0x1000, memRelease|memPreservePH)
	fmt.Printf("RESULT unmap: VirtualFreeEx(view+0x4000, 4 KB, RELEASE|PRESERVE_PLACEHOLDER) on a mapped view: ok=%v %v\n", r != 0, e)
	r, _, e = pVirtualProtectEx.Call(p.h, uintptr(base+0x4000), 0x1000, pageNoAccess, uintptr(unsafe.Pointer(new(uint32))))
	fmt.Printf("INFO unmap: VirtualProtectEx of one 4 KB page inside the view: ok=%v %v\n", r != 0, e)
	r, _, e = pUnmapViewOfFile2.Call(p.h, uintptr(base), memPreservePH)
	fmt.Printf("INFO unmap: UnmapViewOfFile2(whole view): ok=%v %v\n", r != 0, e)
	fmt.Println("INFO unmap: the prototype therefore maps guest memory as one view per 4 KB page")
	// cost of the per-page scheme
	const n = 2000
	as := p.as
	t0 := time.Now()
	for i := uint64(0); i < n; i++ {
		as.mapPage(base+0x20000+i*pageSize, 3)
	}
	d := time.Since(t0)
	t1 := time.Now()
	for i := uint64(0); i < n; i++ {
		as.unmapPage(base + 0x20000 + i*pageSize)
	}
	fmt.Printf("MEASURE per-4KB-page map (split+MapViewOfFile3): %.1f us, unmap: %.1f us (n=%d)\n", float64(d.Microseconds())/n, float64(time.Since(t1).Microseconds())/n, n)
}

// probeInterrupt measures interrupting a running guest thread from the controller.
func probeInterrupt(c *Ctl, guest string) {
	c.resetRun()
	p, err := c.spawnMain(guest, []string{"spin"})
	if err != nil {
		fmt.Println("FAIL interrupt:", err)
		return
	}
	c.mainProc = p
	c.pump(150 * time.Millisecond)
	t := p.threads[0]
	var susp, full, resume []float64
	for i := 0; i < 300; i++ {
		t0 := time.Now()
		pSuspendThread.Call(t.h)
		t1 := time.Now()
		c.getCtx(t.h, ctxControlInt)
		t2 := time.Now()
		pResumeThread.Call(t.h)
		t3 := time.Now()
		susp = append(susp, float64(t1.Sub(t0).Nanoseconds()))
		full = append(full, float64(t3.Sub(t0).Nanoseconds()))
		resume = append(resume, float64(t3.Sub(t2).Nanoseconds()))
		c.pump(time.Millisecond)
	}
	sort.Float64s(susp)
	sort.Float64s(full)
	sort.Float64s(resume)
	fmt.Printf("MEASURE interrupt running guest thread (300x): SuspendThread median=%.1f us p99=%.1f us; Suspend+GetThreadContext+Resume median=%.1f us p99=%.1f us; Resume alone median=%.1f us\n",
		susp[150]/1e3, susp[297]/1e3, full[150]/1e3, full[297]/1e3, resume[150]/1e3)
	c.report(os.Stdout)
	c.killAll()
	c.cleanupAll()
}

// probeThreads checks that a pending debug event stops every thread of the debuggee.
func probeThreads(c *Ctl, guest string) {
	c.resetRun()
	c.holdProbe = true
	exit, out, err := c.runGuest(guest, nil, nil, 60*time.Second)
	fmt.Printf("INFO threads: exit=%d out=%q err=%v\n", exit, out, err)
	var zero, moved int
	for _, d := range c.holdSamples {
		if d == 0 {
			zero++
		} else {
			moved++
		}
	}
	fmt.Printf("RESULT threads: while a debug event was held for 20 ms, the second guest thread's counter advanced in %d of %d holds (0 advanced = all threads stopped)\n", moved, len(c.holdSamples))
	fmt.Printf("RESULT threads: counter advance during a 100 ms nanosleep served over shared memory (no debug event pending): %v\n", c.runDelta)
	c.report(os.Stdout)
}
