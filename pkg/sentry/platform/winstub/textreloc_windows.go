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
	"encoding/binary"
	"os"
	"slices"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
)

// Relocating a cached patch state (ADR 0024, docs/results/1t-pie-cache.md).
//
// A PIE executable and the mmap area (interpreter, libraries, vDSO) land at random
// addresses in every exec, each region as a whole. The patch state of a history
// made at other region bases is the same up to these places, which patching records
// as it writes them:
//   - absAt: 8-byte guest addresses in the stub area (the return address in a trap
//     table entry, the end of a window in a veneer, the target of a relocated
//     rip-relative load), moved by the delta of the region of the code they belong to;
//   - relAt: jmp rel32 from text to the stub area (only text within 2 GiB of it, the
//     low region, uses them); the text is then mapped as a fixed-up copy;
//   - shared: veneers a syscall returns into, which live at an address the platform
//     assigns per site address (sharedVeneer): moved to the address of the new site;
//   - every map keyed by or holding a text address.
//
// Everything else (slot numbers, veneer addresses, the cursors) is the same. It
// holds only if patching made the same choices at the new places: every piece of the
// low region must be within reach of the stub area (5-byte jumps) and every piece of
// the high region out of it (7-byte jumps), before and after; otherwise the piece is
// patched as usual.

// textRelocOn can be turned off (WINSTUB_TEXTRELOC=0) to compare.
var textRelocOn = os.Getenv("WINSTUB_TEXTRELOC") != "0"

// relocMinLen: a smaller piece is patched rather than relocated (relocating copies
// the whole state, patching it costs little).
const relocMinLen = 64 << 10

// absRec is an 8-byte field at at in the stub area that holds a guest address of
// the code in region.
type absRec struct {
	at     uint64
	region int
}

// relRec is a jmp rel32 whose field is at at in the text, to target in the stub area.
type relRec struct{ at, target uint64 }

// sharedRec is a shared veneer (patchReloc): at va, vlen bytes, for the window w0
// with the original bytes window; post is the field holding its own continuation
// address, slot the trap table pointer to it (0: the text jumps with rel32).
type sharedRec struct {
	w0, va, vlen, post, slot uint64
	window                   []byte
}

// relFix sets the rel32 at section offset off to v.
type relFix struct {
	off uint64
	v   int32
}

const relocMargin = 1 << 22

// nearAll reports whether every address in [lo,hi) reaches the whole stub area
// (low area and the big %fs veneer area) with a rel32, with a margin.
func nearAll(lo, hi uint64) bool {
	return lo >= stubasm.GuestMin && hi <= stubasm.AreaLo+(1<<31)-relocMargin
}

// farAll reports whether no address in [lo,hi) reaches any of the stub area.
func farAll(lo, hi uint64) bool {
	return lo >= stubasm.FSBig+stubasm.FSBigSize+(1<<31)+relocMargin && hi <= stubasm.GuestMax
}

func isStub(a uint64) bool {
	return a >= stubasm.AreaLo && a < stubasm.AreaHi || a >= stubasm.FSBig && a < stubasm.FSBig+stubasm.FSBigSize
}

// relocSnap returns s moved by d (per region), or nil and why not. The caller holds
// as.mu; nothing of as changes (but the platform's shared veneer registry may grow).
func (as *addressSpace) relocSnap(s *patchSnap, d [2]uint64) (*patchSnap, string) {
	if s.inPlace {
		return nil, "text patched again in place"
	}
	for _, t := range s.texts {
		r := regionOf(t.lo)
		if d[r] == 0 {
			continue
		}
		ok := false
		if r == 0 {
			ok = nearAll(t.lo, t.hi) && nearAll(t.lo+d[r], t.hi+d[r])
		} else {
			ok = farAll(t.lo, t.hi) && farAll(t.lo+d[r], t.hi+d[r])
		}
		if !ok {
			return nil, "reach of the stub area differs"
		}
	}
	// Shared veneers move to the address the platform has for the new site.
	type move struct{ lo, hi, to uint64 }
	var moves []move
	shared := slices.Clone(s.shared)
	for i, sh := range shared {
		dd := d[regionOf(sh.w0)]
		if dd == 0 {
			continue
		}
		va := as.p.sharedVeneer(sh.w0+dd, sh.window, sh.vlen)
		if va == 0 {
			return nil, "shared veneer area full"
		}
		moves = append(moves, move{sh.va, sh.va + sh.vlen, va})
		shared[i].w0, shared[i].va = sh.w0+dd, va
	}
	for _, m := range moves {
		for _, o := range moves {
			if m.to < o.hi && o.lo < m.to+(m.hi-m.lo) {
				return nil, "shared veneer moves onto another" // (copying in order would break)
			}
		}
	}
	ra := func(a uint64) uint64 {
		if isStub(a) {
			for _, m := range moves {
				if a >= m.lo && a < m.hi {
					return a - m.lo + m.to
				}
			}
			return a
		}
		return a + d[regionOf(a)]
	}
	for i := range shared {
		shared[i].post = ra(shared[i].post)
	}

	n := &patchSnap{
		nsites: s.nsites, vcur: s.vcur, fcur: s.fcur, ftop: s.ftop, gcur: s.gcur, gcommit: s.gcommit,
		siteCache: make(map[uint64]siteInfo, len(s.siteCache)), restartAt: make(map[uint64]uint64, len(s.restartAt)),
		ud2Sites: rekey(s.ud2Sites, ra), tailExact: rekey(s.tailExact, ra), fsCache: make(map[fsKey]uint64, len(s.fsCache)),
		fsUD2: rekey(s.fsUD2, ra), fsSplit: rekey(s.fsSplit, ra), tails: rekey(s.tails, ra), heads: rekey(s.heads, ra),
		fsWhy: rekey(s.fsWhy, ra), edges: rekey(s.edges, ra), shared: shared, inPlace: s.inPlace,
	}
	for k, v := range s.siteCache {
		n.siteCache[ra(k)] = v
	}
	for k, v := range s.restartAt {
		n.restartAt[ra(k)] = ra(v)
	}
	for k, v := range s.fsCache {
		n.fsCache[fsKey{ra(k.addr), k.h}] = v
	}
	n.fsVen = make([]*fsVenInfo, len(s.fsVen))
	for i, vi := range s.fsVen {
		c := *vi
		c.w0 = ra(c.w0)
		n.fsVen[i] = &c
	}
	n.absAt = make([]absRec, len(s.absAt))
	for i, a := range s.absAt {
		n.absAt[i] = absRec{ra(a.at), a.region}
	}
	n.relAt = make([]relRec, len(s.relAt))
	for i, r := range s.relAt {
		n.relAt[i] = relRec{ra(r.at), ra(r.target)}
	}

	// The stub area: guest addresses first (at the old places), then the moved
	// shared veneers with their own address and the pointers to them.
	n.area = make([]areaPiece, len(s.area))
	for i, a := range s.area {
		n.area[i] = areaPiece{a.addr, slices.Clone(a.b)}
	}
	field := func(at uint64, size int) []byte {
		for _, a := range n.area {
			if at >= a.addr && at+uint64(size) <= a.addr+uint64(len(a.b)) {
				return a.b[at-a.addr : at-a.addr+uint64(size)]
			}
		}
		return nil
	}
	for _, a := range s.absAt {
		if dd := d[a.region]; dd != 0 {
			b := field(a.at, 8)
			if b == nil {
				return nil, "guest address outside the stub area kept"
			}
			binary.LittleEndian.PutUint64(b, binary.LittleEndian.Uint64(b)+dd)
		}
	}
	for i, sh := range s.shared {
		if d[regionOf(sh.w0)] == 0 {
			continue
		}
		to := shared[i].va
		src, dst := field(sh.va, int(sh.vlen)), field(to, int(sh.vlen))
		if src == nil || dst == nil {
			return nil, "shared veneer outside the stub area kept"
		}
		copy(dst, src)
		b := field(shared[i].post, 8)
		binary.LittleEndian.PutUint64(b, binary.LittleEndian.Uint64(b)-sh.va+to)
		if sh.slot != 0 {
			b := field(sh.slot, 8)
			if b == nil {
				return nil, "trap table outside the stub area kept"
			}
			binary.LittleEndian.PutUint64(b, to)
		}
	}
	for _, m := range moves {
		clear(field(m.lo, int(m.hi-m.lo))) // (as in an address space that patched at the new places)
	}

	// The text: moved views, and the rel32 fields set again in a copy.
	n.texts = make([]textView, len(s.texts))
	for i, t := range s.texts {
		dd := d[regionOf(t.lo)]
		t.lo, t.hi = t.lo+dd, t.hi+dd
		if dd != 0 {
			t.delta = dd
			for _, r := range n.relAt {
				if r.at >= t.lo && r.at+4 <= t.hi {
					v := int64(r.target) - int64(r.at+4)
					if v != int64(int32(v)) {
						return nil, "rel32 out of reach"
					}
					t.fix = append(t.fix, relFix{t.off + r.at - t.lo, int32(v)})
				}
			}
			if t.fix == nil {
				t.delta = 0 // (the same bytes: the section itself)
			}
		}
		n.texts[i] = t
	}
	return n, ""
}

func rekey[V any](m map[uint64]V, ra func(uint64) uint64) map[uint64]V {
	if m == nil {
		return nil
	}
	n := make(map[uint64]V, len(m))
	for k, v := range m {
		n[ra(k)] = v
	}
	return n
}

// fixedCopy returns a private copy of the section of t with t.fix applied.
func fixedCopy(t textView) (*privSection, error) {
	h, err := call(pCreateMapping, ^uintptr(0), 0, pageExecRW, uintptr(t.size>>32), uintptr(t.size&0xFFFFFFFF), 0)
	if err != nil {
		return nil, err
	}
	dst, err := call(pMapViewOfFile, h, fileMapWrite, 0, 0, 0)
	if err != nil {
		pCloseHandle.Call(h)
		return nil, err
	}
	defer pUnmapViewOfFile.Call(dst)
	src, err := call(pMapViewOfFile, t.sec, fileMapRead, 0, 0, 0)
	if err != nil {
		pCloseHandle.Call(h)
		return nil, err
	}
	b := unsafe.Slice((*byte)(ptr(dst)), t.size)
	copy(b, unsafe.Slice((*byte)(ptr(src)), t.size))
	pUnmapViewOfFile.Call(src)
	for _, f := range t.fix {
		binary.LittleEndian.PutUint32(b[f.off:], uint32(f.v))
	}
	return &privSection{h: h, refs: 1, size: t.size, id: privIDs.Add(1), srcID: t.id, srcDelta: t.delta}, nil
}
