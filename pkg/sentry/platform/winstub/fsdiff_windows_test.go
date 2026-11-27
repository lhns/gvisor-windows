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
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	pkgcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
)

// Differential test of the %fs rewrite (ADR 0020): random %fs instructions of
// every addressing form glibc and the compilers produce run once with the CPU's
// %fs base (native mode) and once rewritten into veneers; registers, flags,
// vector registers and memory afterwards must be identical. The instruction sits
// between random neighbours (so that windows are used) and the registers are set
// so that the operand lands in a data page, whatever its base, index and
// displacement are.

const (
	fdText    = 0xC00000
	fdFar     = 0x3_0000_0000
	fdFS      = 0x910000 // the %fs base: the middle of a data area
	fdArea    = 0x900000
	fdAreaLen = 0x20000
	fdStack   = 0x700000
	fdCaseMax = 256 // bytes reserved for a case in the text
)

type fdOp struct {
	op   []byte
	ext  int    // -1: the reg field is an operand, else the /digit
	imm  int    // immediate bytes: 0, 1 or 4 (2 with a 0x66 prefix)
	kind byte   // 'i' integer (operand size 16/32/64), 'b' byte, 'x' SSE, 's' integer, any size, reg from the SSE side
	lock bool   // may carry a lock prefix (memory destination)
	pfx  []byte // 'x': mandatory prefixes to pick from (0 = none)
	w    bool   // 'x': REX.W allowed
}

func fdOps() []fdOp {
	var ops []fdOp
	alu := []byte{0x00, 0x08, 0x10, 0x18, 0x20, 0x28, 0x30, 0x38}
	for i, b := range alu {
		lock := i != 7
		ops = append(ops, fdOp{op: []byte{b + 1}, ext: -1, kind: 'i', lock: lock}, fdOp{op: []byte{b + 3}, ext: -1, kind: 'i'},
			fdOp{op: []byte{b}, ext: -1, kind: 'b', lock: lock}, fdOp{op: []byte{b + 2}, ext: -1, kind: 'b'})
		ops = append(ops, fdOp{op: []byte{0x81}, ext: i, imm: 4, kind: 'i', lock: lock}, fdOp{op: []byte{0x83}, ext: i, imm: 1, kind: 'i', lock: lock},
			fdOp{op: []byte{0x80}, ext: i, imm: 1, kind: 'b', lock: lock})
	}
	ops = append(ops,
		fdOp{op: []byte{0x89}, ext: -1, kind: 'i'}, fdOp{op: []byte{0x8B}, ext: -1, kind: 'i'},
		fdOp{op: []byte{0x88}, ext: -1, kind: 'b'}, fdOp{op: []byte{0x8A}, ext: -1, kind: 'b'},
		fdOp{op: []byte{0x85}, ext: -1, kind: 'i'}, fdOp{op: []byte{0x84}, ext: -1, kind: 'b'},
		fdOp{op: []byte{0x87}, ext: -1, kind: 'i', lock: true}, fdOp{op: []byte{0x86}, ext: -1, kind: 'b', lock: true},
		fdOp{op: []byte{0xC7}, ext: 0, imm: 4, kind: 'i'}, fdOp{op: []byte{0xC6}, ext: 0, imm: 1, kind: 'b'},
		fdOp{op: []byte{0xF7}, ext: 0, imm: 4, kind: 'i'}, fdOp{op: []byte{0xF6}, ext: 0, imm: 1, kind: 'b'},
		fdOp{op: []byte{0xF7}, ext: 2, kind: 'i', lock: true}, fdOp{op: []byte{0xF7}, ext: 3, kind: 'i', lock: true},
		fdOp{op: []byte{0xF7}, ext: 4, kind: 'i'}, fdOp{op: []byte{0xF7}, ext: 5, kind: 'i'},
		fdOp{op: []byte{0xF6}, ext: 3, kind: 'b', lock: true}, fdOp{op: []byte{0xF6}, ext: 4, kind: 'b'},
		fdOp{op: []byte{0xFF}, ext: 0, kind: 'i', lock: true}, fdOp{op: []byte{0xFF}, ext: 1, kind: 'i', lock: true},
		fdOp{op: []byte{0xFE}, ext: 0, kind: 'b', lock: true}, fdOp{op: []byte{0xFE}, ext: 1, kind: 'b', lock: true},
		fdOp{op: []byte{0x0F, 0xAF}, ext: -1, kind: 'i'}, fdOp{op: []byte{0x69}, ext: -1, imm: 4, kind: 'i'}, fdOp{op: []byte{0x6B}, ext: -1, imm: 1, kind: 'i'},
		fdOp{op: []byte{0x0F, 0xB1}, ext: -1, kind: 'i', lock: true}, fdOp{op: []byte{0x0F, 0xB0}, ext: -1, kind: 'b', lock: true},
		fdOp{op: []byte{0x0F, 0xC1}, ext: -1, kind: 'i', lock: true}, fdOp{op: []byte{0x0F, 0xC0}, ext: -1, kind: 'b', lock: true},
		fdOp{op: []byte{0x0F, 0xBA}, ext: 4, imm: 1, kind: 'i'}, fdOp{op: []byte{0x0F, 0xBA}, ext: 5, imm: 1, kind: 'i', lock: true},
		fdOp{op: []byte{0x0F, 0xBC}, ext: -1, kind: 'i'}, fdOp{op: []byte{0x0F, 0xBD}, ext: -1, kind: 'i'},
		fdOp{op: []byte{0x0F, 0xB6}, ext: -1, kind: 'i'}, fdOp{op: []byte{0x0F, 0xB7}, ext: -1, kind: 'i'},
		fdOp{op: []byte{0x0F, 0xBE}, ext: -1, kind: 'i'}, fdOp{op: []byte{0x0F, 0xBF}, ext: -1, kind: 'i'},
		fdOp{op: []byte{0x0F, 0xB8}, ext: -1, kind: 'i', pfx: []byte{0xF3}},
		fdOp{op: []byte{0xC1}, ext: 0, imm: 1, kind: 'i'}, fdOp{op: []byte{0xC1}, ext: 4, imm: 1, kind: 'i'}, fdOp{op: []byte{0xC1}, ext: 7, imm: 1, kind: 'i'},
		fdOp{op: []byte{0xC0}, ext: 5, imm: 1, kind: 'b'}, fdOp{op: []byte{0xD1}, ext: 1, kind: 'i'}, fdOp{op: []byte{0xD3}, ext: 4, kind: 'i'}, fdOp{op: []byte{0xD2}, ext: 5, kind: 'b'},
		fdOp{op: []byte{0x63}, ext: -1, kind: 'i'},
	)
	for cc := byte(0); cc < 16; cc += 3 {
		ops = append(ops, fdOp{op: []byte{0x0F, 0x40 + cc}, ext: -1, kind: 'i'}, fdOp{op: []byte{0x0F, 0x90 + cc}, ext: 0, kind: 'b'})
	}
	n, p, f3, f2 := []byte{0}, []byte{0x66}, []byte{0xF3}, []byte{0xF2}
	sse := func(op []byte, ext int, imm int, w bool, pfx ...[]byte) {
		var all []byte
		for _, x := range pfx {
			all = append(all, x[0])
		}
		ops = append(ops, fdOp{op: op, ext: ext, imm: imm, kind: 'x', pfx: all, w: w})
	}
	sse([]byte{0x0F, 0x10}, -1, 0, false, n, p, f3, f2)
	sse([]byte{0x0F, 0x11}, -1, 0, false, n, p, f3, f2)
	sse([]byte{0x0F, 0x28}, -1, 0, false, n, p)
	sse([]byte{0x0F, 0x29}, -1, 0, false, n, p)
	sse([]byte{0x0F, 0x6F}, -1, 0, false, p, f3)
	sse([]byte{0x0F, 0x7F}, -1, 0, false, p, f3)
	sse([]byte{0x0F, 0xD6}, -1, 0, false, p)
	sse([]byte{0x0F, 0x12}, -1, 0, false, n, p)
	sse([]byte{0x0F, 0x17}, -1, 0, false, n, p)
	sse([]byte{0x0F, 0x54}, -1, 0, false, n, p)
	sse([]byte{0x0F, 0x58}, -1, 0, false, n, p, f3, f2)
	sse([]byte{0x0F, 0x59}, -1, 0, false, n, p, f3, f2)
	sse([]byte{0x0F, 0xEF}, -1, 0, false, p)
	sse([]byte{0x0F, 0xFC}, -1, 0, false, p)
	sse([]byte{0x0F, 0x6E}, -1, 0, true, p)
	sse([]byte{0x0F, 0x7E}, -1, 0, true, p)
	sse([]byte{0x0F, 0x7E}, -1, 0, false, f3)
	sse([]byte{0x0F, 0x70}, -1, 1, false, p, f3, f2)
	sse([]byte{0x0F, 0xC6}, -1, 1, false, n, p)
	sse([]byte{0x0F, 0x3A, 0x0F}, -1, 1, false, p)
	sse([]byte{0x0F, 0x38, 0x00}, -1, 0, false, p)
	sse([]byte{0x0F, 0x2E}, -1, 0, false, n, p)
	sse([]byte{0x0F, 0x2A}, -1, 0, true, f3, f2)
	ops = append(ops, fdOp{op: []byte{0x0F, 0x2C}, ext: -1, kind: 's', pfx: []byte{0xF3, 0xF2}, w: true})
	return ops
}

type fdMem struct {
	base, index int // register numbers, -1 for none
	scale       int // 0..3
	disp        int32
	big         bool // a disp32 where a disp8 would do
}

// fdGen builds random %fs instructions.
type fdGen struct {
	r    *rand.Rand
	ops  []fdOp
	bmi  bool
	data uint64 // the words the rip-relative neighbours load
	loop bool   // cases run in a loop: nothing may change the registers of the address
}

// insn encodes a random instruction with the memory operand m (placed already)
// and returns it with the registers used as operands (so that neighbours leave
// them alone).
func (g *fdGen) insn(m fdMem) (code []byte, used []int) {
	r := g.r
	if g.bmi && r.Intn(10) == 0 {
		return g.vex(m)
	}
	o := g.ops[r.Intn(len(g.ops))]
	var legacy []byte
	size := 0 // 16, 32, 64 or 0 for byte/SSE
	rexW := false
	switch o.kind {
	case 'i', 's':
		switch r.Intn(5) {
		case 0:
			size = 16
		case 1, 2:
			size = 64
		default:
			size = 32
		}
		if o.op[0] == 0x63 || o.kind == 's' {
			size = 64 * r.Intn(2) // movsxd and cvttss2si: 32 or 64
			if o.op[0] == 0x63 && size == 0 {
				size = 64
			}
			if size == 0 {
				size = 32
			}
		}
		rexW = size == 64
		if len(o.pfx) > 0 && (size == 16 || o.kind == 'i') {
			size = max(size, 32)
		}
		if size == 16 && o.op[0] == 0x0F && o.op[1] == 0xB8 {
			size = 32
		}
	case 'x':
		if o.w {
			rexW = r.Intn(2) == 0
		}
	}
	if size == 16 {
		legacy = append(legacy, 0x66)
	}
	if len(o.pfx) > 0 {
		if p := o.pfx[r.Intn(len(o.pfx))]; p != 0 {
			legacy = append(legacy, p)
		}
	}
	if o.lock && r.Intn(2) == 0 {
		legacy = append(legacy, 0xF0)
	}
	legacy = append(legacy, 0x64)
	r.Shuffle(len(legacy), func(i, j int) { legacy[i], legacy[j] = legacy[j], legacy[i] })
	reg := o.ext
	byteRegNoREX := false
	if reg < 0 {
		reg = r.Intn(16)
		if reg == 4 {
			reg = 0
		}
	}
	forceREX := r.Intn(8) == 0
	needREX := rexW || reg >= 8 && o.ext < 0 || m.base >= 8 || m.index >= 8 || forceREX
	if o.kind == 'b' && !needREX && o.ext < 0 && reg >= 4 && reg <= 7 {
		byteRegNoREX = true // ah..bh: a register that needs no REX stays so
	}
	if o.kind == 'b' && o.ext < 0 && reg == 4 && !byteRegNoREX {
		reg = 3 // (spl would change rsp)
	}
	if o.kind == 'b' && !needREX && reg >= 8 {
		needREX = true
	}
	if g.loop && o.ext < 0 {
		eff := reg
		if byteRegNoREX {
			eff = reg - 4
		}
		if eff == m.base || eff == m.index {
			return g.insn(m) // the instruction would change its own address
		}
	}
	var rex byte
	if needREX {
		rex = 0x40
		if rexW {
			rex |= 8
		}
		if reg >= 8 && o.ext < 0 {
			rex |= 4
		}
		if m.index >= 8 {
			rex |= 2
		}
		if m.base >= 8 {
			rex |= 1
		}
	}
	out := append([]byte(nil), legacy...)
	if rex != 0 {
		out = append(out, rex)
	}
	out = append(out, o.op...)
	out = append(out, fdModRM(reg&7, m)...)
	switch o.imm {
	case 1:
		out = append(out, byte(r.Intn(256)))
	case 4:
		if size == 16 {
			out = binary.LittleEndian.AppendUint16(out, uint16(r.Uint32()))
		} else {
			out = binary.LittleEndian.AppendUint32(out, r.Uint32())
		}
	}
	if o.ext < 0 {
		used = []int{reg}
	}
	return out, used
}

// vex builds an %fs-prefixed BMI instruction (VEX encoded: no legacy prefix is
// possible, the segment override comes first).
func (g *fdGen) vex(m fdMem) (code []byte, used []int) {
	r := g.r
	type v struct {
		mmmmm, pp byte
		op        byte
		imm       bool
		ext       int // -1: reg is an operand and vvvv a register, else /digit (vvvv is the destination)
	}
	forms := []v{{2, 1, 0xF7, false, -1}, {2, 2, 0xF7, false, -1}, {2, 3, 0xF7, false, -1}, {2, 0, 0xF7, false, -1}, {2, 0, 0xF2, false, -1},
		{2, 3, 0xF6, false, -1}, {2, 0, 0xF5, false, -1}, {2, 3, 0xF5, false, -1}, {2, 2, 0xF5, false, -1}, {3, 3, 0xF0, true, -1},
		{2, 0, 0xF3, false, 1}, {2, 0, 0xF3, false, 2}, {2, 0, 0xF3, false, 3}}
	f := forms[r.Intn(len(forms))]
	pick := func() int {
		for {
			x := r.Intn(16)
			if x != 4 && x != m.base && x != m.index {
				return x
			}
		}
	}
	reg, vvvv := pick(), pick()
	if f.imm {
		vvvv = 0 // rorx: vvvv must be unused (1111b)
	}
	if f.ext >= 0 {
		reg = f.ext
	}
	w := byte(r.Intn(2))
	b1 := 0xE0 | f.mmmmm // ~R ~X ~B set: no extension
	if f.ext < 0 && reg >= 8 {
		b1 &^= 0x80
	}
	if m.index >= 8 {
		b1 &^= 0x40
	}
	if m.base >= 8 {
		b1 &^= 0x20
	}
	b2 := w<<7 | byte(^vvvv&15)<<3 | f.pp
	out := []byte{0x64, 0xC4, b1, b2, f.op}
	out = append(out, fdModRM(reg&7, m)...)
	if f.imm {
		out = append(out, byte(r.Intn(64)))
	}
	return out, []int{reg, vvvv}
}

func (g *fdGen) mem() fdMem {
	r := g.r
	m := fdMem{base: -1, index: -1}
	switch r.Intn(10) {
	case 0: // absolute
	case 1: // index only
		m.index = g.gpr()
	case 2, 3, 4, 5:
		m.base = g.gpr()
	default:
		m.base = g.gpr()
		m.index = g.gpr()
		for m.index == m.base {
			m.index = g.gpr()
		}
	}
	m.scale = r.Intn(4)
	return m
}

// gpr returns a register other than rsp.
func (g *fdGen) gpr() int {
	for {
		// (in a loop the address registers must not be changed by the implicit uses of rax, rcx and rdx)
		if x := g.r.Intn(16); x != 4 && !(g.loop && x <= 2) {
			return x
		}
	}
}

// fdModRM encodes the modrm byte, sib and displacement for m. m.disp is set by
// the caller for the final layout (see place); here only its size matters.
func fdModRM(reg int, m fdMem) []byte {
	disp := m.disp
	dl := 4
	mod := byte(2)
	switch {
	case m.base < 0:
		mod = 0
	case disp == 0 && m.base&7 != 5 && !m.big:
		mod, dl = 0, 0
	case disp >= -128 && disp <= 127 && !m.big:
		mod, dl = 1, 1
	}
	var out []byte
	switch {
	case m.base < 0 && m.index < 0: // absolute: sib with no base and no index
		out = []byte{byte(reg)<<3 | 4, 0x25}
	case m.base < 0:
		out = []byte{byte(reg)<<3 | 4, byte(m.scale)<<6 | byte(m.index&7)<<3 | 5}
	case m.index < 0 && m.base&7 != 4:
		out = []byte{mod<<6 | byte(reg)<<3 | byte(m.base&7)}
	case m.index < 0:
		out = []byte{mod<<6 | byte(reg)<<3 | 4, 0x24 | 0<<6 | byte(m.base&7)}
		out[1] = byte(4)<<3 | byte(m.base&7)
	default:
		out = []byte{mod<<6 | byte(reg)<<3 | 4, byte(m.scale)<<6 | byte(m.index&7)<<3 | byte(m.base&7)}
	}
	switch dl {
	case 1:
		out = append(out, byte(int8(disp)))
	case 4:
		out = binary.LittleEndian.AppendUint32(out, uint32(disp))
	}
	return out
}

// place decides the displacement and the register values so that the operand
// (without the %fs base) is the offset t from the %fs base. It returns the
// register values to set (-1 for none).
func (g *fdGen) place(m *fdMem, t int64) (base, index uint64) {
	r := g.r
	var iv int64
	if m.index >= 0 {
		iv = int64(r.Intn(17)) - 8
	}
	scaled := iv << uint(m.scale)
	switch {
	case m.base < 0:
		m.disp = int32(t - scaled)
		m.big = r.Intn(4) == 0
	default:
		switch r.Intn(5) {
		case 0:
			m.disp = 0
		case 1:
			m.disp = int32(r.Intn(256) - 128)
		case 2:
			m.disp = int32(r.Intn(0x7FFFFF) - 0x400000)
		case 3:
			m.disp = int32(-8 * (1 + r.Intn(40)))
		default:
			m.disp = int32(r.Uint32())
		}
		m.big = m.disp >= -128 && m.disp <= 127 && r.Intn(4) == 0
		base = uint64(t - scaled - int64(m.disp))
	}
	index = uint64(iv)
	return
}

// A case: code from the case's start, and its initial state.
type fdCase struct {
	code  []byte
	regs  [16]uint64
	flags uint64
	xmm   [256]byte
	desc  string
	insn  []byte
}

// neighbours are instructions that do not touch the registers in keep.
func (g *fdGen) neighbour(pc uint64, keep map[int]bool) []byte {
	r := g.r
	reg := func() int {
		for {
			x := r.Intn(16)
			if x != 4 && !keep[x] {
				return x
			}
		}
	}
	rexb := func(w bool, rr, b int) []byte {
		x := byte(0x40)
		if w {
			x |= 8
		}
		if rr >= 8 {
			x |= 4
		}
		if b >= 8 {
			x |= 1
		}
		if x == 0x40 {
			return nil
		}
		return []byte{x}
	}
	switch r.Intn(9) {
	case 0: // mov $imm32,%r32
		d := reg()
		return append(append(rexb(false, 0, d), 0xB8|byte(d&7)), le32(r.Uint32())...)
	case 1: // add $imm8,%r64
		d := reg()
		return append(rexb(true, 0, d), 0x83, 0xC0|byte(d&7), byte(r.Intn(256)))
	case 2: // mov %r64,%r64
		a, b := reg(), reg()
		return append(rexb(true, b, a), 0x89, 0xC0|byte(b&7)<<3|byte(a&7))
	case 3: // xor %r32,%r32
		d := reg()
		return append(rexb(false, d, d), 0x31, 0xC0|byte(d&7)<<3|byte(d&7))
	case 4: // lea disp(%rip),%r64 (ends at pc+7)
		d := reg()
		out := append(rexb(true, d, 0), 0x8D, byte(d&7)<<3|5)
		return binary.LittleEndian.AppendUint32(out, uint32(int32(int64(g.data+8*uint64(r.Intn(8)))-int64(pc)-int64(len(out)+4))))
	case 5: // mov disp(%rip),%r64
		d := reg()
		out := append(rexb(true, d, 0), 0x8B, byte(d&7)<<3|5)
		return binary.LittleEndian.AppendUint32(out, uint32(int32(int64(g.data+8*uint64(r.Intn(8)))-int64(pc)-int64(len(out)+4))))
	case 6: // mov disp(%rip),%r32
		d := reg()
		out := append(rexb(false, d, 0), 0x8B, byte(d&7)<<3|5)
		return binary.LittleEndian.AppendUint32(out, uint32(int32(int64(g.data+8*uint64(r.Intn(8)))-int64(pc)-int64(len(out)+4))))
	case 7: // inc %r64
		d := reg()
		return append(rexb(true, 0, d), 0xFF, 0xC0|byte(d&7))
	default:
		return []byte{0x90}
	}
}

// build makes one case placed at pc.
func (g *fdGen) build(pc uint64) *fdCase {
	r := g.r
	for {
		m := g.mem()
		// the operand lands at a 16-byte aligned offset from the %fs base
		t := int64(r.Intn(0x700)-0x380) &^ 15
		bv, iv := g.place(&m, t)
		insn, used := g.insn(m)
		c := &fdCase{insn: insn}
		for i := range c.regs {
			c.regs[i] = r.Uint64()
		}
		if m.base >= 0 {
			c.regs[m.base] = bv
		}
		if m.index >= 0 {
			c.regs[m.index] = iv
		}
		c.regs[4] = fdStack + 0x3000
		c.flags = uint64(r.Uint32()) & 0xCD5
		r.Read(c.xmm[:])
		keep := map[int]bool{4: true}
		for _, u := range used {
			keep[u] = true
		}
		if m.base >= 0 {
			keep[m.base] = true
		}
		if m.index >= 0 {
			keep[m.index] = true
		}
		after := map[int]bool{4: true}
		ctr := -1
		if g.loop {
			for _, x := range r.Perm(4) {
				if !keep[12+x] {
					ctr = 12 + x
					break
				}
			}
			if ctr < 0 {
				continue
			}
			keep[ctr] = true
			after[ctr] = true
			if m.base >= 0 {
				after[m.base] = true
			}
			if m.index >= 0 {
				after[m.index] = true
			}
			c.regs[ctr] = uint64(150000 + r.Intn(150000))
		}
		var code []byte
		for n := r.Intn(4); n > 0; n-- {
			code = append(code, g.neighbour(pc+uint64(len(code)), keep)...)
		}
		code = append(code, insn...)
		for n := r.Intn(4); n > 0; n-- {
			code = append(code, g.neighbour(pc+uint64(len(code)), after)...)
		}
		if ctr >= 0 { // sub $1,%ctr; jnz start
			code = append(code, 0x48|byte(ctr>>3), 0x83, 0xE8|byte(ctr&7), 1, 0x0F, 0x85)
			code = binary.LittleEndian.AppendUint32(code, uint32(int32(-(len(code) + 4))))
		}
		// pushfq; mov %rax,-8(%rsp); mov %rcx,-0x10(%rsp); mov %r11,-0x18(%rsp); mov $500,%eax; syscall
		code = append(code, 0x9C, 0x48, 0x89, 0x44, 0x24, 0xF8, 0x48, 0x89, 0x4C, 0x24, 0xF0, 0x4C, 0x89, 0x5C, 0x24, 0xE8,
			0xB8, 0xF4, 1, 0, 0, 0x0F, 0x05)
		if len(code) > fdCaseMax {
			continue
		}
		c.code = code
		c.desc = fmt.Sprintf("insn % x mem %+v code % x", insn, m, code)
		return c
	}
}

// fdResult is the state after a case.
type fdResult struct {
	regs  [16]uint64
	rax   uint64
	rcx   uint64
	r11   uint64
	flags uint64
	xmm   [256]byte
	mxcsr uint32
	mem   []byte
	err   string
	frip  uint64 // the rip reported with a signal
}

func fdWrite(t *testing.T, h *harness, fr memmap.FileRange, data []byte) {
	t.Helper()
	bs, err := h.mf.MapInternal(fr, hostarch.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := safemem.CopySeq(bs, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(data))); err != nil {
		t.Fatal(err)
	}
}

func fdRead(t *testing.T, h *harness, fr memmap.FileRange, n int) []byte {
	t.Helper()
	bs, err := h.mf.MapInternal(fr, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, n)
	if _, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(out)), bs); err != nil {
		t.Fatal(err)
	}
	return out
}

func hwSet(r *linux.PtraceRegs, v [16]uint64) {
	r.Rax, r.Rcx, r.Rdx, r.Rbx, r.Rsp, r.Rbp, r.Rsi, r.Rdi = v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7]
	r.R8, r.R9, r.R10, r.R11, r.R12, r.R13, r.R14, r.R15 = v[8], v[9], v[10], v[11], v[12], v[13], v[14], v[15]
}

func hwGet(r *linux.PtraceRegs) [16]uint64 { return hwRegs(r) }

// runCases runs the cases on a platform made with the given mode and returns
// their results.
func runCases(t *testing.T, mode string, interrupt bool, textBase uint64, cases []*fdCase, offs []uint64, textCode []byte, areaInit []byte, dataInit []byte) []fdResult {
	t.Helper()
	t.Setenv("WINSTUB_FSMODE", mode)
	h := newHarness(t)
	if h.p.fsRewrite != (mode == "rewrite") {
		t.Fatalf("platform mode: rewrite=%v, want %s", h.p.fsRewrite, mode)
	}
	mm := h.newMM()
	h.mapAt(mm, textBase, h.pages(textCode, (len(textCode)+pageSz-1)/pageSz), rx)
	h.mapAt(mm, textBase-0x100000, h.pages(dataInit, 1), hostarch.Read)
	area := h.pages(areaInit, fdAreaLen/pageSz)
	h.mapAt(mm, fdArea, area, rw)
	stack := h.pages(nil, 4)
	h.mapAt(mm, fdStack, stack, rw)
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	res := make([]fdResult, len(cases))
	nIntr := 0
	defer func() {
		if interrupt {
			t.Logf("FSDIFF %s: %d interrupts", mode, nIntr)
		}
	}()
	for i, cs := range cases {
		// fresh memory for every case
		fdWrite(t, h, area, areaInit)
		fdWrite(t, h, stack, make([]byte, 4*pageSz))
		ac := arch.New(arch.AMD64)
		r := &ac.StateData().Regs
		hwSet(&r.PtraceRegs, cs.regs)
		r.Rip = textBase + offs[i]
		r.Eflags = cs.flags
		r.Fs_base = fdFS
		fp := ac.FloatingPointData().Slice()
		copy(fp[160:416], cs.xmm[:])
		if len(fp) >= 520 {
			binary.LittleEndian.PutUint64(fp[512:], binary.LittleEndian.Uint64(fp[512:])|3) // (x87 and SSE state are in use: xrstor must load them)
		}
		c.FullStateChanged()
		var rr result
		if !interrupt {
			rr = h.sw(c, mm, ac)
		} else {
			// Interrupts land all over the loop, in veneers too: the guest is resumed from
			// the registers it is given back, as the Sentry does after a signal.
			stop := make(chan struct{})
			done := make(chan struct{})
			go func(c platform.Context) {
				defer close(done)
				rnd := rand.New(rand.NewSource(int64(i)))
				for {
					// (a spin: a timer is too coarse to land inside a few instructions)
					for end := time.Now().Add(time.Duration(5+rnd.Intn(120)) * time.Microsecond); time.Now().Before(end); {
						select {
						case <-stop:
							return
						default:
						}
					}
					c.Interrupt()
				}
			}(c)
			for {
				rr = h.sw(c, mm, ac)
				if rr.err != platform.ErrContextInterrupt {
					break
				}
				nIntr++
			}
			close(stop)
			<-done
		}
		if rr.err != nil || ac.StateData().Regs.Orig_rax != 500 {
			why := fmt.Sprintf("%v", rr.err)
			if rr.si != nil {
				why = fmt.Sprintf("signal %d at %#x", rr.si.Signo, rr.si.Addr())
				res[i].frip = ac.StateData().Regs.Rip
			}
			res[i].err = why
			continue // (the thread waits in the fault; the next case answers it with new registers)
		}
		if err := c.PullFullState(mm.as, ac); err != nil {
			t.Fatal(err)
		}
		r = &ac.StateData().Regs
		res[i].regs = hwGet(&r.PtraceRegs)
		sb := fdRead(t, h, stack, 4*pageSz)
		// the stack top: rsp0 = fdStack+0x3000; flags at -8, rax -16, rcx -24, r11 -32
		le := func(off int) uint64 { return binary.LittleEndian.Uint64(sb[0x3000+off:]) }
		res[i].flags = le(-8) & 0x8D5
		res[i].rax, res[i].rcx, res[i].r11 = le(-16), le(-24), le(-32)
		fp = ac.FloatingPointData().Slice()
		copy(res[i].xmm[:], fp[160:416])
		res[i].mxcsr = binary.LittleEndian.Uint32(fp[24:])
		res[i].mem = fdRead(t, h, area, fdAreaLen)
		res[i].regs[0], res[i].regs[1], res[i].regs[11] = 0, 0, 0 // (rax, rcx, r11 come from the saved words)
	}
	return res
}

func TestFSDifferential(t *testing.T) { fsDifferential(t, false, 3000) }

// TestFSDifferentialInterrupt runs each case as a loop of tens of thousands of
// iterations, and interrupts the rewritten one over and over: the thread is stopped
// in all places of the veneers and put back as the Sentry does after a signal; the
// final state must be that of the uninterrupted native run.
func TestFSDifferentialInterrupt(t *testing.T) { fsDifferential(t, true, 60) }

func fsDifferential(t *testing.T, loop bool, n int) {
	if !stubasm.HasFSGSBASE() {
		t.Skip("native mode needs FSGSBASE")
	}
	seed := int64(20261127)
	if s := os.Getenv("FSDIFF_SEED"); s != "" {
		fmt.Sscan(s, &seed)
	}
	if s := os.Getenv("FSDIFF_N"); s != "" {
		fmt.Sscan(s, &n)
	}
	bmi := cpuid.HostFeatureSet().HasFeature(cpuid.X86FeatureBMI2) && cpuid.HostFeatureSet().HasFeature(cpuid.X86FeatureBMI1)
	for _, textBase := range []uint64{fdText, fdFar} {
		t.Run(fmt.Sprintf("text %#x", textBase), func(t *testing.T) {
			g := &fdGen{r: rand.New(rand.NewSource(seed)), ops: fdOps(), bmi: bmi, data: textBase - 0x100000, loop: loop}
			cases := make([]*fdCase, n)
			offs := make([]uint64, n)
			text := bytes.Repeat([]byte{0x90}, 64)
			for i := range cases {
				pc := textBase + uint64(len(text))
				cases[i] = g.build(pc)
				offs[i] = uint64(len(text))
				text = append(text, cases[i].code...)
				for len(text)%16 != 0 {
					text = append(text, 0xCC)
				}
			}
			dataInit := make([]byte, pageSz)
			g.r.Read(dataInit)
			areaInit := make([]byte, fdAreaLen)
			g.r.Read(areaInit)
			nat := runCases(t, "native", false, textBase, cases, offs, text, areaInit, dataInit)
			rew := runCases(t, "rewrite", loop, textBase, cases, offs, text, areaInit, dataInit)
			bad := fsCompare(t, cases, nat, rew, nil)
			if bad > 0 {
				t.Errorf("%d of %d cases differ", bad, n)
			}
			nerr := 0
			for i := range nat {
				if nat[i].err != "" {
					nerr++
				}
			}
			t.Logf("FSDIFF %d cases, %d differ, %d faulted in both (seed %d)", n, bad, nerr, seed)
		})
	}
}

// fsCompare reports the cases whose native and rewritten results differ. roll
// maps the address of an instruction under the jump of a window to the window
// start, where the rewrite may report a fault of it (rollsBack).
func fsCompare(t *testing.T, cases []*fdCase, nat, rew []fdResult, roll map[uint64]uint64) int {
	t.Helper()
	bad := 0
	for i := range cases {
		a, b := nat[i], rew[i]
		var diffs []string
		if a.err != b.err || a.frip != b.frip && roll[a.frip] != b.frip {
			diffs = append(diffs, fmt.Sprintf("native %q (rip %#x), rewrite %q (rip %#x)", a.err, a.frip, b.err, b.frip))
		} else if a.err == "" {
			for k := 0; k < 16; k++ {
				if a.regs[k] != b.regs[k] {
					diffs = append(diffs, fmt.Sprintf("reg %d: native %#x, rewrite %#x", k, a.regs[k], b.regs[k]))
				}
			}
			if a.rax != b.rax || a.rcx != b.rcx { // (r11 is not compared: a context's first run starts with r11 = rip)
				diffs = append(diffs, fmt.Sprintf("rax/rcx native %#x/%#x rewrite %#x/%#x", a.rax, a.rcx, b.rax, b.rcx))
			}
			if a.flags != b.flags {
				diffs = append(diffs, fmt.Sprintf("flags native %#x rewrite %#x", a.flags, b.flags))
			}
			if a.xmm != b.xmm || a.mxcsr != b.mxcsr {
				for k := 0; k < 16; k++ {
					if !bytes.Equal(a.xmm[16*k:16*k+16], b.xmm[16*k:16*k+16]) {
						diffs = append(diffs, fmt.Sprintf("xmm%d native %x rewrite %x (initial %x)", k, a.xmm[16*k:16*k+16], b.xmm[16*k:16*k+16], cases[i].xmm[16*k:16*k+16]))
					}
				}
				if a.mxcsr != b.mxcsr {
					diffs = append(diffs, fmt.Sprintf("mxcsr native %#x rewrite %#x", a.mxcsr, b.mxcsr))
				}
			}
			if !bytes.Equal(a.mem, b.mem) {
				diffs = append(diffs, "memory differs")
			}
		}
		if len(diffs) > 0 {
			bad++
			if bad <= 25 {
				t.Errorf("case %d: %s\n  %s", i, cases[i].desc, strings.Join(diffs, "\n  "))
			}
		}
	}
	return bad
}

// TestFSRealWindows replays the %fs windows of real programs (glibc, busybox, a static
// guest; testdata/fswindows.txt, made by `fsscan -dumpwin`): the instructions around an
// %fs instruction as the compiler left them, the rip-relative loads pointing at a page of
// small TLS offsets, registers holding such offsets or pointers into a data area. Native
// and rewritten runs must end the same way (the same state, or the same fault).
func TestFSRealWindows(t *testing.T) {
	if !stubasm.HasFSGSBASE() {
		t.Skip("native mode needs FSGSBASE")
	}
	raw, err := os.ReadFile("testdata/fswindows.txt")
	if err != nil {
		t.Skipf("no windows: %v", err)
	}
	type win struct {
		code []byte
		rip  []int
	}
	var wins []win
	for _, l := range strings.Split(string(raw), "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		var w win
		if b, err := hexDecode(f[0]); err == nil {
			w.code = b
		} else {
			continue
		}
		if len(f) > 1 {
			for _, o := range strings.Split(f[1], ",") {
				var n int
				fmt.Sscan(o, &n)
				w.rip = append(w.rip, n)
			}
		}
		wins = append(wins, w)
	}
	const rounds = 1
	for _, textBase := range []uint64{fdText, fdFar} {
		t.Run(fmt.Sprintf("text %#x", textBase), func(t *testing.T) {
			rnd := rand.New(rand.NewSource(7))
			dataBase := textBase - 0x100000
			dataInit := make([]byte, pageSz)
			for q := 0; q < pageSz; q += 8 {
				v := uint64(int64(rnd.Intn(0x60)-0x30) * 8)
				if rnd.Intn(4) == 0 {
					v = fdArea + 0x8000 + uint64(rnd.Intn(0x400))*8
				}
				binary.LittleEndian.PutUint64(dataInit[q:], v)
			}
			areaInit := make([]byte, fdAreaLen)
			rnd.Read(areaInit)
			text := bytes.Repeat([]byte{0x90}, 64)
			var cases []*fdCase
			var offs []uint64
			for round := 0; round < rounds; round++ {
				for wi, w := range wins {
					if textBase == fdFar && wi%3 != 0 {
						continue
					}
					code := append([]byte(nil), w.code...)
					pc := textBase + uint64(len(text))
					for _, o := range w.rip {
						target := dataBase + uint64(rnd.Intn(pageSz/8))*8
						binary.LittleEndian.PutUint32(code[o:], uint32(int32(int64(target)-int64(pc+uint64(o)+4))))
					}
					code = append(code, 0x9C, 0x48, 0x89, 0x44, 0x24, 0xF8, 0x48, 0x89, 0x4C, 0x24, 0xF0, 0x4C, 0x89, 0x5C, 0x24, 0xE8, 0xB8, 0xF4, 1, 0, 0, 0x0F, 0x05)
					c := &fdCase{code: code, desc: fmt.Sprintf("window % x", w.code)}
					for i := range c.regs {
						if rnd.Intn(2) == 0 {
							c.regs[i] = uint64(int64(rnd.Intn(0x60)-0x30) * 8)
						} else {
							c.regs[i] = fdArea + 0x8000 + uint64(rnd.Intn(0x400))*8
						}
					}
					c.regs[4] = fdStack + 0x3000
					c.flags = uint64(rnd.Uint32()) & 0xCD5
					rnd.Read(c.xmm[:])
					cases = append(cases, c)
					offs = append(offs, uint64(len(text)))
					text = append(text, code...)
					for len(text)%16 != 0 {
						text = append(text, 0xCC)
					}
				}
			}
			nat := runCases(t, "native", false, textBase, cases, offs, text, areaInit, dataInit)
			rew := runCases(t, "rewrite", false, textBase, cases, offs, text, areaInit, dataInit)
			// A fault under the jump of a window is reported at the window start
			// (TestFSFaultUnderJump resumes there).
			minLen := 5
			if textBase == fdFar {
				minLen = 7
			}
			roll := map[uint64]uint64{}
			wins, _ := FSWindows(text, minLen)
			for _, w := range wins {
				for _, o := range w.Insn {
					if o-w.Lo > 0 && o-w.Lo < minLen {
						roll[textBase+uint64(o)] = textBase + uint64(w.Lo)
					}
				}
			}
			bad := fsCompare(t, cases, nat, rew, roll)
			ok := 0
			for _, r := range nat {
				if r.err == "" {
					ok++
				}
			}
			t.Logf("FSREAL %d windows x %d, %d cases, %d ran to the end natively, %d differ", len(wins), rounds, len(cases), ok, bad)
			if bad > 0 {
				t.Errorf("%d of %d cases differ", bad, len(cases))
			}
		})
	}
}

func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd hex")
	}
	out := make([]byte, len(s)/2)
	for i := range out {
		var b byte
		if _, err := fmt.Sscanf(s[2*i:2*i+2], "%02x", &b); err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}
