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
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
)

// SectionFile is the part of a memmap.File the platform needs on Windows:
// the section objects (and offsets in them) that back a file range, to map them
// into stub processes. *pgalloc.MemoryFile implements it.
type SectionFile interface {
	memmap.File
	Sections(fr memmap.FileRange) []pgalloc.SectionRange
}

// ReadOnlySectionFile is a memmap.File whose pages are those of a read-only
// section, at the same offsets (an image mapping): its read-only, non-executable
// mappings are mapped into stubs from that section instead of being copied.
type ReadOnlySectionFile interface {
	memmap.File
	ReadOnlySection() windows.Handle
}

// shareReadOnly can be turned off (WINSTUB_NOSHARE) to compare with copying.
var shareReadOnly = os.Getenv("WINSTUB_NOSHARE") == ""

var (
	errDead        = errors.New("winstub: the stub process is gone")
	errOutOfRange  = errors.New("winstub: address outside the guest range")
	errNoSection   = errors.New("winstub: the memory file does not expose sections")
	errSealed      = errors.New("winstub: range overlaps the stub's own mappings")
	errNotFree     = errors.New("winstub: range is not inside the reserved guest range")
	errUnsupported = errors.New("winstub: unsupported")
)

// privSection is a section holding the patched private copy of a text range. It
// is shared (reference counted) by the views a partial unmap leaves behind.
type privSection struct {
	h    uintptr
	refs int
}

// view is one mapped range of the stub's address space. Its backing is a section
// range; the Windows protection is uniform over the view.
//
// Execute permission requested together with write (W^X): the view starts as the
// shared memory mapped read-write; the first instruction fetch from it makes a
// patched private copy mapped read-execute (wxPriv), and the first write to that
// copy goes back to the shared memory.
type view struct {
	lo, hi uint64
	sec    uintptr
	off    uint64 // offset of lo in sec
	prot   int
	priv   *privSection
	sys    bool

	wx     bool
	wxPriv bool
	wxSec  uintptr
	wxOff  uint64 // shared backing of lo
}

// addressSpace implements platform.AddressSpace on one stub process.
type addressSpace struct {
	platform.NoAddressSpaceIO

	p        *Platform
	h        uintptr // stub process
	hMain    uintptr
	wpid     uint32
	dead     atomic.Bool
	released atomic.Bool
	loopDone chan struct{}
	started  chan error

	// mu guards everything below up to tmu.
	mu        sync.Mutex
	free      []rng // placeholders, maximal (never adjacent), sorted
	views     []*view
	areaSec   uintptr
	ctlSec    uintptr
	areaView  unsafe.Pointer
	ctlView   unsafe.Pointer
	nsites    int
	vcur      uint64
	siteCache map[uint64]siteInfo
	restartAt map[uint64]uint64
	ud2Sites  map[uint64]bool
	fbigSec   uintptr // the big %fs veneer area (rewrite mode)
	fbigView  unsafe.Pointer
	gcur      uint64 // next free address in it
	gcommit   uint64 // end of its committed part
	fcur      uint64 // next free address in the %fs veneer area
	ftop      uint64 // end of the free space shared by syscall veneers (up from vcur) and overflowing %fs veneers (down)
	fsCache   map[fsKey]uint64
	fsVen     []*fsVenInfo
	fsUD2     map[uint64][]byte
	fsSplit   map[uint64][]byte // first bytes of %fs instructions that continue in the next piece, by address
	fsWhy     map[uint64]string // why a %fs instruction stayed ud2, and how often the debugger ran it (diagnostics)
	fsHits    map[uint64]uint32
	tails     map[uint64][]byte // original last bytes of the text ranges mapped, by end address
	tailExact map[uint64]bool   // the tail starts on an instruction boundary the decoder found
	heads     map[uint64][]byte // original first bytes of text mapped with nothing below it, by start address (reseam)
	privs     []*privSection
	edges     map[uint64]edge

	createMu  sync.Mutex // serializes thread creation
	lastEvent atomic.Value
	// diagnostics: threads ended with TerminateThread instead of by themselves
	nTermFallback   atomic.Uint32
	nThreadReplaced atomic.Uint32
	// debug-event counts: events handled, %fs instructions run by the debugger, stops moved out of a veneer
	nEvents, nFSInline, nFSFix           atomic.Uint64
	nFSRepair, nFSGenuine, nFSRepairFail atomic.Uint64 // %fs bases restored by the repair routine, faults it reported
	tmu                                  sync.Mutex
	byTid                                map[uint32]*thread
	slots                                [stubasm.MaxThreads]*thread
	freeSlots                            []int

	holesDone atomic.Bool

	lastScanNS int64 // trace: time of the last scan lookup
}

func newAddressSpace(p *Platform) *addressSpace {
	return &addressSpace{
		p: p, loopDone: make(chan struct{}), started: make(chan error, 1),
		vcur: stubasm.Veneers, siteCache: map[uint64]siteInfo{}, restartAt: map[uint64]uint64{},
		ud2Sites: map[uint64]bool{}, fcur: stubasm.FSVeneers, ftop: stubasm.VeneersShared, fsCache: map[fsKey]uint64{}, fsUD2: map[uint64][]byte{}, fsSplit: map[uint64][]byte{}, fsWhy: map[uint64]string{}, fsHits: map[uint64]uint32{}, tails: map[uint64][]byte{}, tailExact: map[uint64]bool{}, heads: map[uint64][]byte{}, byTid: map[uint32]*thread{}, edges: map[uint64]edge{},
	}
}

// ---- placeholders and views -------------------------------------------------

// reserve turns every free, 64 KB-aligned gap of [ReserveMin, ReserveMax) into a
// placeholder, so that Windows can never place an allocation of its own there.
func (as *addressSpace) reserve(used []rng) error {
	cur := stubasm.ReserveMin
	used = append(append([]rng(nil), used...), rng{stubasm.WinWindow, stubasm.WinWindow + stubasm.WinWindowSize})
	sort.Slice(used, func(i, j int) bool { return used[i].lo < used[j].lo })
	add := func(s, e uint64) error {
		s = (s + 0xFFFF) &^ 0xFFFF
		e &^= 0xFFFF
		if e <= s {
			return nil
		}
		if _, err := call(pVirtualAlloc2, as.h, uintptr(s), uintptr(e-s), memReserve|memPlaceholder, pageNoAccess, 0, 0); err != nil {
			return fmt.Errorf("VirtualAlloc2 placeholder %#x-%#x: %w", s, e, err)
		}
		as.free = append(as.free, rng{s, e})
		return nil
	}
	for _, u := range used {
		if u.hi <= cur {
			continue
		}
		if u.lo > cur {
			if err := add(cur, min(u.lo, stubasm.ReserveMax)); err != nil {
				return err
			}
		}
		cur = max(cur, u.hi)
	}
	if cur < stubasm.ReserveMax {
		return add(cur, stubasm.ReserveMax)
	}
	return nil
}

func (as *addressSpace) freeIndex(lo uint64) int {
	return sort.Search(len(as.free), func(i int) bool { return as.free[i].hi > lo })
}

// freeInsert records r as a placeholder and merges it with adjacent ones.
func (as *addressSpace) freeInsert(r rng) {
	i := sort.Search(len(as.free), func(i int) bool { return as.free[i].lo >= r.lo })
	cur := r
	if i > 0 && as.free[i-1].hi == cur.lo {
		u := rng{as.free[i-1].lo, cur.hi}
		as.coalesce(u)
		cur = u
		as.free = append(as.free[:i-1], as.free[i:]...)
		i--
	}
	if i < len(as.free) && as.free[i].lo == cur.hi {
		u := rng{cur.lo, as.free[i].hi}
		as.coalesce(u)
		cur = u
		as.free = append(as.free[:i], as.free[i+1:]...)
	}
	as.free = append(as.free, rng{})
	copy(as.free[i+1:], as.free[i:])
	as.free[i] = cur
}

// coalesce merges the adjacent placeholders that exactly cover u. A failure means
// Windows already merged them.
func (as *addressSpace) coalesce(u rng) {
	if _, err := call(pVirtualFreeEx, as.h, uintptr(u.lo), uintptr(u.hi-u.lo), memRelease|memCoalesce); err != nil {
		debugf("coalesce %v: %v", u, err)
	}
}

// carve makes r a placeholder of its own (it must be inside one free placeholder).
func (as *addressSpace) carve(r rng) error {
	i := as.freeIndex(r.lo)
	if i >= len(as.free) || as.free[i].lo > r.lo || as.free[i].hi < r.hi {
		return fmt.Errorf("%w: %v", errNotFree, r)
	}
	m := as.free[i]
	if m != r {
		if _, err := call(pVirtualFreeEx, as.h, uintptr(r.lo), uintptr(r.hi-r.lo), memRelease|memPreservePH); err != nil {
			return fmt.Errorf("split placeholder %v in %v: %w", r, m, err)
		}
	}
	var rest []rng
	if m.lo < r.lo {
		rest = append(rest, rng{m.lo, r.lo})
	}
	if r.hi < m.hi {
		rest = append(rest, rng{r.hi, m.hi})
	}
	as.free = append(as.free[:i], append(rest, as.free[i+1:]...)...)
	return nil
}

func (as *addressSpace) insertView(v *view) {
	i := sort.Search(len(as.views), func(i int) bool { return as.views[i].lo >= v.lo })
	as.views = append(as.views, nil)
	copy(as.views[i+1:], as.views[i:])
	as.views[i] = v
}

func (as *addressSpace) findView(a uint64) *view {
	i := sort.Search(len(as.views), func(i int) bool { return as.views[i].hi > a })
	if i < len(as.views) && as.views[i].lo <= a {
		return as.views[i]
	}
	return nil
}

// mapView maps v (whose range must be free or already carved out) and records it.
func (as *addressSpace) mapView(v *view) error {
	r := rng{v.lo, v.hi}
	if err := as.carve(r); err != nil {
		return err
	}
	if _, err := call(pMapViewOfFile3, v.sec, as.h, uintptr(v.lo), uintptr(v.off), uintptr(v.hi-v.lo), memReplacePH, winProt(v.prot), 0, 0); err != nil {
		as.freeInsert(r)
		return fmt.Errorf("MapViewOfFile3 %v: %w", r, err)
	}
	as.insertView(v)
	return nil
}

// removeRange unmaps [lo,hi) and remaps what is left of partially covered views
// (Windows can only unmap a view as a whole).
func (as *addressSpace) removeRange(lo, hi uint64) error {
	i := sort.Search(len(as.views), func(i int) bool { return as.views[i].hi > lo })
	j := i
	for j < len(as.views) && as.views[j].lo < hi {
		if as.views[j].sys {
			return errSealed
		}
		j++
	}
	if i == j {
		return nil
	}
	as.purgeSites(lo, hi)
	for k, e := range as.edges {
		if k > lo && k <= hi {
			e.tail0f = false
		}
		if k >= lo && k < hi {
			e.head05 = false
		}
		as.edges[k] = e
	}
	olds := append([]*view(nil), as.views[i:j]...)
	// Where a private text view is cut, the new ends may form a split syscall
	// with a range mapped there later: remember what the surviving pieces end
	// and begin with (reading the text before it is unmapped).
	var leftTail, rightHead bool
	for _, v := range olds {
		if v.priv == nil {
			continue
		}
		if v.lo < lo {
			if b, ok := as.readStub(lo-1, 1); ok && b[0] == 0x0F {
				if piece, ok := as.readStub(v.lo, int(lo-v.lo)); ok {
					leftTail = scanText(piece, 0, false).tail0f
				}
			}
		}
		if v.hi > hi {
			if b, ok := as.readStub(hi, 1); ok && b[0] == 0x05 {
				rightHead = true
			}
		}
	}
	as.views = append(as.views[:i], as.views[j:]...)
	var pieces []*view
	for _, v := range olds {
		if _, err := call(pUnmapViewOfFile2, as.h, uintptr(v.lo), memPreservePH); err != nil {
			// The guest would keep the old pages there.
			log.Printf("winstub: unmapping the view %#x-%#x of stub pid %d failed: %v", v.lo, v.hi, as.wpid, err)
		}
		as.freeInsert(rng{v.lo, v.hi})
		if v.lo < lo {
			pieces = append(pieces, v.slice(v.lo, lo))
		}
		if v.hi > hi {
			pieces = append(pieces, v.slice(hi, v.hi))
		}
	}
	var err error
	for _, pc := range pieces {
		if e := as.mapView(pc); e != nil && err == nil {
			err = e
		}
	}
	if leftTail {
		e := as.edges[lo]
		e.tail0f = true
		as.edges[lo] = e
	}
	if rightHead {
		e := as.edges[hi]
		e.head05 = true
		as.edges[hi] = e
	}
	for _, v := range olds {
		as.unrefPriv(v.priv)
	}
	return err
}

// slice returns the part [lo,hi) of v as a new view (taking a reference on its
// private section).
func (v *view) slice(lo, hi uint64) *view {
	n := *v
	n.lo, n.hi = lo, hi
	n.off = v.off + (lo - v.lo)
	n.wxOff = v.wxOff + (lo - v.lo)
	if n.priv != nil {
		n.priv.refs++
	}
	return &n
}

func (as *addressSpace) unrefPriv(p *privSection) {
	if p == nil {
		return
	}
	p.refs--
	if p.refs == 0 {
		pCloseHandle.Call(p.h)
		for i, q := range as.privs {
			if q == p {
				as.privs = append(as.privs[:i], as.privs[i+1:]...)
				break
			}
		}
	}
}

// newPrivate creates a private section holding content.
func (as *addressSpace) newPrivate(content []byte) (*privSection, error) {
	n := uint64(len(content))
	h, err := call(pCreateMapping, ^uintptr(0), 0, pageExecRW, uintptr(n>>32), uintptr(n&0xFFFFFFFF), 0)
	if err != nil {
		return nil, fmt.Errorf("CreateFileMapping(%d): %w", n, err)
	}
	b, err := call(pMapViewOfFile, h, fileMapWrite, 0, 0, 0)
	if err != nil {
		pCloseHandle.Call(h)
		return nil, fmt.Errorf("MapViewOfFile: %w", err)
	}
	copy(unsafe.Slice((*byte)(ptr(b)), n), content)
	pUnmapViewOfFile.Call(b)
	p := &privSection{h: h, refs: 1}
	as.privs = append(as.privs, p)
	return p, nil
}

// ---- platform.AddressSpace --------------------------------------------------

func accessBits(at hostarch.AccessType) int {
	p := 0
	if at.Read {
		p |= protR
	}
	if at.Write {
		p |= protW | protR
	}
	if at.Execute {
		p |= protX
	}
	return p
}

// MapFile implements platform.AddressSpace.MapFile.
func (as *addressSpace) MapFile(addr hostarch.Addr, f memmap.File, fr memmap.FileRange, at hostarch.AccessType, precommit bool) error {
	if as.dead.Load() {
		return errDead
	}
	sf, isSec := f.(SectionFile)
	lo, hi := uint64(addr), uint64(addr)+fr.Length()
	if traceOn {
		t0 := qpc()
		defer func() { Trace("MapFile %#x-%#x %v sec %v %T %.3fms", lo, hi, at, isSec, f, float64(qpc()-t0)/1e6) }()
	}
	if lo < stubasm.GuestMin || hi > stubasm.GuestMax || hi < lo {
		return errOutOfRange
	}
	prot := accessBits(at)
	// Text is copied, scanned and patched. A file that is not the memory file
	// (an image mapping, say) has no section to map: its read-only mappings are
	// copied into private memory too; a writable one cannot be shared.
	isText := prot&protX != 0 && prot&protW == 0
	if !isSec && prot&protW != 0 {
		return errNoSection
	}
	// A read-only mapping of a file that is a read-only section (an erofs image)
	// is shared with the other processes: the stub maps the file's own pages.
	if rf, ok := f.(ReadOnlySectionFile); ok && !isSec && !isText && prot&protW == 0 && shareReadOnly {
		if h := rf.ReadOnlySection(); h != 0 {
			as.mu.Lock()
			err := as.removeRange(lo, hi)
			if err == nil {
				err = as.mapView(&view{lo: lo, hi: hi, sec: uintptr(h), off: fr.Start, prot: protR})
			}
			as.mu.Unlock()
			if err == nil {
				return nil
			}
			debugf("mapping the image section at %#x failed (%v); copying", lo, err)
		}
	}
	var srs []pgalloc.SectionRange
	if isSec && !isText {
		srs = sf.Sections(fr)
	}

	var content []byte
	if isText || !isSec {
		content = make([]byte, fr.Length())
		bs, err := f.MapInternal(fr, hostarch.Read)
		if err != nil {
			return err
		}
		if _, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(content)), bs); err != nil {
			return err
		}
	}

	as.mu.Lock()
	defer as.mu.Unlock()
	if err := as.removeRange(lo, hi); err != nil {
		return err
	}
	if content != nil && !isText {
		ps, err := as.newPrivate(content)
		if err != nil {
			return err
		}
		v := &view{lo: lo, hi: hi, sec: ps.h, prot: protR, priv: ps}
		if err := as.mapView(v); err != nil {
			as.unrefPriv(ps)
			return err
		}
		return nil
	}
	if content != nil {
		t1 := qpc()
		st := as.patchText(content, lo, hi)
		t2 := qpc()
		debugf("text %#x-%#x: %+v (fs veneers: %d of %d bytes)", lo, hi, st, as.fcur-stubasm.FSVeneers, stubasm.FSVeneersSize)
		ps, err := as.newPrivate(content)
		if err != nil {
			return err
		}
		v := &view{lo: lo, hi: hi, sec: ps.h, prot: protR | protX, priv: ps}
		if err := as.mapView(v); err != nil {
			as.unrefPriv(ps)
			return err
		}
		if traceOn {
			Trace("text %#x-%#x: patch %.3fms (scan %.3fms) section %.3fms %+v", lo, hi, float64(t2-t1)/1e6, float64(as.lastScanNS)/1e6, float64(qpc()-t2)/1e6, st)
		}
		return nil
	}
	wx := prot&protX != 0 // with write: W^X, executable only after a scan
	prot &^= protX
	for _, sr := range srs {
		va := lo + (sr.FileOffset - fr.Start)
		v := &view{lo: va, hi: va + sr.Length, sec: uintptr(sr.Section), off: sr.SectionOffset, prot: prot,
			wx: wx, wxSec: uintptr(sr.Section), wxOff: sr.SectionOffset}
		if err := as.mapView(v); err != nil {
			return err
		}
	}
	return nil
}

// Unmap implements platform.AddressSpace.Unmap.
func (as *addressSpace) Unmap(addr hostarch.Addr, length uint64) {
	if as.dead.Load() {
		return
	}
	as.mu.Lock()
	defer as.mu.Unlock()
	// The memory manager widens an unmap to the gaps between its mappings, up to
	// the end of the address space: the range may span the stub's own views in
	// the guest range (the %fs veneer area, reserved against the guest), which stay.
	// Refusing the whole range instead left stale views behind: a heap shrunk and
	// grown again kept its old pages, which the Sentry no longer wrote to
	// (docs/results/1p-rewrite-heap.md).
	lo, hi := uint64(addr), uint64(addr)+length
	for lo < hi {
		end, next := hi, hi
		i := sort.Search(len(as.views), func(i int) bool { return as.views[i].hi > lo })
		for ; i < len(as.views) && as.views[i].lo < hi; i++ {
			if v := as.views[i]; v.sys {
				end, next = max(lo, v.lo), min(hi, v.hi)
				break
			}
		}
		if end > lo {
			if err := as.removeRange(lo, end); err != nil {
				log.Printf("winstub: Unmap(%#x, %#x) in stub pid %d: %v", lo, end-lo, as.wpid, err)
			}
		}
		lo = next
	}
}

// PreFork implements platform.AddressSpace.PreFork. The child's address space is a
// new stub that the memory manager fills through MapFile; nothing is shared.
func (as *addressSpace) PreFork() {}

// PostFork implements platform.AddressSpace.PostFork.
func (as *addressSpace) PostFork() {}

// PrepareForPtrace implements platform.AddressSpace.PrepareForPtrace. Rewritten
// syscall sites cannot be restored, so ptrace is not supported.
func (as *addressSpace) PrepareForPtrace(ctx context.Context, mm platform.MemoryManager) error {
	return linuxerr.EPERM
}

// UserModifiedGS implements platform.AddressSpace.UserModifiedGS. The trampoline
// uses %gs for the thread environment block.
func (as *addressSpace) UserModifiedGS(ctx context.Context, mm platform.MemoryManager) error {
	return linuxerr.EPERM
}

// Release implements platform.AddressSpace.Release.
func (as *addressSpace) Release() {
	if as.released.Swap(true) {
		return
	}
	Trace("as: release")
	unregisterAS(as)
	as.markDead()
	pTerminateProcess.Call(as.h, 0)
	// The debugger goroutine ends within a poll interval of the release. If it
	// does not (a stub stuck in the kernel that cannot be terminated, or a debug
	// event that is never delivered), the address space is abandoned: the process,
	// the goroutine and the handles leak rather than block the task that exits.
	select {
	case <-as.loopDone:
	case <-time.After(10 * time.Second):
		debugf("abandoning stub process %d: its debugger loop did not end", as.wpid)
		return
	}
	Trace("as: process gone")
	as.mu.Lock()
	defer as.mu.Unlock()
	as.tmu.Lock()
	for _, t := range as.slots {
		if t != nil {
			pCloseHandle.Call(t.evReq)
			pCloseHandle.Call(t.evReply)
			if t.hThread != 0 && t.idx != 0 {
				pCloseHandle.Call(t.hThread)
				t.hThread = 0 // a later freeThread must not close it again: the value may be reused
			}
		}
	}
	as.tmu.Unlock()
	for _, p := range as.privs {
		pCloseHandle.Call(p.h)
	}
	as.privs = nil
	pUnmapViewOfFile.Call(uintptr(as.areaView))
	pUnmapViewOfFile.Call(uintptr(as.ctlView))
	if as.fbigView != nil {
		pUnmapViewOfFile.Call(uintptr(as.fbigView))
		pCloseHandle.Call(as.fbigSec)
	}
	pCloseHandle.Call(as.areaSec)
	pCloseHandle.Call(as.ctlSec)
	pCloseHandle.Call(as.hMain)
	pCloseHandle.Call(as.h)
}

// markDead wakes everything waiting on the stub.
func (as *addressSpace) markDead() {
	as.dead.Store(true)
	as.tmu.Lock()
	for _, t := range as.slots {
		if t != nil {
			pSetEvent.Call(t.evReq)
		}
	}
	as.tmu.Unlock()
}

// reserveHoles reserves, through the memory manager, the guest-range addresses
// Windows keeps for itself (KUSER_SHARED_DATA at 0x7FFE0000 and one more page
// below 0x7FFEC000 whose position differs per process), the way systrap reserves
// its usertrap table.
func (as *addressSpace) reserveHoles(ctx context.Context, mm platform.MemoryManager) {
	if as.holesDone.Swap(true) {
		return
	}
	for _, h := range []hostarch.AddrRange{
		{Start: holeStart, End: holeEnd},
		{Start: hostarch.Addr(stubasm.WinWindow), End: hostarch.Addr(stubasm.WinWindow + stubasm.WinWindowSize)},
		{Start: hostarch.Addr(stubasm.FSBig), End: hostarch.Addr(stubasm.FSBig + stubasm.FSBigSize)},
	} {
		if h.Start == hostarch.Addr(stubasm.FSBig) && !as.p.fsRewrite {
			continue
		}
		if _, _, err := mm.FindVMAByName(h, holeName); err == nil {
			continue // inherited through fork
		}
		_, err := mm.MMap(ctx, memmap.MMapOpts{
			Force:   true,
			Unmap:   true,
			Fixed:   true,
			Addr:    h.Start,
			Length:  uint64(h.Length()),
			Private: true,
			Name:    holeName,
			Sealed:  true,
		})
		if err != nil {
			debugf("reserving %v for Windows failed: %v", h, err)
		}
	}
}

const (
	holeStart hostarch.Addr = 0x7FFE0000
	holeEnd   hostarch.Addr = 0x7FFF0000
	holeName                = "[winstub-reserved]"
)

// ---- W^X ---------------------------------------------------------------------

const wxWindow = 1 << 20

// wxExec handles an instruction fetch from a writable+executable view that is
// still the shared memory: a 1 MiB window around a is scanned, patched and mapped
// as a private read-execute copy.
func (as *addressSpace) wxExec(a uint64) bool {
	as.mu.Lock()
	defer as.mu.Unlock()
	v := as.findView(a)
	if v == nil || !v.wx || v.wxPriv {
		return false
	}
	lo := max(v.lo, a&^(wxWindow-1))
	hi := min(v.hi, (a&^(wxWindow-1))+wxWindow)
	buf := make([]byte, hi-lo)
	var n uintptr
	if _, err := call(pReadProcMem, as.h, uintptr(lo), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&n))); err != nil {
		debugf("wxExec read: %v", err)
		return false
	}
	ws, wo := v.wxSec, v.wxOff+(lo-v.lo)
	if err := as.removeRange(lo, hi); err != nil {
		return false
	}
	shared := func() {
		as.mapView(&view{lo: lo, hi: hi, sec: ws, off: wo, prot: protR | protW, wx: true, wxSec: ws, wxOff: wo})
	}
	st := as.patchText(buf, lo, hi)
	debugf("wx rescan %#x-%#x: %+v", lo, hi, st)
	ps, err := as.newPrivate(buf)
	if err != nil {
		shared()
		return false
	}
	nv := &view{lo: lo, hi: hi, sec: ps.h, prot: protR | protX, priv: ps, wx: true, wxPriv: true, wxSec: ws, wxOff: wo}
	if err := as.mapView(nv); err != nil {
		as.unrefPriv(ps)
		debugf("wxExec map: %v", err)
		shared()
		return false
	}
	return true
}

// wxWrite handles a write to the private executable copy: the range goes back to
// the shared memory, writable and not executable.
func (as *addressSpace) wxWrite(a uint64) bool {
	as.mu.Lock()
	defer as.mu.Unlock()
	v := as.findView(a)
	if v == nil || !v.wx || !v.wxPriv {
		return false
	}
	lo, hi, ws, wo := v.lo, v.hi, v.wxSec, v.wxOff
	if err := as.removeRange(lo, hi); err != nil {
		return false
	}
	nv := &view{lo: lo, hi: hi, sec: ws, off: wo, prot: protR | protW, wx: true, wxSec: ws, wxOff: wo}
	if err := as.mapView(nv); err != nil {
		debugf("wxWrite map: %v", err)
		return false
	}
	return true
}

// patchText scans and patches the text content that will be mapped at [lo,hi)
// and handles a syscall instruction that straddles one of its ends: 0x0f ends
// one range, 0x05 starts the next. Whichever range is mapped second turns the
// 0x05 into 0x0b (ud2, the debugger path) so that the pair is never executable.
// The caller holds as.mu.
//
// Text arrives in pieces, in any order, and a piece may begin in the middle of an
// instruction. The decoder needs the real instruction boundaries at the start, so:
//   - a piece mapped above one that is already there starts decoding in the bytes
//     the lower piece ended with (as.tails, at a real boundary when the lower piece
//     was decoded); only sites in the first bytes may still be unsure (zoneContext);
//   - a piece mapped with nothing below it is decoded from its first byte, and the
//     start of the original is kept (as.heads); when the piece below arrives,
//     reseam decodes the seam again and repatches what the guess got wrong.
func (as *addressSpace) patchText(content []byte, lo, hi uint64) siteStats {
	head := content[0] == 0x05
	from := 0
	if head && as.edges[lo].tail0f {
		from = 1 // the 0x05 belongs to a syscall that starts in the range below
	}
	var pre []byte
	exact := false
	if from == 0 {
		pre, exact = as.tails[lo], as.tailExact[lo]
	}
	tailRaw := append([]byte(nil), content[max(0, len(content)-tailKeep):]...)
	if len(pre) == 0 && from == 0 {
		as.heads[lo] = append([]byte(nil), content[:min(len(content), headKeep)]...)
	} else {
		delete(as.heads, lo)
	}
	buf, bufBase := content, lo
	if len(pre) > 0 {
		buf, bufBase = append(append([]byte(nil), pre...), content...), lo-uint64(len(pre))
	}
	zone := 0
	if len(pre) > 0 {
		if !exact {
			zone = zoneContext
		}
	} else if from == 0 {
		zone = zoneNoContext // the range may begin inside an instruction (the piece below may be mapped later)
	}
	// An %fs instruction that starts at the end of this piece and ends in the next
	// is neither piece's to patch: its first byte becomes int3 and the debugger
	// runs it (fsSplit) once the rest is there.
	stop := len(buf)
	if as.p.fsRewrite && bytes.IndexByte(buf[max(from, len(buf)-14):], 0x64) >= 0 {
		if p := splitFSAt(buf, decodeAll(buf[from:]), from); p >= 0 {
			stop = p
		}
	}
	var splitBytes []byte
	if stop < len(buf) {
		splitBytes = append([]byte(nil), buf[stop:]...)
	}
	st, scan := as.patchCode(buf, bufBase, from, len(pre), zone, stop, nil)
	if splitBytes != nil {
		buf[stop] = 0xCC
		as.fsSplit[bufBase+uint64(stop)] = splitBytes
		debugf("%%fs instruction split at the end of %#x: starts at %#x", hi, bufBase+uint64(stop))
	}
	if len(pre) > 0 {
		copy(content, buf[len(pre):])
	}
	// The bytes the next piece decodes from: from the last instruction boundary at
	// least 16 bytes before the end if the decoder knows one, else the last bytes.
	tail, tailOK := tailRaw, false
	for _, b := range scan.tailB { // (descending)
		if c := b - len(pre) - (len(content) - len(tailRaw)); c >= 0 && len(tailRaw)-c >= 16 {
			tail, tailOK = tailRaw[c:], true
			break
		}
	}
	as.tails[hi], as.tailExact[hi] = tail, tailOK
	debugf("edges at %#x: head05 %v from %d tail0f %v", lo, head, from, scan.tail0f)
	lowE, highE := as.edges[lo], as.edges[hi]
	if head && lowE.tail0f {
		content[0] = 0x0B
		as.ud2Sites[lo-1] = true
	}
	lowE.head05 = head
	highE.tail0f = scan.tail0f
	as.edges[lo], as.edges[hi] = lowE, highE
	// The piece above was mapped first: decode the seam again. (Text that was cut by an
	// unmap has no kept start; a syscall across such a seam is handled by fixHead.)
	if h := as.heads[hi]; h != nil {
		if as.reseam(hi, h, tail, tailOK, scan.tail0f && highE.head05) {
			highE.head05 = false
			as.edges[hi] = highE
		}
	}
	if scan.tail0f && highE.head05 {
		if as.fixHead(hi) {
			as.ud2Sites[hi-1] = true
			highE.head05 = false
			as.edges[hi] = highE
		}
	}
	return st
}

const (
	headKeep = 576 // bytes of a piece's original start kept for reseam
	tailKeep = 128 // bytes of a piece's original end kept for the piece above
)

// edge records, for an address where two ranges meet, what the range ending
// there ends with (tail0f) and the range starting there begins with (head05).
type edge struct{ tail0f, head05 bool }

// reseam repatches the start of the piece mapped at lo, which was patched before the
// piece below it (ending in tail) was known, so decoded from a guess at where its
// first instruction begins. It decodes the seam again with the real bytes below, finds
// where the guessed instruction boundaries (orig decoded from its first byte, as
// patchText did) stop differing from the real ones, puts the original bytes back over
// every patch that touches the part before that point, and patches that part again
// from the real decoding. straddle: a syscall begins in the last byte below (0x0f) and
// ends in the first byte of the piece (0x05); that byte becomes 0x0b. It reports
// whether that byte was handled. The caller holds as.mu.
func (as *addressSpace) reseam(lo uint64, orig, tail []byte, exact, straddle bool) bool {
	delete(as.heads, lo)
	n := len(orig)
	// The real boundaries and the guessed ones, as offsets in the piece.
	real := map[int]bool{}
	pb := append(append([]byte(nil), tail...), orig...)
	for _, x := range decodeRange(pb, 0, len(pb), nil) {
		if x.off >= len(tail) {
			real[x.off-len(tail)] = true
		}
	}
	guess := map[int]bool{}
	for _, x := range decodeRange(orig, 0, n, nil) {
		guess[x.off] = true
	}
	last := -1 // the last offset where the two disagree
	for p := 0; p < n-32; p++ {
		if real[p] != guess[p] {
			last = p
		}
	}
	if last < 0 {
		return false // the same instructions: nothing was wrong
	}
	end := -1 // the first boundary after the last disagreement
	for p := last + 1; p < n-32; p++ {
		if guess[p] {
			end = p
			break
		}
	}
	if end < 0 {
		debugf("reseam %#x: the decodings never agree in %d bytes", lo, n)
		return false
	}
	v := as.findView(lo)
	if v == nil || v.priv == nil || v.hi < lo+uint64(n) {
		debugf("reseam %#x: the piece is not in one private view", lo)
		return false
	}
	cur, ok := as.readStub(lo, n)
	if !ok {
		return false
	}
	// Patches are the bytes that differ from the original: runs of them. Every run
	// that touches [0,end) goes back to the original in full, and everything up to its
	// end is patched again.
	type run struct{ lo, hi int }
	var runs []run
	for i := 0; i < n; i++ {
		if cur[i] != orig[i] {
			j := i
			for j < n && cur[j] != orig[j] {
				j++
			}
			runs = append(runs, run{i, j})
			i = j
		}
	}
	for changed := true; changed; {
		changed = false
		for _, r := range runs {
			if r.lo < end && r.hi > end {
				end, changed = r.hi, true
			}
		}
	}
	if end+64 > n {
		debugf("reseam %#x: the repatched part (%d bytes) reaches the end of the kept bytes", lo, end)
		return false
	}
	var keep []rng
	for _, r := range runs {
		if r.lo >= end {
			keep = append(keep, rng{uint64(len(tail) + r.lo), uint64(len(tail) + r.hi)})
		}
	}
	as.purgeSites(lo, lo+uint64(end))
	buf := append(append([]byte(nil), tail...), orig...)
	zone := 0
	if !exact {
		zone = zoneContext
	}
	rst, _ := as.patchCode(buf, lo-uint64(len(tail)), 0, len(tail), zone, len(tail)+end, keep)
	debugf("reseam %#x: tail % x | head % x | patched %+v exact %v", lo, tail[max(0, len(tail)-16):], orig[:24], rst, exact)
	fix := append([]byte(nil), buf[len(tail):len(tail)+end]...)
	if straddle {
		fix[0] = 0x0B
		as.ud2Sites[lo-1] = true
	}
	if err := as.writeText(v, lo, fix); err != nil {
		debugf("reseam %#x: %v", lo, err)
		return straddle
	}
	debugf("reseam %#x: the first %d bytes were decoded wrongly (disagreement up to %d); patched again", lo, end, last)
	return straddle
}

// fixHead handles the range mapped at a whose first byte (0x05) completes a
// syscall instruction that starts in the range below, which was mapped later:
// the first byte becomes 0x0b (ud2) and, because the sweep over the range began
// at the wrong offset, the rest is scanned again from offset 1 and the private
// text updated in place.
func (as *addressSpace) fixHead(a uint64) bool {
	v := as.findView(a)
	if v == nil || v.priv == nil || v.lo != a {
		return false
	}
	buf, ok := as.readStub(v.lo, int(v.hi-v.lo))
	if !ok {
		return false
	}
	orig := append([]byte(nil), buf...)
	as.patchCode(buf, v.lo, 1, 0, 0, len(buf), nil)
	buf[0] = 0x0B
	off := v.off
	base := off &^ 0xFFFF
	n := off - base + uint64(len(buf))
	m, err := call(pMapViewOfFile, v.priv.h, fileMapWrite, uintptr(base>>32), uintptr(base&0xFFFFFFFF), uintptr(n))
	if err != nil {
		debugf("fixHead: %v", err)
		return false
	}
	dst := unsafe.Slice((*byte)(ptr(m+uintptr(off-base))), len(buf))
	for i := range buf {
		if buf[i] != orig[i] {
			dst[i] = buf[i]
		}
	}
	pUnmapViewOfFile.Call(m)
	return true
}

// writeText overwrites text that is mapped at addr in the private view v.
func (as *addressSpace) writeText(v *view, addr uint64, b []byte) error {
	off := v.off + (addr - v.lo)
	base := off &^ 0xFFFF
	m, err := call(pMapViewOfFile, v.priv.h, fileMapWrite, uintptr(base>>32), uintptr(base&0xFFFFFFFF), uintptr(off-base+uint64(len(b))))
	if err != nil {
		return err
	}
	copy(unsafe.Slice((*byte)(ptr(m+uintptr(off-base))), len(b)), b)
	pUnmapViewOfFile.Call(m)
	return nil
}

// viewDesc describes the view at a for the trace.
func (as *addressSpace) viewDesc(a uint64) string {
	as.mu.Lock()
	defer as.mu.Unlock()
	v := as.findView(a)
	if v == nil {
		return "unmapped"
	}
	return fmt.Sprintf("view %#x-%#x prot %d priv %v wx %v/%v", v.lo, v.hi, v.prot, v.priv != nil, v.wx, v.wxPriv)
}

// mapped reports whether a lies in a view (to tell SEGV_ACCERR from SEGV_MAPERR).
func (as *addressSpace) mapped(a uint64) bool {
	as.mu.Lock()
	defer as.mu.Unlock()
	return as.findView(a) != nil
}

// restartTarget translates the instruction pointer the Sentry hands back (see
// patchImm and patchReloc).
func (as *addressSpace) restartTarget(rip uint64) uint64 {
	as.mu.Lock()
	defer as.mu.Unlock()
	if t, ok := as.restartAt[rip]; ok {
		return t
	}
	return rip
}

func (as *addressSpace) isUD2Site(a uint64) bool {
	as.mu.Lock()
	defer as.mu.Unlock()
	return as.ud2Sites[a]
}

// purgeSites forgets what was recorded about the patched text in [lo,hi): the
// addresses the Sentry may hand back inside patched windows (restartAt), the
// unpatched sites, and the saved last bytes of the pieces ending there. They would
// redirect or mis-decode whatever is mapped there next. (The slot and veneer
// caches stay: they are keyed by the content.)
func (as *addressSpace) purgeSites(lo, hi uint64) {
	for k := range as.restartAt {
		if k >= lo && k < hi {
			delete(as.restartAt, k)
		}
	}
	for k := range as.ud2Sites {
		if k >= lo && k < hi {
			delete(as.ud2Sites, k)
		}
	}
	for k := range as.fsUD2 {
		if k >= lo && k < hi {
			delete(as.fsUD2, k)
			delete(as.fsWhy, k)
			delete(as.fsHits, k)
		}
	}
	for k := range as.fsSplit {
		if k >= lo && k < hi {
			delete(as.fsSplit, k)
		}
	}
	for k := range as.tails {
		if k > lo && k <= hi {
			delete(as.tails, k)
			delete(as.tailExact, k)
		}
	}
	for k := range as.heads {
		if k >= lo && k < hi {
			delete(as.heads, k)
		}
	}
}
