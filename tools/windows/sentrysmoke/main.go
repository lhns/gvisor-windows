// Command sentrysmoke exercises the Windows host layers of the Sentry that
// exist so far: random numbers, host thread IDs, cpuid, the Sentry's own
// clocks, pkg/safecopy fault recovery and the pgalloc MemoryFile on sections.
// It prints one PASS, FAIL or INFO line per probe and exits non-zero if any
// probe failed. It must run on Windows (CI: .github/workflows/windows-sentry.yml).
package main

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/hosttid"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/rand"
	"gvisor.dev/gvisor/pkg/safecopy"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	ktime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

var failed bool

func report(ok bool, name, format string, args ...any) {
	s := "PASS"
	if !ok {
		s = "FAIL"
		failed = true
	}
	fmt.Printf("%s %s: %s\n", s, name, fmt.Sprintf(format, args...))
}

func info(name, format string, args ...any) {
	fmt.Printf("INFO %s: %s\n", name, fmt.Sprintf(format, args...))
}

// probe runs f and reports a panic as a failure instead of dying.
func probe(name string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			report(false, name, "panic: %v", r)
		}
	}()
	f()
}

func main() {
	info("env", "%s/%s %s GOMAXPROCS=%d", runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.GOMAXPROCS(0))
	probe("rand", probeRand)
	probe("hosttid", probeHostTID)
	probe("cpuid", probeCPUID)
	probe("time", probeTime)
	probe("safecopy", probeSafecopy)
	probe("pgalloc", probePgalloc)
	probe("release", probeRelease)
	if failed {
		os.Exit(1)
	}
}

func probeRand() {
	a, b := make([]byte, 32), make([]byte, 32)
	n1, err1 := rand.Read(a)
	n2, err2 := rand.Read(b)
	report(err1 == nil && err2 == nil && n1 == 32 && n2 == 32 && !bytes.Equal(a, b) && !bytes.Equal(a, make([]byte, 32)), "rand.Read", "two distinct non-zero reads (%v, %v)", err1, err2)
	report(rand.Init() == nil, "rand.Init", "no-op")
}

func probeHostTID() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	got, want := hosttid.Current(), uint64(windows.GetCurrentThreadId())
	report(got == want, "hosttid.Current", "got %d, GetCurrentThreadId %d", got, want)
}

func probeCPUID() {
	cpuid.Initialize()
	fs := cpuid.HostFeatureSet()
	info("cpuid", "UseXsave=%v UseXsaveopt=%v UseXsavec=%v", fs.UseXsave(), fs.UseXsaveopt(), fs.UseXsavec())
	report(true, "cpuid.Initialize", "completed")
}

func probeTime() {
	c := ktime.NewCalibratedClocks(false)
	c.Update(false)
	m1, err1 := c.GetTime(ktime.Monotonic)
	time.Sleep(20 * time.Millisecond)
	c.Update(false)
	m2, err2 := c.GetTime(ktime.Monotonic)
	report(err1 == nil && err2 == nil && m2 > m1 && m2-m1 < int64(5*time.Second), "time.Monotonic", "advanced %d ns over a 20 ms sleep (%v, %v)", m2-m1, err1, err2)
	r, err := c.GetTime(ktime.Realtime)
	diff := time.Duration(r - time.Now().UnixNano())
	if diff < 0 {
		diff = -diff
	}
	report(err == nil && diff < 2*time.Second, "time.Realtime", "differs from time.Now by %v (%v)", diff, err)
	info("time.Rdtsc", "%d", ktime.Rdtsc())
}

func virtualAlloc(size uintptr, prot uint32) uintptr {
	p, err := windows.VirtualAlloc(0, size, windows.MEM_COMMIT|windows.MEM_RESERVE, prot)
	if err != nil {
		panic(err)
	}
	return p
}

func probeSafecopy() {
	const page = 4096
	good := virtualAlloc(2*page, windows.PAGE_READWRITE)
	var old uint32
	if err := windows.VirtualProtect(good+page, page, windows.PAGE_NOACCESS, &old); err != nil {
		panic(err)
	}
	bad := good + page
	buf := make([]byte, 2*page)
	for i := range buf {
		buf[i] = byte(i)
	}

	// A copy that stays in the good page works.
	n, err := safecopy.CopyOut(unsafe.Pointer(good), buf[:page])
	report(n == page && err == nil, "safecopy.CopyOut ok", "n=%d err=%v", n, err)
	dst := make([]byte, 2*page)
	n, err = safecopy.CopyIn(dst[:page], unsafe.Pointer(good))
	report(n == page && err == nil && bytes.Equal(dst[:page], buf[:page]), "safecopy.CopyIn ok", "n=%d err=%v", n, err)

	// Faults are recovered and report the address and the done count.
	n, err = safecopy.CopyIn(dst, unsafe.Pointer(good))
	report(n == page && err == (safecopy.SegvError{Addr: bad}), "safecopy.CopyIn fault in the middle", "n=%d err=%v, want n=%d SIGSEGV at %#x", n, err, page, bad)
	n, err = safecopy.CopyOut(unsafe.Pointer(good), buf)
	report(n == page && err == (safecopy.SegvError{Addr: bad}), "safecopy.CopyOut fault in the middle", "n=%d err=%v", n, err)
	z, err := safecopy.ZeroOut(unsafe.Pointer(good), 2*page)
	report(z == page && err == (safecopy.SegvError{Addr: bad}), "safecopy.ZeroOut fault in the middle", "n=%d err=%v", z, err)
	zeroed := unsafe.Slice((*byte)(unsafe.Pointer(good)), page)
	report(bytes.Equal(zeroed, make([]byte, page)), "safecopy.ZeroOut zeroed before the fault", "")
	_, err = safecopy.CopyIn(dst[:8], unsafe.Pointer(bad))
	report(err == (safecopy.SegvError{Addr: bad}), "safecopy.CopyIn at the fault", "err=%v", err)

	_, err = safecopy.SwapUint32(unsafe.Pointer(bad), 1)
	report(err == (safecopy.SegvError{Addr: bad}), "safecopy.SwapUint32", "err=%v", err)
	_, err = safecopy.SwapUint64(unsafe.Pointer(bad), 1)
	report(err == (safecopy.SegvError{Addr: bad}), "safecopy.SwapUint64", "err=%v", err)
	_, err = safecopy.CompareAndSwapUint32(unsafe.Pointer(bad), 0, 1)
	report(err == (safecopy.SegvError{Addr: bad}), "safecopy.CompareAndSwapUint32", "err=%v", err)
	_, err = safecopy.LoadUint32(unsafe.Pointer(bad))
	report(err == (safecopy.SegvError{Addr: bad}), "safecopy.LoadUint32", "err=%v", err)
	old32, err := safecopy.SwapUint32(unsafe.Pointer(good), 7)
	report(old32 == 0 && err == nil, "safecopy.SwapUint32 ok", "old=%d err=%v", old32, err)

	// Repeated and concurrent faults.
	const rounds = 20000
	t0 := time.Now()
	okc := 0
	for i := 0; i < rounds; i++ {
		if _, err := safecopy.CopyIn(dst[:64], unsafe.Pointer(bad)); err == (safecopy.SegvError{Addr: bad}) {
			okc++
		}
	}
	report(okc == rounds, "safecopy repeated faults", "%d/%d recovered, %.1f us per fault", okc, rounds, float64(time.Since(t0).Microseconds())/rounds)
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := make([]byte, 64)
			c := 0
			for i := 0; i < 2000; i++ {
				if _, err := safecopy.CopyIn(d, unsafe.Pointer(bad)); err == (safecopy.SegvError{Addr: bad}) {
					c++
				}
			}
			mu.Lock()
			total += c
			mu.Unlock()
		}()
	}
	wg.Wait()
	report(total == 16000, "safecopy concurrent faults", "%d/16000 recovered", total)

	// Coexistence: the Go runtime's own fault handling still works for faults
	// outside safecopy.
	old2 := debug.SetPanicOnFault(true)
	func() {
		defer func() {
			r := recover()
			report(r != nil, "go runtime panic-on-fault still works", "recovered: %v", r)
		}()
		_ = *(*byte)(unsafe.Pointer(bad))
	}()
	debug.SetPanicOnFault(old2)
	runtime.GC()
}

type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

func workingSetMB() uintptr {
	var m processMemoryCounters
	m.cb = uint32(unsafe.Sizeof(m))
	p := syscall.NewLazyDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")
	h, _ := windows.GetCurrentProcess()
	if r, _, _ := p.Call(uintptr(h), uintptr(unsafe.Pointer(&m)), uintptr(m.cb)); r == 0 {
		return 0
	}
	return m.workingSetSize >> 20
}

func probePgalloc() {
	f, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionManual,
		DecommitOnDestroy:       true,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		report(false, "pgalloc.NewMemoryFile", "%v", err)
		return
	}
	defer f.Destroy()
	report(true, "pgalloc.NewMemoryFile", "created")

	const size = 256 << 20
	fr, err := f.Allocate(size, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		report(false, "pgalloc.Allocate", "%v", err)
		return
	}
	report(fr.Length() == size, "pgalloc.Allocate", "%v, TotalSize %d", fr, f.TotalSize())

	// Write and read pages through the internal mappings.
	bs, err := f.MapInternal(fr, hostarch.ReadWrite)
	if err != nil {
		report(false, "pgalloc.MapInternal", "%v", err)
		return
	}
	pattern := make([]byte, hostarch.PageSize)
	for i := range pattern {
		pattern[i] = byte(i*7 + 1)
	}
	wn, werr := safemem.CopySeq(bs, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(bytes.Repeat(pattern, size/len(pattern)))))
	report(werr == nil && wn == uint64(size), "pgalloc write through MapInternal", "%d bytes, %v", wn, werr)
	rb, _ := f.MapInternal(memmap.FileRange{Start: fr.Start + 8*hostarch.PageSize, End: fr.Start + 9*hostarch.PageSize}, hostarch.Read)
	got := make([]byte, hostarch.PageSize)
	rn, rerr := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), rb)
	report(rerr == nil && rn == uint64(len(got)) && bytes.Equal(got, pattern), "pgalloc read through MapInternal", "%d bytes, %v", rn, rerr)

	// A second view of the section, like the one a stub process will have.
	srs := f.Sections(fr)
	report(len(srs) == 1, "pgalloc.Sections", "%+v", srs)
	view, verr := windows.MapViewOfFile(srs[0].Section, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, 0)
	if verr != nil {
		report(false, "pgalloc second view", "MapViewOfFile: %v", verr)
		return
	}
	defer windows.UnmapViewOfFile(view)
	other := unsafe.Slice((*byte)(unsafe.Pointer(view+uintptr(srs[0].SectionOffset))), srs[0].Length)
	report(bytes.Equal(other[:hostarch.PageSize], pattern), "pgalloc second view sees the writes", "")

	// Decommit releases memory and zeroes, in every view.
	wsBefore := workingSetMB()
	f.Decommit(fr)
	wsAfter := workingSetMB()
	info("pgalloc working set", "%d MB before Decommit, %d MB after (the range is %d MB)", wsBefore, wsAfter, size>>20)
	report(isZero(other[:size]), "pgalloc.Decommit zeroes (second view)", "")
	zb, _ := f.MapInternal(fr, hostarch.Read)
	allZero := true
	for ; !zb.IsEmpty(); zb = zb.Tail() {
		allZero = allZero && isZero(zb.Head().ToSlice())
	}
	report(allZero, "pgalloc.Decommit zeroes (MapInternal)", "")
	tu, terr := f.TotalUsage()
	info("pgalloc.TotalUsage", "%d MB after Decommit (%v)", tu>>20, terr)
	f.DecRef(fr)

	// Reuse after free: recycled pages read as zero.
	fr2, err := f.Allocate(1<<20, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		report(false, "pgalloc reallocate", "%v", err)
		return
	}
	b2, _ := f.MapInternal(fr2, hostarch.ReadWrite)
	for ; !b2.IsEmpty(); b2 = b2.Tail() {
		s := b2.Head().ToSlice()
		for i := range s {
			s[i] = 0xee
		}
	}
	f.DecRef(fr2)
	fr3, err := f.Allocate(1<<20, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		report(false, "pgalloc reallocate", "%v", err)
		return
	}
	b3, _ := f.MapInternal(fr3, hostarch.Read)
	z3 := true
	for ; !b3.IsEmpty(); b3 = b3.Tail() {
		z3 = z3 && isZero(b3.Head().ToSlice())
	}
	report(z3, "pgalloc reallocation reads zero", "%v then %v", fr2, fr3)
	f.DecRef(fr3)
}

func isZero(b []byte) bool {
	for i := range b {
		if b[i] != 0 {
			return false
		}
	}
	return true
}

type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

func availPhysMB() uint64 {
	var m memoryStatusEx
	m.length = uint32(unsafe.Sizeof(m))
	syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx").Call(uintptr(unsafe.Pointer(&m)))
	return m.availPhys >> 20
}

// probeRelease measures whether Decommit gives physical memory back, with
// only one view of the section mapped (a second view keeps pages resident).
func probeRelease() {
	f, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionManual,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		report(false, "release.NewMemoryFile", "%v", err)
		return
	}
	defer f.Destroy()
	const size = 512 << 20
	fr, err := f.Allocate(size, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		report(false, "release.Allocate", "%v", err)
		return
	}
	ws0, av0 := workingSetMB(), availPhysMB()
	bs, _ := f.MapInternal(fr, hostarch.ReadWrite)
	for ; !bs.IsEmpty(); bs = bs.Tail() {
		s := bs.Head().ToSlice()
		for i := 0; i < len(s); i += 4096 {
			s[i] = 1
		}
	}
	ws1, av1 := workingSetMB(), availPhysMB()
	f.Decommit(fr)
	ws2, av2 := workingSetMB(), availPhysMB()
	info("release", "working set %d -> %d -> %d MB, available physical %d -> %d -> %d MB (touched, then Decommit of %d MB)", ws0, ws1, ws2, av0, av1, av2, size>>20)
	touched := int64(ws1) - int64(ws0)
	freed := int64(ws1) - int64(ws2)
	info("release", "working set released %d of %d MB touched; available physical regained %d MB", freed, touched, int64(av2)-int64(av1))
	report(touched > size>>20/2, "release touched", "the working set grew by %d MB", touched)
	if freed >= touched/2 {
		report(true, "release Decommit frees memory", "released %d of %d MB", freed, touched)
	} else {
		info("release Decommit does not free memory", "released only %d of %d MB (pages are zeroed by clearing, not discarded)", freed, touched)
	}
	d, c, nz, e := pgalloc.DecommitStats()
	info("release", "Decommit zeroed %d MB by discarding and %d MB by clearing (non-zero after discard: %d, last DiscardVirtualMemory error: %d)", d>>20, c>>20, nz, e)
	f.DecRef(fr)
}
