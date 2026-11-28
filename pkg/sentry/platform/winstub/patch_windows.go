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
	"hash/maphash"
	"sync"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
	"gvisor.dev/gvisor/pkg/sentry/profkeys"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/x86asm"
)

// siteStats counts what the scanner found and did.
type siteStats struct {
	pairs    int // 0f 05 byte pairs in the scanned range
	syscalls int // decoded SYSCALL instructions
	imm      int // mov $imm32,%eax|%rax; syscall -> jmp *slot
	reloc    int // any other site -> jump to a per-site veneer
	ud2      int // left as ud2 (debugger path)
	fs       int // %fs instructions rewritten into veneers
	fsUD2    int // %fs instructions left as ud2 (the debugger runs them)
	fsBad    int // %fs instructions in an encoding that cannot be rewritten
}

// claims records the byte ranges (offsets in the scanned code) patches have
// taken, so that a later patch does not displace bytes an earlier one rewrote.
type claims struct {
	l     []rng
	fixed []rng // bytes that must stay as they are (checked in full)
}

func (c *claims) add(lo, hi int) { c.l = append(c.l, rng{uint64(lo), uint64(hi)}) }

func (c *claims) taken(lo, hi int) bool {
	for _, f := range c.fixed {
		if int(f.lo) < hi && lo < int(f.hi) {
			return true
		}
	}
	for i := len(c.l) - 1; i >= 0 && i >= len(c.l)-32; i-- {
		if int(c.l[i].lo) < hi && lo < int(c.l[i].hi) {
			return true
		}
	}
	return false
}

func (c *claims) inFixed(off int) bool {
	for _, f := range c.fixed {
		if int(f.lo) <= off && off < int(f.hi) {
			return true
		}
	}
	return false
}

// scanCache remembers scan results by content: the same library text is mapped
// again by every process. An entry keeps the original bytes and a hit must match
// them exactly; the hash only finds the candidates. (A cryptographic hash of the
// whole text cost more than the scan it saved on CPUs without SHA instructions,
// 13 to 18 ms for a 1.5 MB busybox on the development VM.)
type scanCache struct {
	mu    sync.Mutex
	seed  maphash.Seed
	m     map[uint64][]*scanEntry
	bytes int // original bytes kept
}

type scanEntry struct {
	code   []byte
	from   int
	wantFS bool
	r      *scanResult
}

// scanCacheBytes bounds the original bytes the cache keeps; past it, it starts over.
const scanCacheBytes = 256 << 20

// scan scans code, starting at offset from (1 when the first byte is the second
// half of a syscall instruction that begins in the previous range).
func (c *scanCache) scan(code []byte, from int, wantFS bool) *scanResult {
	c.mu.Lock()
	if c.m == nil {
		c.seed = maphash.MakeSeed()
		c.m = map[uint64][]*scanEntry{}
	}
	seed := c.seed
	c.mu.Unlock()
	key := maphash.Bytes(seed, code)
	c.mu.Lock()
	for _, e := range c.m[key] {
		if e.from == from && e.wantFS == wantFS && bytes.Equal(e.code, code) {
			c.mu.Unlock()
			return e.r
		}
	}
	c.mu.Unlock()
	r := scanText(code, from, wantFS)
	e := &scanEntry{code: append([]byte(nil), code...), from: from, wantFS: wantFS, r: r}
	c.mu.Lock()
	if c.bytes+len(code) > scanCacheBytes {
		c.m, c.bytes = map[uint64][]*scanEntry{}, 0
	}
	c.m[key] = append(c.m[key], e)
	c.bytes += len(code)
	c.mu.Unlock()
	return r
}

// siteInfo remembers a patched `mov $imm,%eax; syscall` site so that scanning the
// same text again (W^X rescans) reuses its table entry.
type siteInfo struct {
	nr    uint32
	entry uint64
}

// areaWrite writes into the low stub area (trap table, veneers).
func (as *addressSpace) areaWrite(addr uint64, b []byte) {
	copy(unsafe.Slice((*byte)(unsafe.Add(as.areaView, addr-stubasm.AreaLo)), len(b)), b)
}

// patchCode rewrites every syscall instruction in code (the bytes that will be
// mapped at base) before any of it can execute:
//   - `mov $imm32,%eax|%rax; syscall` becomes `jmp *slot` through the trap table;
//   - any other site becomes `jmp rel32` (or `jmp *slot` when the veneer is out of
//     rel32 reach) to a per-site veneer holding the displaced instructions (copied
//     unchanged) around a jump into the trampoline;
//   - a site that cannot be patched becomes ud2 (the debugger path).
//
// Bookkeeping for restarts and ud2 sites goes to as.restartAt and as.ud2Sites.
// The caller holds as.mu.
// The sites in the first zone bytes after the context get only the safe patch:
// zoneNoContext when nothing is known about the bytes below, zoneContext when the
// decoder started in the original tail of the range below.
const (
	zoneNoContext = 48
	zoneContext   = 8
)

//
// keep lists byte ranges that are already patched and stay so (a re-patch of the start
// of a range, see reseam); the sites inside are left alone and no patch overlaps them.
func (as *addressSpace) patchCode(code []byte, base uint64, from, skip, zone, stop int, keep []rng) (siteStats, *scanResult) {
	var st siteStats
	t0 := qpc()
	scan := as.p.scans.scan(code, from, as.p.fsRewrite)
	as.lastScanNS = qpc() - t0
	st.pairs = scan.pairs
	cl := claims{fixed: keep}
	if skip > 0 {
		cl.add(0, skip) // context bytes of the range below: decoded, never patched
	}
	if stop < len(code) {
		cl.add(stop, len(code)) // an instruction that continues in the next piece (splitFSAt)
	}
	for i := range scan.sites {
		site := &scan.sites[i]
		if site.off < skip || site.off >= stop || cl.inFixed(site.off) {
			continue
		}
		win, tgt := site.win[:site.n], site.tgt[:site.n]
		// A range may begin in the middle of an instruction (the text is mapped
		// in pieces); the decoder needs a few instructions to find the real
		// boundaries, so the sites there only get the safe patch (ud2).
		unsure := site.off < skip+zone
		if site.fs {
			as.patchFS(code, base, site, &cl, &st, unsure)
			continue
		}
		st.syscalls++
		if unsure {
			code[site.off], code[site.off+1] = 0x0F, 0x0B
			as.ud2Sites[base+uint64(site.off)] = true
			st.ud2++
			continue
		}
		if as.patchImm(code, base, win, site.s, tgt, &cl, &st) {
			continue
		}
		if as.patchReloc(code, base, win, site.s, tgt, &cl, &st) {
			continue
		}
		code[site.off], code[site.off+1] = 0x0F, 0x0B
		as.ud2Sites[base+uint64(site.off)] = true
		st.ud2++
		debugf("ud2 site %#x", base+uint64(site.off))
	}
	return st, scan
}

func (as *addressSpace) newSlot() (idx uint64, ok bool) {
	if as.nsites >= stubasm.MaxSites {
		return 0, false
	}
	idx = uint64(as.nsites)
	as.nsites++
	return idx, true
}

func (as *addressSpace) patchImm(code []byte, base uint64, list []ins, s int, tgt []bool, cl *claims, st *siteStats) bool {
	x := list[s]
	a := base + uint64(x.off)
	if s == 0 || list[s-1].off+list[s-1].n != x.off || tgt[s] || list[s-1].op != x86asm.MOV {
		return false
	}
	pv := list[s-1]
	if cl.taken(pv.off, x.off+x.n) {
		return false
	}
	pc := code[pv.off : pv.off+pv.n]
	var sysno uint32
	kind := 0
	if pv.n == 5 && pc[0] == 0xB8 {
		kind, sysno = 32, binary.LittleEndian.Uint32(pc[1:])
	} else if pv.n == 7 && pc[0] == 0x48 && pc[1] == 0xC7 && pc[2] == 0xC0 {
		kind, sysno = 64, binary.LittleEndian.Uint32(pc[3:])
	} else {
		return false
	}
	var idx, entry uint64
	if si, ok := as.siteCache[a]; ok && si.nr == sysno {
		entry = si.entry
		idx = (entry - stubasm.TrapCode) / stubasm.EntrySize
	} else {
		var ok bool
		if idx, ok = as.newSlot(); !ok {
			return false
		}
		entry = stubasm.TrapCode + stubasm.EntrySize*idx
		var e []byte
		if sysno == sysGetcpu && kind == 32 && isVDSOGetcpu(code, pv.off) {
			// The vDSO's getcpu is a bare syscall: answer it in the stub.
			as.absAt = append(as.absAt, absRec{entry + 2, regionOf(a)})
			e = append(e, 0x48, 0xB9)
			e = binary.LittleEndian.AppendUint64(e, a+2) // movabs $ret,%rcx
			e = append(e, 0xE9)                          // jmp rel32 to GetcpuCode
			e = binary.LittleEndian.AppendUint32(e, uint32(int32(int64(stubasm.StubCode+stubasm.OffGetcpu)-int64(entry+uint64(len(e))+4))))
		} else {
			as.absAt = append(as.absAt, absRec{entry + 7, regionOf(a)})
			e = []byte{0xB8}
			e = binary.LittleEndian.AppendUint32(e, sysno)
			e = append(e, 0x48, 0xB9)
			e = binary.LittleEndian.AppendUint64(e, a+2)
			e = append(e, 0x49, 0xBB)
			e = binary.LittleEndian.AppendUint64(e, stubasm.StubCode+stubasm.OffTramp)
			e = append(e, 0x41, 0xFF, 0xE3)
		}
		as.areaWrite(entry, e)
		as.areaWrite(stubasm.TrapPtrs+8*idx, binary.LittleEndian.AppendUint64(nil, entry))
		as.siteCache[a] = siteInfo{sysno, entry}
	}
	j := []byte{0xFF, 0x24, 0x25}
	j = binary.LittleEndian.AppendUint32(j, uint32(stubasm.TrapPtrs+8*idx))
	if kind == 64 {
		j = append(j, 0x90, 0x90)
	}
	copy(code[pv.off:], j)
	// The Sentry restarts an interrupted syscall by moving the instruction pointer
	// back by 2 (to the syscall instruction, now the middle of the jump): send it to
	// the table entry instead, which re-enters the trampoline with the number.
	as.restartAt[a] = entry
	debugf("imm site %#x: syscall %d via entry %#x", a, sysno, entry)
	cl.add(pv.off, x.off+x.n)
	st.imm++
	return true
}

const sysGetcpu = 309

// vdsoGetcpu is the whole getcpu function of the Sentry's vDSO
// (pkg/sentry/loader/vdsodata): endbr64; mov $309,%eax; syscall; cltq; ret.
var vdsoGetcpu = []byte{0xF3, 0x0F, 0x1E, 0xFA, 0xB8, 0x35, 0x01, 0x00, 0x00, 0x0F, 0x05, 0x48, 0x98, 0xC3}

// isVDSOGetcpu reports whether the mov at off in code is the one of the vDSO's getcpu.
// Only that site is answered in the stub: a getcpu syscall elsewhere keeps its
// system-call semantics (EFAULT for a bad pointer, flags preserved).
func isVDSOGetcpu(code []byte, off int) bool {
	return off >= 4 && off-4+len(vdsoGetcpu) <= len(code) && bytes.Equal(code[off-4:off-4+len(vdsoGetcpu)], vdsoGetcpu)
}

// patchReloc patches a site that is not `mov $imm,%eax; syscall`: the smallest
// window of whole instructions around the syscall in which no instruction except
// the first is a branch target and every instruction can be copied, becomes a jump
// to a veneer.
func (as *addressSpace) patchReloc(code []byte, base uint64, list []ins, s int, tgt []bool, cl *claims, st *siteStats) bool {
	contig := func(i int) bool { return i > 0 && list[i-1].off+list[i-1].n == list[i].off }
	x := list[s]
	a := base + uint64(x.off)
	va := as.vcur
	near := func(from uint64) bool {
		rel := int64(va) - int64(from+5)
		return rel >= -1<<31 && rel < 1<<31
	}
	minLen := 5
	if !near(a) {
		minLen = 7 // out of rel32 reach: jmp *abs32 through a table slot
	}
	// The best window ends with the syscall itself (nothing displaced after it):
	// the syscall then returns into the original text, which is the same in every
	// address space. Only if no such window exists, instructions after the syscall
	// are displaced and the return address is inside the veneer.
	search := func(maxPost int) (bestA, bestB, bestL int) {
		bestA, bestB, bestL = -1, -1, 1<<30
		for ia := s; ia >= 0 && ia >= s-3; ia-- {
			if ia < s && !contig(ia+1) {
				break
			}
			for ib := s; ib < len(list) && ib <= s+maxPost; ib++ {
				if ib > s && !contig(ib) {
					break
				}
				l := list[ib].off + list[ib].n - list[ia].off
				if l < minLen {
					continue
				}
				ok := !cl.taken(list[ia].off, list[ib].off+list[ib].n)
				for k := ia; ok && k <= ib; k++ {
					c := list[k].cls
					if !as.p.fsRewrite && (c == clsFS || c == clsFSBad) {
						c = clsOK // natively an %fs instruction is copied as it is
					}
					if k != s && (c != clsOK && !(c == clsLastOnly && k == ib)) {
						ok = false
						break
					}
					if k > ia && tgt[k] {
						ok = false
						break
					}
				}
				if ok && l < bestL {
					bestA, bestB, bestL = ia, ib, l
				}
			}
		}
		return
	}
	bestA, bestB, bestL := search(0)
	if debugEnabled {
		var ss []int
		for k := range list {
			ss = append(ss, list[k].off, list[k].n)
		}
		debugf("reloc site %#x: list(off,n) %v s=%d window %d..%d len %d (taken: %v)", base+uint64(x.off), ss, s, bestA, bestB, bestL, cl.l)
		debugf("reloc site %#x bytes % x", base+uint64(x.off), code[list[0].off:list[len(list)-1].off+list[len(list)-1].n])
	}
	if bestA < 0 {
		bestA, bestB, bestL = search(3)
	}
	if bestA < 0 {
		return false
	}
	w0 := base + uint64(list[bestA].off)
	wEnd := w0 + uint64(bestL)
	var pre, post []byte
	for k := bestA; k < s; k++ {
		pre = append(pre, code[list[k].off:list[k].off+list[k].n]...)
	}
	for k := s + 1; k <= bestB; k++ {
		post = append(post, code[list[k].off:list[k].off+list[k].n]...)
	}
	// A veneer with displaced instructions after the syscall is where the
	// syscall returns to, and a forked child resumes there: it is placed at an
	// address that depends only on the site, the same in every address space.
	// Veneers without them are only entered from the text, and need no such
	// promise.
	vlen := uint64(len(pre)+23+len(post)+23+15) &^ 15
	if len(post) > 0 {
		va = as.p.sharedVeneer(w0, code[list[bestA].off:list[bestA].off+bestL], vlen)
		if va == 0 {
			return false
		}
		if profkeys.On {
			profkeys.Count("winstub syscall site: shared veneer", 0)
		}
	} else {
		vlen = uint64(len(pre)+23+15) &^ 15
		if va+vlen > as.ftop {
			return false
		}
	}
	var j []byte
	var slot uint64
	if minLen == 5 {
		if !near(w0) {
			return false
		}
		j = []byte{0xE9}
		j = binary.LittleEndian.AppendUint32(j, uint32(int32(int64(va)-int64(w0+5))))
		as.relAt = append(as.relAt, relRec{w0 + 1, va})
	} else {
		idx, ok := as.newSlot()
		if !ok {
			return false
		}
		slot = stubasm.TrapPtrs + 8*idx
		as.areaWrite(slot, binary.LittleEndian.AppendUint64(nil, va))
		j = []byte{0xFF, 0x24, 0x25}
		j = binary.LittleEndian.AppendUint32(j, uint32(stubasm.TrapPtrs+8*idx))
	}
	for len(j) < bestL {
		j = append(j, 0x90)
	}
	postAddr := va + uint64(len(pre)) + 23
	if len(post) == 0 {
		postAddr = wEnd
	}
	v := append([]byte(nil), pre...)
	v = append(v, 0x48, 0xB9) // movabs $post,%rcx
	v = binary.LittleEndian.AppendUint64(v, postAddr)
	v = append(v, 0x49, 0xBB) // movabs $tramp,%r11
	v = binary.LittleEndian.AppendUint64(v, stubasm.StubCode+stubasm.OffTramp)
	v = append(v, 0x41, 0xFF, 0xE3) // jmp *%r11
	if len(post) > 0 {
		v = append(v, post...)
		v = append(v, 0x49, 0xBB) // movabs $end,%r11
		v = binary.LittleEndian.AppendUint64(v, wEnd)
		v = append(v, 0x41, 0xFF, 0xE3)
	}
	as.areaWrite(va, v)
	// What a relocated copy of this patch must change (textreloc_windows.go).
	r := regionOf(w0)
	if len(post) == 0 {
		as.vcur += vlen
		as.absAt = append(as.absAt, absRec{va + uint64(len(pre)) + 2, r})
	} else {
		as.absAt = append(as.absAt, absRec{va + uint64(len(pre)) + 23 + uint64(len(post)) + 2, r})
		as.shared = append(as.shared, sharedRec{w0: w0, va: va, vlen: vlen, post: va + uint64(len(pre)) + 2, slot: slot,
			window: append([]byte(nil), code[list[bestA].off:list[bestA].off+bestL]...)})
	}
	copy(code[list[bestA].off:], j)
	// A restarted syscall re-enters the trampoline from the veneer; the registers
	// before the syscall (rax in particular) are restored by the Sentry.
	as.restartAt[postAddr-2] = va + uint64(len(pre))
	cl.add(list[bestA].off, list[bestA].off+bestL)
	st.reloc++
	return true
}

// veneerRegistry hands out the addresses of the veneers that a syscall returns
// into. A clone child with a new address space starts at the return address of
// its parent, so every address space must put the veneer of a site at the same
// address: the address is chosen per (site address, displaced bytes) once, for
// the whole platform. The space is not reclaimed.
type veneerRegistry struct {
	mu   sync.Mutex
	next uint64
	m    map[veneerKey]uint64
}

type veneerKey struct {
	addr uint64
	hash [32]byte
}

// sharedVeneer returns the address of the veneer for the window at addr with
// the given original bytes, allocating vlen bytes for it on first use; 0 if the
// shared area is full.
func (p *Platform) sharedVeneer(addr uint64, window []byte, vlen uint64) uint64 {
	k := veneerKey{addr: addr, hash: sha256.Sum256(window)}
	r := &p.veneers
	r.mu.Lock()
	defer r.mu.Unlock()
	if va, ok := r.m[k]; ok {
		return va
	}
	if r.m == nil {
		r.m = map[veneerKey]uint64{}
		r.next = stubasm.VeneersShared
	}
	if r.next+vlen > stubasm.Veneers+stubasm.VeneersSize {
		return 0
	}
	va := r.next
	r.next += vlen
	r.m[k] = va
	return va
}
