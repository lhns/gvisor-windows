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
	"crypto/sha256"
	"encoding/binary"
	"maps"
	"slices"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
)

// Patched text is reused across address spaces (docs/results/1r-platform-speed.md).
//
// Patching a piece of text depends on more than its bytes: it allocates trap-table
// slots and veneers in the address space's stub area and records restart addresses,
// unpatched sites and the bytes at piece boundaries, and it may patch the start of the
// piece above again (reseam). All of that is the patch state of the address space,
// and it is a deterministic function of the text operations done in it so far: the
// pieces mapped (address and bytes) and the unmaps that touched text. as.hist is a
// hash of that history.
//
// So the result of a patch is cached under (history before, address range), with the
// original bytes for an exact comparison and a copy of the whole patch state after it,
// including the sections that hold the patched text. A later address space with the
// same history that maps the same bytes at the same range takes the state and maps
// the sections instead of copying, scanning and patching again. Sections are shared
// read-only; a later in-place patch (reseam, fixHead) copies one first (ownPriv).
//
// Addresses in the history are relative to the first text mapped in their region
// (below 4 GiB: executables; above: the mmap area with the interpreter, the
// libraries and the vDSO), whose address the entry keeps (bases). A PIE or a dynamic
// program lands elsewhere in every exec, but each region moves as a whole: an entry
// made at other bases is relocated by the difference (textreloc_windows.go, ADR 0023).
//
// An address space that turns writable memory into text (W^X, JIT) leaves the cache
// for good (histOK false).

// textCacheOn can be turned off (WINSTUB_TEXTCACHE=0) to compare.
var textCacheOn = os.Getenv("WINSTUB_TEXTCACHE") != "0"

// textCacheBytes bounds what the cache keeps of the entries with a state (original
// bytes, sections and state); past it, it starts over. textLightBytes bounds the
// bytes of the entries seen once (an exec of a PIE or of a dynamic binary lands at
// a random address and is seen once only); the oldest go first.
const (
	textCacheBytes = 128 << 20
	textLightBytes = 32 << 20
)

type textCache struct {
	mu         sync.Mutex
	m          map[textKey][]*textEntry
	bytes      int
	light      []*textEntry // entries without a state, oldest first (some may have one by now)
	lightBytes int
	serial     uint64
	hits       atomic.Uint64 // patches taken from the cache (diagnostics, tests)
	relocs     atomic.Uint64 // of them relocated (textreloc_windows.go)
}

// textKey: the history before, and the range relative to the base of its region.
type textKey struct {
	hist   [32]byte
	region int
	lo, hi uint64
}

// regionOf returns the region of a guest address (see above).
func regionOf(a uint64) int {
	if a < 1<<32 {
		return 0
	}
	return 1
}

// textEntry is one patch of orig after a history. The state after it (after) is
// kept only once a second address space has patched the same: text mapped once,
// such as the vDSO at its random address, costs just its bytes.
type textEntry struct {
	key       textKey
	orig      []byte
	after     *patchSnap // nil until the second patch
	histAfter [32]byte
	bases     [2]uint64 // of the address space whose state after is, 0 for a region without text
	size      int
	users     int  // address spaces using it now (textLookup to done)
	dropped   bool // out of the cache: the last user releases it
}

// textView is a text view of a snapshot: the entry holds a handle of its section.
type textView struct {
	lo, hi, off uint64
	sec         uintptr // the entry's own handle
	size        uint64  // of the section
	id          uint64  // privSection.id
	// A relocated view (textreloc_windows.go): mapped as a copy of the section with
	// the rel32 fields at these section offsets set to these values.
	fix   []relFix
	delta uint64
}

// privIDs numbers the private sections, so that two handles of one section are
// known to be the same.
var privIDs atomic.Uint64

type areaPiece struct {
	addr uint64
	b    []byte
}

// patchSnap is the patch state of an address space.
type patchSnap struct {
	nsites                             int
	vcur, fcur, ftop, gcur, gcommit    uint64
	siteCache                          map[uint64]siteInfo
	restartAt                          map[uint64]uint64
	ud2Sites, tailExact                map[uint64]bool
	fsCache                            map[fsKey]uint64
	fsVen                              []*fsVenInfo
	fsUD2, fsSplit, tails, heads       map[uint64][]byte
	fsWhy                              map[uint64]string
	edges                              map[uint64]edge
	area                               []areaPiece
	texts                              []textView
	absAt                              []absRec
	relAt                              []relRec
	shared                             []sharedRec
}

// textKeyFor returns the key of a patch of [lo,hi) now. A range across the two
// regions is not relative to one base: the bases go into the history (pinBases).
// The caller holds as.mu.
func (as *addressSpace) textKeyFor(lo, hi uint64) textKey {
	r := regionOf(lo)
	if regionOf(hi-1) != r {
		as.pinBases()
	}
	base := as.bases[r]
	if base == 0 {
		base = lo // (the first text of the region: textStore sets it)
	}
	return textKey{as.hist, r, lo - base, hi - base}
}

// pinBases makes the history from now on depend on where the regions are: for
// operations that do not move with them.
func (as *addressSpace) pinBases() {
	as.histMix('B', as.bases[0], as.bases[1], 0)
}

// histUnmap records an unmap of [lo,hi) that touched text: each region's part
// relative to its base (a region without a base has no text, and nothing of it is
// in the patch state).
func (as *addressSpace) histUnmap(lo, hi uint64) {
	for r, rr := range []rng{{lo, min(hi, 1 << 32)}, {max(lo, 1<<32), hi}} {
		if rr.lo < rr.hi && as.bases[r] != 0 {
			as.histMix("uU"[r], rr.lo-as.bases[r], rr.hi-as.bases[r], 0)
		}
	}
}

// histMix records a text operation in the history of the address space.
func (as *addressSpace) histMix(op byte, a, b, c uint64) {
	if !as.histOK {
		return
	}
	var buf [32 + 1 + 24]byte
	copy(buf[:], as.hist[:])
	buf[32] = op
	binary.LittleEndian.PutUint64(buf[33:], a)
	binary.LittleEndian.PutUint64(buf[41:], b)
	binary.LittleEndian.PutUint64(buf[49:], c)
	as.hist = sha256.Sum256(buf[:])
}

// equalFile reports whether the bytes of fr in f are orig.
func equalFile(f memmap.File, fr memmap.FileRange, orig []byte) bool {
	if fr.Length() != uint64(len(orig)) {
		return false
	}
	bs, err := f.MapInternal(fr, hostarch.Read)
	if err != nil {
		return false
	}
	off := 0
	var tmp []byte
	for !bs.IsEmpty() {
		b := bs.Head()
		n := b.Len()
		if off+n > len(orig) {
			return false
		}
		var got []byte
		if b.NeedSafecopy() {
			// (the pages of a host file may go away under the read)
			if cap(tmp) < n {
				tmp = make([]byte, n)
			}
			if c, err := safemem.Copy(safemem.BlockFromSafeSlice(tmp[:n]), b); err != nil || c != n {
				return false
			}
			got = tmp[:n]
		} else {
			got = b.ToSlice()
		}
		if !bytes.Equal(got, orig[off:off+n]) {
			return false
		}
		off += n
		bs = bs.Tail()
	}
	return off == len(orig)
}

// textLookup returns the cached patch of fr of f under k, or nil. The caller holds as.mu.
func (as *addressSpace) textLookup(k textKey, f memmap.File, fr memmap.FileRange) *textEntry {
	if !textCacheOn || !as.histOK {
		return nil
	}
	c := &as.p.texts
	c.mu.Lock()
	es := append([]*textEntry(nil), c.m[k]...)
	for _, e := range es {
		e.users++
	}
	c.mu.Unlock()
	var hit *textEntry
	for _, e := range es {
		if hit == nil && equalFile(f, fr, e.orig) {
			hit = e
			continue
		}
		c.done(e)
	}
	return hit
}

// done ends the use of an entry textLookup returned.
func (c *textCache) done(e *textEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.users--; e.users == 0 && e.dropped {
		e.release()
	}
}

// textStore records the patch just made of orig under k (textKeyFor before the
// patch) at [lo,hi), and moves the history of as past it. e is the entry textLookup
// found, or nil (orig is then a copy of the original bytes). The caller holds as.mu.
func (as *addressSpace) textStore(k textKey, lo uint64, orig []byte, e *textEntry) {
	if !textCacheOn || !as.histOK {
		return
	}
	if as.bases[k.region] == 0 {
		as.bases[k.region] = lo
	}
	c := &as.p.texts
	if e != nil {
		// The second patch of these bytes after this history: keep the state.
		as.hist = e.histAfter
		c.mu.Lock()
		done := e.after != nil || e.dropped // (kept already, at other bases)
		c.mu.Unlock()
		if done {
			return
		}
		snap, size := as.snapshot()
		c.mu.Lock()
		defer c.mu.Unlock()
		if e.after != nil || e.dropped {
			snap.release() // another address space was first
			return
		}
		e.after, e.bases, e.size = snap, as.bases, e.size+size
		c.lightBytes -= len(e.orig)
		c.bytes += e.size
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serial++
	// The serial names these bytes: an address space only takes this entry after
	// comparing them, so equal histories mean equal text.
	as.histMix("tT"[k.region], k.lo, k.hi, c.serial)
	e = &textEntry{key: k, orig: orig, histAfter: as.hist, size: len(orig)}
	if c.bytes > textCacheBytes {
		for _, es := range c.m {
			for _, x := range es {
				if x.dropped = true; x.users == 0 {
					x.release()
				}
			}
		}
		c.m, c.bytes, c.light, c.lightBytes = nil, 0, nil, 0
	}
	if c.m == nil {
		c.m = map[textKey][]*textEntry{}
	}
	c.m[e.key] = append(c.m[e.key], e)
	c.light = append(c.light, e)
	c.lightBytes += len(orig)
	for c.lightBytes > textLightBytes && len(c.light) > 0 {
		x := c.light[0]
		c.light = c.light[1:]
		if x.after != nil || x.dropped {
			continue // has a state now (counted in c.bytes)
		}
		c.lightBytes -= len(x.orig)
		x.dropped = true
		es := c.m[x.key]
		for i, y := range es {
			if y == x {
				es = append(es[:i:i], es[i+1:]...)
				break
			}
		}
		if len(es) == 0 {
			delete(c.m, x.key)
		} else {
			c.m[x.key] = es
		}
	}
}

func (e *textEntry) release() {
	if e.after != nil {
		e.after.release()
	}
}

func (s *patchSnap) release() {
	for _, t := range s.texts {
		pCloseHandle.Call(t.sec)
	}
}

// snapshot copies the patch state of as. Every private text section becomes shared
// with the cache. The caller holds as.mu.
func (as *addressSpace) snapshot() (*patchSnap, int) {
	s := &patchSnap{
		nsites: as.nsites, vcur: as.vcur, fcur: as.fcur, ftop: as.ftop, gcur: as.gcur, gcommit: as.gcommit,
		siteCache: maps.Clone(as.siteCache), restartAt: maps.Clone(as.restartAt), ud2Sites: maps.Clone(as.ud2Sites),
		tailExact: maps.Clone(as.tailExact), fsCache: maps.Clone(as.fsCache), fsVen: append([]*fsVenInfo(nil), as.fsVen...),
		fsUD2: maps.Clone(as.fsUD2), fsSplit: maps.Clone(as.fsSplit), tails: maps.Clone(as.tails), heads: maps.Clone(as.heads),
		fsWhy: maps.Clone(as.fsWhy), edges: maps.Clone(as.edges),
		absAt: slices.Clone(as.absAt), relAt: slices.Clone(as.relAt), shared: slices.Clone(as.shared),
	}
	size := 0
	for _, r := range as.areaUsed() {
		b := make([]byte, r.hi-r.lo)
		if r.lo >= stubasm.FSBig {
			copy(b, unsafe.Slice((*byte)(unsafe.Add(as.fbigView, r.lo-stubasm.FSBig)), len(b)))
		} else {
			copy(b, unsafe.Slice((*byte)(unsafe.Add(as.areaView, r.lo-stubasm.AreaLo)), len(b)))
		}
		s.area = append(s.area, areaPiece{r.lo, b})
		size += len(b)
	}
	for _, v := range as.views {
		if !v.isText() {
			continue
		}
		p := v.priv
		p.shared = true
		s.texts = append(s.texts, textView{lo: v.lo, hi: v.hi, off: v.off, sec: dupInto(^uintptr(0), p.h), size: p.size, id: p.id})
		size += int(v.hi - v.lo)
	}
	return s, size
}

// areaUsed returns the parts of the stub area and the big %fs veneer area that
// patching has written.
func (as *addressSpace) areaUsed() []rng {
	r := []rng{
		{stubasm.TrapPtrs, stubasm.TrapPtrs + 8*uint64(as.nsites)},
		{stubasm.TrapCode, stubasm.TrapCode + stubasm.EntrySize*uint64(as.nsites)},
		{stubasm.Veneers, as.vcur},
		{as.ftop, stubasm.Veneers + stubasm.VeneersSize}, // %fs overflow, then the shared syscall veneers
		{stubasm.FSVeneers, as.fcur},
	}
	if as.fbigView != nil {
		r = append(r, rng{stubasm.FSBig, as.gcur})
	}
	out := r[:0]
	for _, x := range r {
		if x.hi > x.lo {
			out = append(out, x)
		}
	}
	return out
}

// basesWith returns the bases of as once [lo,hi) (key k) is mapped.
func (as *addressSpace) basesWith(k textKey, lo uint64) [2]uint64 {
	b := as.bases
	if b[k.region] == 0 {
		b[k.region] = lo
	}
	return b
}

// applySnap makes s, the patch state after a history that continues the one of
// as up to histAfter, at bases, the state of as and maps its text views. The caller
// holds as.mu.
func (as *addressSpace) applySnap(s *patchSnap, histAfter [32]byte, bases [2]uint64) error {
	as.nsites, as.vcur, as.fcur, as.ftop, as.gcur = s.nsites, s.vcur, s.fcur, s.ftop, s.gcur
	as.siteCache, as.restartAt, as.ud2Sites = maps.Clone(s.siteCache), maps.Clone(s.restartAt), maps.Clone(s.ud2Sites)
	as.tailExact, as.fsCache, as.fsVen = maps.Clone(s.tailExact), maps.Clone(s.fsCache), append([]*fsVenInfo(nil), s.fsVen...)
	as.fsUD2, as.fsSplit, as.tails, as.heads = maps.Clone(s.fsUD2), maps.Clone(s.fsSplit), maps.Clone(s.tails), maps.Clone(s.heads)
	as.fsWhy, as.edges = maps.Clone(s.fsWhy), maps.Clone(s.edges)
	as.absAt, as.relAt, as.shared = slices.Clone(s.absAt), slices.Clone(s.relAt), slices.Clone(s.shared)
	for as.fbigView != nil && as.gcommit < s.gcommit {
		if _, err := windows.VirtualAlloc(uintptr(as.fbigView)+uintptr(as.gcommit-stubasm.FSBig), uintptr(stubasm.FSBigChunk), windows.MEM_COMMIT, windows.PAGE_READWRITE); err != nil {
			as.histOK = false
			return err
		}
		as.gcommit += stubasm.FSBigChunk
	}
	for _, a := range s.area {
		as.fsPut(a.addr, a.b) // (the stub area or the big area, by address)
	}
	for _, t := range s.texts {
		if v := as.findView(t.lo); v != nil && v.lo == t.lo && v.hi == t.hi && v.priv != nil &&
			(v.priv.id == t.id || t.fix != nil && v.priv.srcID == t.id && v.priv.srcDelta == t.delta) {
			continue // the same section already (or the same relocated copy of it)
		}
		if err := as.replaceText(t); err != nil {
			as.histOK = false
			return err
		}
	}
	as.hist, as.bases = histAfter, bases
	return nil
}

// replaceText maps the cached text view t, in place of what is mapped there.
func (as *addressSpace) replaceText(t textView) error {
	i := 0
	for i < len(as.views) {
		v := as.views[i]
		if v.lo < t.hi && t.lo < v.hi {
			if v.lo < t.lo || v.hi > t.hi || v.sys {
				return errSealed // (cannot happen: the history says the same views are here)
			}
			call(pUnmapViewOfFile2, as.h, uintptr(v.lo), memPreservePH)
			as.freeInsert(rng{v.lo, v.hi})
			as.views = append(as.views[:i], as.views[i+1:]...)
			as.unrefPriv(v.priv)
			continue
		}
		i++
	}
	var p *privSection
	if t.fix != nil {
		var err error
		if p, err = fixedCopy(t); err != nil {
			return err
		}
	} else {
		p = &privSection{h: dupInto(^uintptr(0), t.sec), refs: 1, size: t.size, shared: true, id: t.id}
	}
	as.privs = append(as.privs, p)
	if err := as.mapView(&view{lo: t.lo, hi: t.hi, sec: p.h, off: t.off, prot: protR | protX, priv: p}); err != nil {
		as.unrefPriv(p)
		return err
	}
	return nil
}

// ownPriv gives the text view v a section of its own if its section is shared with
// the cache, so that it can be patched in place. Every view of the section in as
// moves to the copy. The caller holds as.mu.
func (as *addressSpace) ownPriv(v *view) error {
	old := v.priv
	if old == nil || !old.shared {
		return nil
	}
	h, err := call(pCreateMapping, ^uintptr(0), 0, pageExecRW, uintptr(old.size>>32), uintptr(old.size&0xFFFFFFFF), 0)
	if err != nil {
		return err
	}
	dst, err := call(pMapViewOfFile, h, fileMapWrite, 0, 0, 0)
	if err != nil {
		pCloseHandle.Call(h)
		return err
	}
	src, err := call(pMapViewOfFile, old.h, fileMapRead, 0, 0, 0)
	if err != nil {
		pUnmapViewOfFile.Call(dst)
		pCloseHandle.Call(h)
		return err
	}
	copy(unsafe.Slice((*byte)(ptr(dst)), old.size), unsafe.Slice((*byte)(ptr(src)), old.size))
	pUnmapViewOfFile.Call(src)
	pUnmapViewOfFile.Call(dst)
	p := &privSection{h: h, size: old.size, id: privIDs.Add(1)}
	as.privs = append(as.privs, p)
	for _, w := range as.views {
		if w.priv != old {
			continue
		}
		call(pUnmapViewOfFile2, as.h, uintptr(w.lo), memPreservePH)
		w.sec, w.priv = h, p
		if _, err := call(pMapViewOfFile3, h, as.h, uintptr(w.lo), uintptr(w.off), uintptr(w.hi-w.lo), memReplacePH, winProt(w.prot), 0, 0); err != nil {
			return err
		}
		p.refs++
		old.refs--
	}
	if old.refs == 0 {
		as.closePriv(old)
	}
	return nil
}
