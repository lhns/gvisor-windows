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
	"crypto/sha256"
	"encoding/binary"
	"runtime"
	"sort"
	"sync"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/x86asm"
)

// siteStats counts what the scanner found and did.
type siteStats struct {
	pairs    int // 0f 05 byte pairs in the scanned range
	syscalls int // decoded SYSCALL instructions
	imm      int // mov $imm32,%eax|%rax; syscall -> jmp *slot
	reloc    int // any other site -> jump to a per-site veneer
	ud2      int // left as ud2 (debugger path)
}

// Instruction classes for displacing an instruction into a veneer.
const (
	clsOK       = iota // can be copied byte for byte
	clsLastOnly        // ret or indirect jmp: only as the last displaced instruction
	clsNever           // rip-relative, relative branch, call, int, ud2, ...
)

type ins struct {
	off    int
	n      int
	op     x86asm.Op
	cls    uint8
	hasTgt bool
	tgt    int // branch target as an offset in the scanned range
}

// scanSite is what the patcher needs to know about one syscall instruction: the
// instructions around it (up to 3 before and after) and which are branch targets.
// It does not depend on the address the range is mapped at.
type scanSite struct {
	off int
	win [7]ins
	tgt [7]bool
	n   int
	s   int // index of the syscall in win
}

type scanResult struct {
	pairs int // 0f 05 byte pairs
	sites []scanSite
	// tail0f: the last byte is 0x0f and starts an instruction (the sweep reaches
	// it): a `syscall` whose second byte is the first byte of the next range.
	tail0f bool
}

// decodeRange decodes code linearly from from to end (instructions that start
// before end), skipping bytes that do not decode.
func decodeRange(code []byte, from, end int, list []ins) []ins {
	for from < end {
		var done bool
		list, from, done = decodeRangeSafe(code, from, end, list)
		if !done {
			from++ // the decoder panicked here (a truncated instruction at the end of the range)
		}
	}
	return list
}

// decodeRangeSafe is decodeRange up to a panic of the decoder; it returns the
// offset of the instruction being decoded and false in that case.
func decodeRangeSafe(code []byte, from, end int, list []ins) (res []ins, at int, done bool) {
	off := from
	defer func() {
		if r := recover(); r != nil {
			res, at, done = list, off, false
		}
	}()
	for ; off < end; {
		inst, err := x86asm.Decode(code[off:], 64)
		if err != nil || inst.Len == 0 {
			off++
			continue
		}
		x := ins{off: off, n: inst.Len, op: inst.Op, cls: classify(inst)}
		if rel, ok := inst.Args[0].(x86asm.Rel); ok {
			x.hasTgt, x.tgt = true, off+inst.Len+int(rel)
		}
		list = append(list, x)
		off += inst.Len
	}
	return list, off, true
}

// decodeAll is the linear sweep over the whole range, split over several
// goroutines: every chunk after the first is decoded from its start, which is
// usually not an instruction boundary; the results are joined where a later
// chunk's decoding agrees with the true stream (it almost always does within a few
// instructions), and the stream is decoded serially until then.
func decodeAll(code []byte) []ins {
	const minChunk = 128 << 10
	workers := min(runtime.GOMAXPROCS(0), len(code)/minChunk)
	if workers < 2 {
		return decodeRange(code, 0, len(code), make([]ins, 0, len(code)/4))
	}
	cs := (len(code) + workers - 1) / workers
	parts := make([][]ins, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			lo, hi := w*cs, min((w+1)*cs, len(code))
			parts[w] = decodeRange(code, lo, hi, make([]ins, 0, cs/4))
		}(w)
	}
	wg.Wait()
	list := parts[0]
	pos := 0
	if n := len(list); n > 0 {
		pos = list[n-1].off + list[n-1].n
	}
	for w := 1; w < workers; w++ {
		end := min((w+1)*cs, len(code))
		part := parts[w]
		for pos < end {
			i := sort.Search(len(part), func(i int) bool { return part[i].off >= pos })
			if i < len(part) && part[i].off == pos {
				list = append(list, part[i:]...)
				last := part[len(part)-1]
				pos = last.off + last.n
				break
			}
			one := decodeRange(code, pos, pos+1, nil)
			if len(one) == 0 {
				pos++
				continue
			}
			list = append(list, one[0])
			pos += one[0].n
		}
	}
	return list
}

// scanText finds every syscall instruction in code and its surroundings.
func scanText(code []byte, from int) *scanResult {
	r := &scanResult{}
	for i := 0; i+1 < len(code); i++ {
		if code[i] == 0x0F && code[i+1] == 0x05 {
			r.pairs++
		}
	}
	if r.pairs == 0 && (len(code) == 0 || code[len(code)-1] != 0x0F) {
		return r
	}
	list := decodeAll(code[from:])
	for i := range list {
		list[i].off += from
		list[i].tgt += from
	}
	end, lastStart := from, -1
	if n := len(list); n > 0 {
		end, lastStart = list[n-1].off+list[n-1].n, list[n-1].off
	}
	// (the decoder may or may not report a lone 0x0f as an instruction)
	r.tail0f = len(code) > 0 && (end == len(code)-1 || lastStart == len(code)-1) && code[len(code)-1] == 0x0F
	isTgt := make([]uint64, (len(code)+63)/64)
	for _, x := range list {
		if x.hasTgt && x.tgt >= 0 && x.tgt < len(code) {
			isTgt[x.tgt/64] |= 1 << (x.tgt % 64)
		}
	}
	for s, x := range list {
		if x.op != x86asm.SYSCALL {
			continue
		}
		lo, hi := max(0, s-3), min(len(list)-1, s+3)
		site := scanSite{off: x.off, n: hi - lo + 1, s: s - lo}
		for k := lo; k <= hi; k++ {
			site.win[k-lo] = list[k]
			site.tgt[k-lo] = list[k].off < len(code) && isTgt[list[k].off/64]&(1<<(list[k].off%64)) != 0
		}
		r.sites = append(r.sites, site)
	}
	return r
}

// scanCache remembers scan results by content: the same library text is mapped
// again by every process.
type scanCache struct {
	mu sync.Mutex
	m  map[[32]byte]*scanResult
}

// scan scans code, starting at offset from (1 when the first byte is the second
// half of a syscall instruction that begins in the previous range).
func (c *scanCache) scan(code []byte, from int) *scanResult {
	h := sha256.New()
	h.Write(code)
	h.Write([]byte{byte(from)})
	var key [32]byte
	h.Sum(key[:0])
	c.mu.Lock()
	r := c.m[key]
	c.mu.Unlock()
	if r != nil {
		return r
	}
	r = scanText(code, from)
	c.mu.Lock()
	if c.m == nil || len(c.m) >= 512 {
		c.m = map[[32]byte]*scanResult{}
	}
	c.m[key] = r
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
func (as *addressSpace) patchCode(code []byte, base uint64, from int) (siteStats, *scanResult) {
	var st siteStats
	scan := as.p.scans.scan(code, from)
	st.pairs = scan.pairs
	for i := range scan.sites {
		site := &scan.sites[i]
		st.syscalls++
		win, tgt := site.win[:site.n], site.tgt[:site.n]
		if as.patchImm(code, base, win, site.s, tgt, &st) {
			continue
		}
		if as.patchReloc(code, base, win, site.s, tgt, &st) {
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

func (as *addressSpace) patchImm(code []byte, base uint64, list []ins, s int, tgt []bool, st *siteStats) bool {
	x := list[s]
	a := base + uint64(x.off)
	if s == 0 || list[s-1].off+list[s-1].n != x.off || tgt[s] || list[s-1].op != x86asm.MOV {
		return false
	}
	pv := list[s-1]
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
		e := []byte{0xB8}
		e = binary.LittleEndian.AppendUint32(e, sysno)
		e = append(e, 0x48, 0xB9)
		e = binary.LittleEndian.AppendUint64(e, a+2)
		e = append(e, 0x49, 0xBB)
		e = binary.LittleEndian.AppendUint64(e, stubasm.StubCode+stubasm.OffTramp)
		e = append(e, 0x41, 0xFF, 0xE3)
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
	st.imm++
	return true
}

// patchReloc patches a site that is not `mov $imm,%eax; syscall`: the smallest
// window of whole instructions around the syscall in which no instruction except
// the first is a branch target and every instruction can be copied, becomes a jump
// to a veneer.
func (as *addressSpace) patchReloc(code []byte, base uint64, list []ins, s int, tgt []bool, st *siteStats) bool {
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
				ok := true
				for k := ia; k <= ib; k++ {
					if k != s && (list[k].cls == clsNever || (list[k].cls == clsLastOnly && k != ib)) {
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
	} else {
		vlen = uint64(len(pre)+23+15) &^ 15
		if va+vlen > stubasm.VeneersShared {
			return false
		}
	}
	var j []byte
	if minLen == 5 {
		if !near(w0) {
			return false
		}
		j = []byte{0xE9}
		j = binary.LittleEndian.AppendUint32(j, uint32(int32(int64(va)-int64(w0+5))))
	} else {
		idx, ok := as.newSlot()
		if !ok {
			return false
		}
		as.areaWrite(stubasm.TrapPtrs+8*idx, binary.LittleEndian.AppendUint64(nil, va))
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
	if len(post) == 0 {
		as.vcur += vlen
	}
	copy(code[list[bestA].off:], j)
	// A restarted syscall re-enters the trampoline from the veneer; the registers
	// before the syscall (rax in particular) are restored by the Sentry.
	as.restartAt[postAddr-2] = va + uint64(len(pre))
	st.reloc++
	return true
}

func classify(in x86asm.Inst) uint8 {
	if in.PCRel != 0 {
		return clsNever
	}
	switch in.Op {
	case x86asm.SYSCALL, x86asm.SYSENTER, x86asm.INT, x86asm.CALL, x86asm.LCALL, x86asm.UD1, x86asm.UD2, x86asm.HLT, x86asm.INTO:
		return clsNever
	case x86asm.RET, x86asm.LRET, x86asm.JMP, x86asm.LJMP:
		return clsLastOnly
	}
	return clsOK
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
