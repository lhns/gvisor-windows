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

//go:build amd64

package winstub

import (
	"encoding/binary"
	"runtime"
	"sort"
	"sync"

	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/x86asm"
)

// This file is the part of the patcher that depends only on the code bytes (no
// Windows API): decoding, finding syscall and %fs sites, choosing the window of
// instructions a patch replaces, and building the veneers of %fs sites (ADR 0020).
// It builds on every OS so that tools/windows/fsscan can report on Linux binaries.

// Instruction classes for displacing an instruction into a veneer.
const (
	clsOK       = iota // can be copied byte for byte
	clsLastOnly        // ret or indirect jmp: only as the last displaced instruction
	clsNever           // relative branch, call, int, ud2, other rip-relative, ...
	clsFS              // %fs memory access the veneer builder can rewrite
	clsFSBad           // %fs memory access in an encoding it cannot rewrite
	clsRip             // mov/lea of a rip-relative address into a register: relocatable
)

// Kinds of relocatable rip-relative instructions (ins.rkind).
const (
	ripLoad64 = iota + 1
	ripLoad32
	ripLea
)

type ins struct {
	off    int
	n      int
	op     x86asm.Op
	cls    uint8
	hasTgt bool
	tgt    int // branch target as an offset in the scanned range
	// clsRip: the address is off+n+ripDisp, the destination register rdst
	rkind   uint8
	rdst    uint8
	ripDisp int32
}

// scanSite is what the patcher needs to know about one syscall or %fs
// instruction: the instructions around it (up to 3 before and after) and which
// are branch targets. It does not depend on the address the range is mapped at.
type scanSite struct {
	off int
	win [7]ins
	tgt [7]bool
	n   int
	s   int  // index of the site's instruction in win
	fs  bool // an %fs site (otherwise a syscall)
}

type scanResult struct {
	pairs int // 0f 05 byte pairs
	sites []scanSite
	// tail0f: the last byte is 0x0f and starts an instruction (the sweep reaches
	// it): a `syscall` whose second byte is the first byte of the next range.
	tail0f bool
}

// ---- decoding ------------------------------------------------------------------

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
	for off < end {
		inst, err := x86asm.Decode(code[off:], 64)
		if err != nil || inst.Len == 0 {
			off++
			continue
		}
		x := ins{off: off, n: inst.Len, op: inst.Op}
		classify(inst, code[off:off+inst.Len], &x)
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

// classify sets the class of a decoded instruction (b holds its bytes).
func classify(in x86asm.Inst, b []byte, x *ins) {
	if fsMemOp(in) {
		x.cls = clsFSBad
		if rewritableFS(in, b) {
			x.cls = clsFS
		}
		return
	}
	x.cls = clsOK
	if in.PCRel != 0 {
		x.cls = clsNever
		if _, isRel := in.Args[0].(x86asm.Rel); !isRel {
			ripClass(in, x)
		}
		return
	}
	switch in.Op {
	case x86asm.SYSCALL, x86asm.SYSENTER, x86asm.INT, x86asm.CALL, x86asm.LCALL, x86asm.UD1, x86asm.UD2, x86asm.HLT, x86asm.INTO:
		x.cls = clsNever
	case x86asm.RET, x86asm.LRET, x86asm.JMP, x86asm.LJMP:
		x.cls = clsLastOnly
	}
}

// ripClass recognizes `mov (%rip-relative),%reg` and `lea (%rip-relative),%reg`,
// which a veneer can reproduce at any address by loading the absolute address.
func ripClass(in x86asm.Inst, x *ins) {
	dst, ok := in.Args[0].(x86asm.Reg)
	m, ok2 := in.Args[1].(x86asm.Mem)
	if !ok || !ok2 || m.Base != x86asm.RIP || m.Index != 0 || m.Segment != 0 || in.Args[2] != nil {
		return
	}
	switch {
	case in.Op == x86asm.LEA && dst >= x86asm.RAX && dst <= x86asm.R15 && in.DataSize == 64:
		x.rkind, x.rdst = ripLea, uint8(dst-x86asm.RAX)
	case in.Op == x86asm.MOV && dst >= x86asm.RAX && dst <= x86asm.R15 && in.DataSize == 64:
		x.rkind, x.rdst = ripLoad64, uint8(dst-x86asm.RAX)
	case in.Op == x86asm.MOV && dst >= x86asm.EAX && dst <= x86asm.R15L && in.DataSize == 32:
		x.rkind, x.rdst = ripLoad32, uint8(dst-x86asm.EAX)
	default:
		return
	}
	x.cls, x.ripDisp = clsRip, int32(m.Disp)
}

// ---- %fs instructions ---------------------------------------------------------

// fsMemOp reports whether in has a memory operand addressed through %fs (LEA and
// NOP ignore the segment).
func fsMemOp(in x86asm.Inst) bool {
	if in.Op == x86asm.LEA || in.Op == x86asm.NOP {
		return false
	}
	// (the decoder does not always record the segment of a VEX-encoded operand)
	pfx := false
	for _, p := range in.Prefix {
		if p == x86asm.PrefixFS || p == x86asm.PrefixFS|x86asm.PrefixImplicit {
			pfx = true
		}
	}
	for _, a := range in.Args {
		if m, ok := a.(x86asm.Mem); ok && (m.Segment == x86asm.FS || pfx && m.Segment == 0) {
			return true
		}
	}
	return false
}

// fsRaw is an %fs-prefixed instruction taken apart at the byte level.
type fsRaw struct {
	pre         []byte // legacy prefixes except the segment override
	rex         byte   // 0 if there is none
	opc         []byte
	reg         byte // modrm.reg
	regx        int  // reg with its extension bit (REX.R, VEX.R)
	vex         []byte // the VEX prefix (2 or 3 bytes), if any
	vvvv        int    // VEX.vvvv: may name a general register (BMI)
	scale       byte // sib.scale bits
	base, index int  // register numbers, -1 for none
	disp        int32
	tail        []byte // bytes after the displacement (immediates)
}

// parseFS takes apart a legacy-prefixed instruction with one %fs override and a
// register-based memory operand. Everything else (VEX/EVEX, string operations,
// moffs forms, rip-relative operands, address-size overrides) is not supported.
func parseFS(b []byte) (r fsRaw, ok bool) {
	i := 0
	sawFS := false
loop:
	for ; i < len(b); i++ {
		switch b[i] {
		case 0x64:
			sawFS = true
		case 0x66, 0xF0, 0xF2, 0xF3:
			r.pre = append(r.pre, b[i])
		default:
			break loop // includes 0x67 and the other segment prefixes
		}
	}
	if !sawFS || i >= len(b) {
		return r, false
	}
	if b[i]&0xF0 == 0x40 {
		r.rex = b[i]
		i++
	}
	if i >= len(b) {
		return r, false
	}
	c0 := b[i]
	switch c0 {
	case 0x62, 0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xAA, 0xAB, 0xAC, 0xAD, 0xAE, 0xAF, 0xD7, 0x6C, 0x6D, 0x6E, 0x6F:
		return r, false
	case 0xC4, 0xC5:
		n := 2
		if c0 == 0xC4 {
			n = 3
		}
		if len(r.pre) != 0 || i+n+1 > len(b) {
			return r, false
		}
		r.vex = b[i : i+n]
		rbit := int(^b[i+1]>>7) & 1
		if n == 3 {
			r.rex = 0x40 | (b[i+2]>>7)<<3 | byte(rbit)<<2 | (^b[i+1]>>6&1)<<1 | ^b[i+1]>>5&1
			r.vvvv = int(^b[i+2]>>3) & 15
		} else {
			r.vvvv = int(^b[i+1]>>3) & 15
		}
		r.regx = rbit << 3
		r.opc = b[i+n : i+n+1]
		i += n + 1
	case 0x0F:
		if i+1 >= len(b) {
			return r, false
		}
		n := 2
		if b[i+1] == 0x38 || b[i+1] == 0x3A {
			n = 3
		}
		if i+n >= len(b) {
			return r, false
		}
		r.opc = b[i : i+n]
		i += n
	default:
		r.opc = b[i : i+1]
		i++
	}
	if i >= len(b) {
		return r, false
	}
	mod, rm := b[i]>>6, b[i]&7
	r.reg = (b[i] >> 3) & 7
	r.regx |= int(r.reg)
	if len(r.vex) == 0 && r.rex&4 != 0 {
		r.regx |= 8
	}
	i++
	if mod == 3 {
		return r, false
	}
	if len(r.opc) == 2 && r.opc[0] == 0x0F && r.opc[1] == 0xC7 && r.reg == 1 {
		return r, false // cmpxchg8b/16b: implicit rax, rbx, rcx, rdx
	}
	xb, xx := 0, 0
	if r.rex&1 != 0 {
		xb = 8
	}
	if r.rex&2 != 0 {
		xx = 8
	}
	r.base, r.index = -1, -1
	dispLen := 0
	switch mod {
	case 1:
		dispLen = 1
	case 2:
		dispLen = 4
	}
	if rm == 4 {
		if i >= len(b) {
			return r, false
		}
		sib := b[i]
		i++
		r.scale = sib >> 6
		if idx := int(sib>>3)&7 | xx; idx != 4 {
			r.index = idx
		}
		if sib&7 == 5 && mod == 0 {
			dispLen = 4
		} else {
			r.base = int(sib&7) | xb
		}
	} else if rm == 5 && mod == 0 {
		return r, false // rip-relative
	} else {
		r.base = int(rm) | xb
	}
	if i+dispLen > len(b) {
		return r, false
	}
	switch dispLen {
	case 1:
		r.disp = int32(int8(b[i]))
	case 4:
		r.disp = int32(binary.LittleEndian.Uint32(b[i:]))
	}
	i += dispLen
	r.tail = b[i:]
	return r, true
}

func regNum(r x86asm.Reg) int {
	if r == 0 {
		return -1
	}
	if r >= x86asm.RAX && r <= x86asm.R15 {
		return int(r - x86asm.RAX)
	}
	return -2
}

// rewritableFS reports whether the %fs instruction b (decoded as in) can be
// rewritten: the byte-level parse agrees with the decoder's memory operand.
func rewritableFS(in x86asm.Inst, b []byte) bool {
	switch in.Op {
	case x86asm.CALL, x86asm.LCALL, x86asm.JMP, x86asm.LJMP, x86asm.RET, x86asm.LRET, x86asm.XLATB:
		return false
	}
	r, ok := parseFS(b)
	if !ok {
		return false
	}
	for _, a := range in.Args {
		m, isMem := a.(x86asm.Mem)
		if !isMem {
			continue
		}
		if m.Segment != x86asm.FS || regNum(m.Base) != r.base || regNum(m.Index) != r.index || int32(m.Disp) != r.disp { // (the decoder zero-extends a disp32)
			return false
		}
		if r.index >= 0 && int(m.Scale) != 1<<r.scale {
			return false
		}
	}
	return true
}

// scratch picks the register the veneer uses for the base: not one the
// instruction uses (explicitly or, conservatively, implicitly); without a REX
// prefix only registers that need none, so that 8-bit register operands keep
// their meaning.
func (r *fsRaw) scratch() (int, bool) {
	used := uint32(1<<0 | 1<<1 | 1<<2 | 1<<4) // rax, rcx, rdx, rsp
	reg := r.regx
	used |= 1<<reg | 1<<r.vvvv
	if r.rex == 0 && len(r.vex) == 0 && reg >= 4 && reg <= 7 {
		used |= 1 << (reg - 4) // ah, ch, dh, bh: parts of rax, rcx, rdx, rbx
	}
	if r.base >= 0 {
		used |= 1 << r.base
	}
	if r.index >= 0 {
		used |= 1 << r.index
	}
	cands := []int{3, 6, 7, 5}
	if r.rex != 0 {
		cands = append(cands, 8, 9, 10, 11, 12, 13, 14, 15)
	}
	for _, c := range cands {
		if used&(1<<c) == 0 {
			return c, true
		}
	}
	return 0, false
}

// emit encodes the instruction with its memory operand [r1 + index*scale + disp];
// the segment override is dropped.
func (r *fsRaw) emit(r1 int) []byte {
	out := append([]byte(nil), r.pre...)
	idx := 4
	if r.index >= 0 {
		idx = r.index
	}
	if len(r.vex) > 0 {
		out = append(out, r.vex[0])
		if len(r.vex) == 2 {
			out = append(out, r.vex[1]) // (a register above r7 cannot be in it: scratch() does not pick one)
		} else {
			b1 := r.vex[1] | 0x60
			if r.index >= 8 {
				b1 &^= 0x40
			}
			if r1 >= 8 {
				b1 &^= 0x20
			}
			out = append(out, b1, r.vex[2])
		}
	} else if r.rex != 0 || r1 >= 8 {
		rex := byte(0x40) | r.rex&0x0C
		if r.index >= 8 {
			rex |= 2
		}
		if r1 >= 8 {
			rex |= 1
		}
		out = append(out, rex)
	}
	out = append(out, r.opc...)
	out = append(out, 0x80|r.reg<<3|4, r.scale<<6|byte(idx&7)<<3|byte(r1&7))
	out = binary.LittleEndian.AppendUint32(out, uint32(r.disp))
	return append(out, r.tail...)
}

// gsMov encodes `mov %gs:off,r` (load) or `mov r,%gs:off` (store), 64 bit.
func gsMov(load bool, r int, off uint32) []byte {
	op := byte(0x89)
	if load {
		op = 0x8B
	}
	b := []byte{0x65, 0x48 | byte(r>>3)<<2, op, byte(r&7)<<3 | 4, 0x25}
	return binary.LittleEndian.AppendUint32(b, off)
}

// ---- windows --------------------------------------------------------------------

// planFSWindow chooses the instructions list[a..b] (a <= s <= b, contiguous) that
// a jump to a veneer replaces for the %fs instruction list[s]: at least minLen
// bytes, no branch target except at the first instruction, every other
// instruction copyable (ret/jmp only last), nothing free reports as taken.
// The shortest such window wins.
func planFSWindow(list []ins, tgt []bool, s, minLen int, free func(lo, hi int) bool) (a, b int, ok bool) {
	contig := func(i int) bool { return i > 0 && list[i-1].off+list[i-1].n == list[i].off }
	best := 1 << 30
	for ia := s; ia >= 0 && ia >= s-3; ia-- {
		if ia < s && !contig(ia+1) {
			break
		}
		for ib := s; ib < len(list) && ib <= s+3; ib++ {
			if ib > s && !contig(ib) {
				break
			}
			l := list[ib].off + list[ib].n - list[ia].off
			if l < minLen || l >= best {
				continue
			}
			good := true
			for k := ia; k <= ib; k++ {
				c := list[k].cls
				if k != s && c != clsOK && c != clsRip && !(c == clsLastOnly && k == ib) {
					good = false
					break
				}
				if k > ia && tgt[k] {
					good = false
					break
				}
			}
			if good && free(list[ia].off, list[ib].off+list[ib].n) {
				a, b, best, ok = ia, ib, l, true
			}
		}
	}
	return
}

// ---- veneers ------------------------------------------------------------------------

// fsBound maps an instruction start in a veneer to the one it came from.
type fsBound struct{ v, o uint32 }

// fsVeneer is the code for one %fs window (ADR 0020):
//
//	displaced instructions before the %fs one
//	mov %r1,%gs:save            ; the guest's r1
//	mov %gs:fsbase,%r1
//	[lea (%r1,%base),%r1]
//	the instruction with [%r1+...] in place of %fs:...
//	mov %gs:save,%r1
//	displaced instructions after it
//	jmp *0(%rip) ; .quad end of the window
//
// A thread stopped inside it is moved back to the start of the %fs instruction
// (or, once that instruction has completed, forward past it) and r1 restored.
type fsVeneer struct {
	code    []byte
	bnd     []fsBound // every displaced instruction and the %fs one, in code order
	r1      int
	saved   uint32 // code offset of the first instruction after the save
	restore uint32 // code offset of the restore
	rlen    uint32 // its length
	fsO     uint32 // window offset of the %fs instruction
	afterO  uint32 // window offset of what follows it
	winLen  uint32
}

// buildFSVeneer builds the veneer for the window list[a..b] of code, which is
// mapped at base.
func buildFSVeneer(code []byte, base uint64, list []ins, a, b, s int) (*fsVeneer, bool) {
	raw, ok := parseFS(code[list[s].off : list[s].off+list[s].n])
	if !ok {
		return nil, false
	}
	r1, ok := raw.scratch()
	if !ok {
		return nil, false
	}
	w0 := list[a].off
	v := &fsVeneer{r1: r1, fsO: uint32(list[s].off - w0), winLen: uint32(list[b].off + list[b].n - w0)}
	v.afterO = uint32(list[s].off + list[s].n - w0)
	var c []byte
	displace := func(k int) bool {
		x := list[k]
		v.bnd = append(v.bnd, fsBound{uint32(len(c)), uint32(x.off - w0)})
		if x.cls != clsRip {
			c = append(c, code[x.off:x.off+x.n]...)
			return true
		}
		dst := int(x.rdst)
		target := base + uint64(x.off+x.n) + uint64(int64(x.ripDisp))
		c = append(c, 0x48|byte(dst>>3), 0xB8|byte(dst&7))
		c = binary.LittleEndian.AppendUint64(c, target)
		if x.rkind == ripLea {
			return true
		}
		rex := byte(0x40) | byte(dst>>3)<<2 | byte(dst>>3)
		if x.rkind == ripLoad64 {
			rex |= 8
		}
		if rex != 0x40 {
			c = append(c, rex)
		}
		c = append(c, 0x8B, 0x80|byte(dst&7)<<3|byte(dst&7))
		if dst&7 == 4 {
			c = append(c, 0x24)
		}
		c = binary.LittleEndian.AppendUint32(c, 0)
		return true
	}
	for k := a; k < s; k++ {
		displace(k)
	}
	v.bnd = append(v.bnd, fsBound{uint32(len(c)), v.fsO})
	c = append(c, gsMov(false, r1, stubasm.TebSave)...)
	v.saved = uint32(len(c))
	c = append(c, gsMov(true, r1, stubasm.TebFS)...)
	if raw.base >= 0 {
		// lea (%r1,%base),%r1; rsp cannot be an index, so it becomes the base.
		sb, si := r1, raw.base
		if raw.base == 4 {
			sb, si = 4, r1
		}
		rex := byte(0x48) | byte(r1>>3)<<2 | byte(si>>3)<<1 | byte(sb>>3)
		c = append(c, rex, 0x8D, 0x40|byte(r1&7)<<3|4, byte(si&7)<<3|byte(sb&7), 0)
	}
	c = append(c, raw.emit(r1)...)
	v.restore = uint32(len(c))
	c = append(c, gsMov(true, r1, stubasm.TebSave)...)
	v.rlen = uint32(len(c)) - v.restore
	for k := s + 1; k <= b; k++ {
		displace(k)
	}
	v.bnd = append(v.bnd, fsBound{uint32(len(c)), v.winLen})
	c = append(c, 0xFF, 0x25, 0, 0, 0, 0)
	c = binary.LittleEndian.AppendUint64(c, base+uint64(w0)+uint64(v.winLen))
	v.code = c
	return v, true
}

// ---- scanning -------------------------------------------------------------------

// scanText finds every syscall instruction in code and its surroundings, and,
// with wantFS, every %fs instruction.
func scanText(code []byte, from int, wantFS bool) *scanResult {
	r := &scanResult{}
	for i := 0; i+1 < len(code); i++ {
		if code[i] == 0x0F && code[i+1] == 0x05 {
			r.pairs++
		}
	}
	if !wantFS && r.pairs == 0 && (len(code) == 0 || code[len(code)-1] != 0x0F) {
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
		isFS := x.cls == clsFS || x.cls == clsFSBad
		if x.op != x86asm.SYSCALL && !(wantFS && isFS) {
			continue
		}
		lo, hi := max(0, s-3), min(len(list)-1, s+3)
		site := scanSite{off: x.off, n: hi - lo + 1, s: s - lo, fs: x.op != x86asm.SYSCALL}
		for k := lo; k <= hi; k++ {
			site.win[k-lo] = list[k]
			site.tgt[k-lo] = list[k].off < len(code) && isTgt[list[k].off/64]&(1<<(list[k].off%64)) != 0
		}
		r.sites = append(r.sites, site)
	}
	return r
}

// FSClassCount counts %fs instructions by how a patch can reach them.
type FSClassCount struct {
	Direct int // the instruction itself is long enough for the jump
	Window int // shorter, but a window of neighbouring instructions fits
	None   int // neither
}

// FSReport is the result of ScanFS.
type FSReport struct {
	Total    int // %fs memory instructions
	Unsupp   int // of those, in an encoding the rewrite does not handle
	Rip      int // supported ones whose best window relocates a rip-relative load
	Near, Far FSClassCount // jump rel32 (5 bytes), jump through a low slot (7 bytes)
}

// ScanFS counts the %fs instructions of code by class (see FSClassCount): Near
// when the veneer is within rel32 reach of the code, Far otherwise.
func ScanFS(code []byte) FSReport {
	var rep FSReport
	scan := scanText(code, 0, true)
	for i := range scan.sites {
		site := &scan.sites[i]
		if !site.fs {
			continue
		}
		rep.Total++
		x := site.win[site.s]
		if x.cls == clsFSBad {
			rep.Unsupp++
			continue
		}
		win, tgt := site.win[:site.n], site.tgt[:site.n]
		for j, minLen := range []int{5, 7} {
			cc := &rep.Near
			if j == 1 {
				cc = &rep.Far
			}
			if x.n >= minLen {
				cc.Direct++
				continue
			}
			a, b, ok := planFSWindow(win, tgt, site.s, minLen, func(int, int) bool { return true })
			if !ok {
				cc.None++
				continue
			}
			cc.Window++
			if j == 1 {
				for k := a; k <= b; k++ {
					if win[k].cls == clsRip {
						rep.Rip++
						break
					}
				}
			}
		}
	}
	return rep
}
