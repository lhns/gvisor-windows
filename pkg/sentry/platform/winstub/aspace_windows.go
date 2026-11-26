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
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/context"
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
	privs     []*privSection
	edges     map[uint64]edge

	createMu  sync.Mutex // serializes thread creation
	tmu       sync.Mutex
	byTid     map[uint32]*thread
	slots     [stubasm.MaxThreads]*thread
	freeSlots []int

	holesDone atomic.Bool
}

func newAddressSpace(p *Platform) *addressSpace {
	return &addressSpace{
		p: p, loopDone: make(chan struct{}), started: make(chan error, 1),
		vcur: stubasm.Veneers, siteCache: map[uint64]siteInfo{}, restartAt: map[uint64]uint64{},
		ud2Sites: map[uint64]bool{}, byTid: map[uint32]*thread{}, edges: map[uint64]edge{},
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
					leftTail = scanText(piece, 0).tail0f
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
		call(pUnmapViewOfFile2, as.h, uintptr(v.lo), memPreservePH)
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
		st := as.patchText(content, lo, hi)
		debugf("text %#x-%#x: %+v", lo, hi, st)
		ps, err := as.newPrivate(content)
		if err != nil {
			return err
		}
		v := &view{lo: lo, hi: hi, sec: ps.h, prot: protR | protX, priv: ps}
		if err := as.mapView(v); err != nil {
			as.unrefPriv(ps)
			return err
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
	if err := as.removeRange(uint64(addr), uint64(addr)+length); err != nil {
		debugf("Unmap(%#x, %#x): %v", uint64(addr), length, err)
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
	return errUnsupported
}

// UserModifiedGS implements platform.AddressSpace.UserModifiedGS. The trampoline
// uses %gs for the thread environment block.
func (as *addressSpace) UserModifiedGS(ctx context.Context, mm platform.MemoryManager) error {
	return errUnsupported
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
	<-as.loopDone
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
	} {
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
func (as *addressSpace) patchText(content []byte, lo, hi uint64) siteStats {
	head := content[0] == 0x05
	from := 0
	if head && as.edges[lo].tail0f {
		from = 1 // the 0x05 belongs to a syscall that starts in the range below
	}
	st, scan := as.patchCode(content, lo, from)
	debugf("edges at %#x: head05 %v from %d tail0f %v", lo, head, from, scan.tail0f)
	lowE, highE := as.edges[lo], as.edges[hi]
	if head && lowE.tail0f {
		content[0] = 0x0B
		as.ud2Sites[lo-1] = true
	}
	lowE.head05 = head
	highE.tail0f = scan.tail0f
	as.edges[lo], as.edges[hi] = lowE, highE
	if scan.tail0f && highE.head05 {
		if as.fixHead(hi) {
			as.ud2Sites[hi-1] = true
			highE.head05 = false
			as.edges[hi] = highE
		}
	}
	return st
}

// edge records, for an address where two ranges meet, what the range ending
// there ends with (tail0f) and the range starting there begins with (head05).
type edge struct{ tail0f, head05 bool }

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
	as.patchCode(buf, v.lo, 1)
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
