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
	"bytes"
	"testing"

	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/x86asm"
)

func decodeAt(t *testing.T, b []byte) (x86asm.Inst, []byte) {
	t.Helper()
	in, err := x86asm.Decode(b, 64)
	if err != nil {
		t.Fatalf("decode % x: %v", b, err)
	}
	return in, b[in.Len:]
}

// TestFSVeneerEncoding decodes the veneers of a set of %fs instructions and
// checks that they are the same instructions on [scratch + ...], wrapped in the
// save, load and restore of the scratch register.
func TestFSVeneerEncoding(t *testing.T) {
	cases := [][]byte{
		{0x64, 0x48, 0x8b, 0x04, 0x25, 0x28, 0, 0, 0},                // mov %fs:0x28,%rax
		{0x64, 0x48, 0x8b, 0x04, 0x25, 0xf8, 0xff, 0xff, 0xff},       // mov %fs:-8,%rax
		{0x64, 0x48, 0x8b, 0x01},                                     // mov %fs:(%rcx),%rax
		{0x64, 0x48, 0x8b, 0x41, 0xf8},                               // mov %fs:-8(%rcx),%rax
		{0x64, 0x4c, 0x8b, 0x34, 0x25, 0xf8, 0xff, 0xff, 0xff},       // mov %fs:-8,%r14 (Go)
		{0x64, 0x8b, 0x07},                                           // mov %fs:(%rdi),%eax
		{0x64, 0x48, 0x8b, 0x44, 0x7e, 0x10},                         // mov %fs:0x10(%rsi,%rdi,2),%rax
		{0x64, 0x4a, 0x8b, 0x44, 0xe3, 0x08},                         // mov %fs:8(%rbx,%r12,8),%rax
		{0x64, 0x48, 0x83, 0x04, 0x25, 0x40, 0, 0, 0, 5},             // addq $5,%fs:0x40
		{0x64, 0xc7, 0x04, 0x25, 0x48, 0, 0, 0, 0x34, 0x12, 0, 0},    // movl $0x1234,%fs:0x48
		{0x64, 0x8a, 0x3c, 0x25, 0x28, 0, 0, 0},                      // mov %fs:0x28,%bh
		{0x64, 0x40, 0x8a, 0x3c, 0x25, 0x28, 0, 0, 0},                // mov %fs:0x28,%dil
		{0x64, 0x4c, 0x8b, 0x04, 0x25, 0x10, 0, 0, 0},                // mov %fs:0x10,%r8
		{0x64, 0x4d, 0x8b, 0x4c, 0x24, 0x08},                         // mov %fs:8(%r12),%r9
		{0x64, 0x48, 0x8b, 0x04, 0x24},                               // mov %fs:(%rsp),%rax
		{0x64, 0x48, 0x8b, 0x45, 0x00},                               // mov %fs:0(%rbp),%rax
		{0x66, 0x64, 0x8b, 0x07},                                     // mov %fs:(%rdi),%ax
		{0xf0, 0x64, 0x48, 0x0f, 0xb1, 0x0c, 0x25, 0x40, 0, 0, 0},    // lock cmpxchg %rcx,%fs:0x40
		{0x64, 0x0f, 0x10, 0x04, 0x25, 0x40, 0, 0, 0},                // movups %fs:0x40,%xmm0
		{0x64, 0x0f, 0xb6, 0x04, 0x25, 0x28, 0, 0, 0},                // movzbl %fs:0x28,%eax
	}
	for _, orig := range cases {
		in, _ := decodeAt(t, orig)
		l := decodeRange(orig, 0, len(orig), nil)
		if len(l) != 1 {
			t.Errorf("% x: %d instructions", orig, len(l))
			continue
		}
		x := l[0]
		if x.cls != clsFS {
			t.Errorf("% x (%v): class %d, want clsFS", orig, in.Op, x.cls)
			continue
		}
		list := []ins{x}
		list[0].off, list[0].n = 0, len(orig)
		v, ok := buildFSVeneer(orig, 0x400000, list, 0, 0, 0)
		if !ok {
			t.Errorf("% x: no veneer", orig)
			continue
		}
		rest := v.code
		var insts []x86asm.Inst
		for len(rest) > 0 {
			if rest[0] == 0xFF && rest[1] == 0x25 { // jmp *0(%rip) and its literal
				break
			}
			var i x86asm.Inst
			i, rest = decodeAt(t, rest)
			insts = append(insts, i)
		}
		if len(insts) != 4 && len(insts) != 5 {
			t.Errorf("% x: %d instructions in the veneer: % x", orig, len(insts), v.code)
			continue
		}
		save, load, restore := insts[0], insts[1], insts[len(insts)-1]
		op := insts[len(insts)-2]
		r1, _ := save.Args[1].(x86asm.Reg)
		if m, ok := save.Args[0].(x86asm.Mem); save.Op != x86asm.MOV || !ok || m.Segment != x86asm.GS || m.Base != 0 || m.Index != 0 || m.Disp != 0x1488 {
			t.Errorf("% x: bad save %v", orig, save)
		}
		if m, ok := load.Args[1].(x86asm.Mem); load.Op != x86asm.MOV || load.Args[0] != save.Args[1] || !ok || m.Segment != x86asm.GS || m.Disp != 0x1490 {
			t.Errorf("% x: bad load %v", orig, load)
		}
		if m, ok := restore.Args[1].(x86asm.Mem); restore.Op != x86asm.MOV || restore.Args[0] != save.Args[1] || !ok || m.Segment != x86asm.GS || m.Disp != 0x1488 {
			t.Errorf("% x: bad restore %v", orig, restore)
		}
		if r1 < x86asm.RAX || r1 > x86asm.R15 {
			t.Errorf("% x: scratch %v is not a 64-bit register", orig, r1)
		}
		// The scratch register is not one the instruction uses.
		for _, a := range in.Args {
			switch a := a.(type) {
			case x86asm.Reg:
				if a == r1 || a+(x86asm.RAX-x86asm.EAX) == r1 {
					t.Errorf("% x: scratch %v is an operand", orig, r1)
				}
			case x86asm.Mem:
				if a.Base == r1 || a.Index == r1 {
					t.Errorf("% x: scratch %v is in the address", orig, r1)
				}
			}
		}
		// The operation: same mnemonic and operands, the memory operand relative
		// to the scratch register.
		if op.Op != in.Op {
			t.Errorf("% x: op %v, want %v", orig, op.Op, in.Op)
		}
		for i, a := range in.Args {
			m, isMem := a.(x86asm.Mem)
			if !isMem {
				if op.Args[i] != a {
					t.Errorf("% x: operand %d is %v, want %v", orig, i, op.Args[i], a)
				}
				continue
			}
			nm, ok := op.Args[i].(x86asm.Mem)
			if !ok || nm.Segment != 0 || int32(nm.Disp) != int32(m.Disp) || nm.Index != m.Index || (m.Index != 0 && nm.Scale != m.Scale) {
				t.Errorf("% x: operand %d is %v, want %v on %v", orig, i, op.Args[i], m, r1)
				continue
			}
			if m.Base == 0 {
				if nm.Base != r1 || len(insts) != 4 {
					t.Errorf("% x: base %v, want %v and no lea", orig, nm.Base, r1)
				}
			} else {
				lea := insts[2]
				lm, ok := lea.Args[1].(x86asm.Mem)
				if len(insts) != 5 || lea.Op != x86asm.LEA || lea.Args[0] != save.Args[1] || !ok || lm.Disp != 0 || nm.Base != r1 ||
					!(lm.Base == r1 && lm.Index == m.Base || lm.Base == m.Base && lm.Index == r1) {
					t.Errorf("% x: bad address computation %v / %v", orig, lea, op)
				}
			}
		}
	}
}

// TestScanFS checks the classification of %fs instructions by window.
func TestScanFS(t *testing.T) {
	code := []byte{
		0x64, 0x48, 0x8b, 0x04, 0x25, 0x28, 0, 0, 0, // 9 bytes: direct
		0xb9, 8, 0, 0, 0, 0x64, 0x48, 0x8b, 0x01, // mov $8,%ecx; 4 bytes with a neighbour: window
		0x90, 0xeb, 0x00, 0x64, 0x8b, 0x01, 0xeb, 0x00, // 3 bytes between jumps: none
		0x48, 0x8b, 0x0d, 0x10, 0, 0, 0, 0x64, 0x8b, 0x01, // mov x(%rip),%rcx; mov %fs:(%rcx),%eax: window with a rip load
		0x64, 0x48, 0xa5, // movs: unsupported
		0x64, 0xc4, 0xe2, 0xfb, 0xf7, 0x04, 0x25, 0x98, 0x06, 0, 0, // a BMI2 op the decoder does not know
	}
	rep := ScanFS(code)
	if rep.Total != 6 || rep.Unsupp != 1 {
		t.Fatalf("report %+v: want 6 instructions, 1 unsupported", rep)
	}
	// the lone 3-byte one has neighbours that are not copyable: none. The 4-byte
	// one after mov $8,%ecx has a window only with the near jump: under the far
	// jump's 7 bytes a fault of it would be reported at the window start, and the
	// mov writes its base (rollsBack).
	wantNear := FSClassCount{Direct: 2, Window: 2, None: 1}
	wantFar := FSClassCount{Direct: 2, Window: 1, None: 2}
	if rep.Near != wantNear || rep.Far != wantFar {
		t.Errorf("near %+v, far %+v, want %+v, %+v", rep.Near, rep.Far, wantNear, wantFar)
	}
	if rep.Rip != 1 {
		t.Errorf("windows with a rip-relative load: %d, want 1", rep.Rip)
	}
}

// TestFSVeneerVEX checks the veneers of VEX-encoded %fs instructions byte for
// byte (the decoder does not know them all).
func TestFSVeneerVEX(t *testing.T) {
	cases := []struct{ orig, want []byte }{
		{ // BMI2 op on %fs:0x698 (glibc)
			[]byte{0x64, 0xc4, 0xe2, 0xfb, 0xf7, 0x04, 0x25, 0x98, 0x06, 0, 0},
			[]byte{0xc4, 0xe2, 0xfb, 0xf7, 0x84, 0x23, 0x98, 0x06, 0, 0},
		},
		{ // vmovups %fs:0x40,%xmm0
			[]byte{0x64, 0xc5, 0xf8, 0x10, 0x04, 0x25, 0x40, 0, 0, 0},
			[]byte{0xc5, 0xf8, 0x10, 0x84, 0x23, 0x40, 0, 0, 0},
		},
		{ // BMI2 op on %fs:-8(%rcx)
			[]byte{0x64, 0xc4, 0xe2, 0xf9, 0xf7, 0x41, 0xf8},
			[]byte{0x48, 0x8d, 0x5c, 0x0b, 0x00, 0xc4, 0xe2, 0xf9, 0xf7, 0x84, 0x23, 0xf8, 0xff, 0xff, 0xff},
		},
	}
	for _, c := range cases {
		l := decodeRange(c.orig, 0, len(c.orig), nil)
		if len(l) != 1 || l[0].n != len(c.orig) || l[0].cls != clsFS {
			t.Errorf("% x: decoded as %+v", c.orig, l)
			continue
		}
		v, ok := buildFSVeneer(c.orig, 0x400000, l, 0, 0, 0)
		if !ok || !bytes.Contains(v.code, c.want) {
			t.Errorf("% x: veneer % x does not contain % x", c.orig, v.code, c.want)
		}
	}
}

func TestSplitFSAt(t *testing.T) {
	body := []byte{0x48, 0x89, 0xe5, 0x0f, 0xb6, 0x05, 0xbd, 0x40, 0x0c, 0x00} // mov %rsp,%rbp; movzbl rip-relative
	for _, tail := range [][]byte{{0x64}, {0x64, 0x88}, {0x64, 0x88, 0x04}, {0x64, 0x48, 0x8b, 0x04, 0x25, 1}, {0x66, 0x64, 0x89}} {
		code := append(append(make([]byte, 0, 40), body...), tail...)
		if p := splitFSAt(code, decodeAll(code), 0); p != len(body) {
			t.Errorf("tail % x: split at %d, want %d", tail, p, len(body))
		}
	}
	for _, tail := range [][]byte{{0x48}, {0x48, 0x89}, {0x0f}, {0x65, 0x88}, nil} {
		code := append(append(make([]byte, 0, 40), body...), tail...)
		if p := splitFSAt(code, decodeAll(code), 0); p != -1 {
			t.Errorf("tail % x: split at %d, want none", tail, p)
		}
	}
	whole := append(append([]byte(nil), body...), 0x64, 0x88, 0x04, 0x25, 0x11, 0x06, 0, 0)
	if p := splitFSAt(whole, decodeAll(whole), 0); p != -1 {
		t.Errorf("a whole instruction is reported split at %d", p)
	}
}

// TestDecodeBoundaries: instructions that the decoder once got the length of, which
// made the sweep swallow the first bytes of the next instruction (an %fs instruction
// after a vzeroupper was never rewritten).
func TestDecodeBoundaries(t *testing.T) {
	fsLoad := []byte{0x64, 0x48, 0x2b, 0x04, 0x25, 0x28, 0, 0, 0} // sub %fs:0x28,%rax
	for _, c := range []struct {
		name string
		code []byte
		n    int // length of the first instruction
	}{
		{"vzeroupper", []byte{0xc5, 0xf8, 0x77}, 3},
		{"vzeroall", []byte{0xc5, 0xfc, 0x77}, 3},
		{"vzeroupper, 3-byte VEX", []byte{0xc4, 0xe1, 0x78, 0x77}, 4},
		{"endbr64", []byte{0xf3, 0x0f, 0x1e, 0xfa}, 4},
		{"endbr32", []byte{0xf3, 0x0f, 0x1e, 0xfb}, 4},
		{"vmovdqu", []byte{0xc5, 0xfe, 0x6f, 0x07}, 4},
		{"andn", []byte{0xc4, 0xe2, 0x78, 0xf2, 0xc1}, 5},
		{"shrx with a memory operand", []byte{0xc4, 0xe2, 0x7b, 0xf7, 0x44, 0x24, 0x08}, 7},
		{"rorx", []byte{0xc4, 0xe3, 0x7b, 0xf0, 0xc1, 0x05}, 6},
		{"kmovd", []byte{0xc5, 0xfb, 0x93, 0xc0}, 4},
		{"rdsspq", []byte{0xf3, 0x48, 0x0f, 0x1e, 0xc8}, 5},
		{"saveprevssp", []byte{0xf3, 0x0f, 0x01, 0xea}, 4},
		{"incsspq", []byte{0xf3, 0x48, 0x0f, 0xae, 0xeb}, 5},
		{"rstorssp", []byte{0xf3, 0x0f, 0x01, 0x69, 0xf8}, 5},
		{"EVEX vmovups", []byte{0x62, 0xf1, 0x7c, 0x48, 0x10, 0x00}, 6},
	} {
		for _, next := range [][]byte{{0x48, 0x8b, 0x45, 0xc8}, {0x48, 0x8b}, {0x45, 0xc8}} {
			code := append(append(append([]byte(nil), c.code...), next...), fsLoad...)
			// next is not always whole instructions: only the first instruction's length is checked
			list := decodeRange(code, 0, len(code), nil)
			if len(list) == 0 || list[0].n != c.n {
				t.Errorf("%s: length of the first instruction in % x is %v, want %d", c.name, code, list, c.n)
			}
		}
		// followed by whole instructions, the %fs one is found
		code := append(append(append([]byte(nil), c.code...), 0x48, 0x8b, 0x45, 0xc8), fsLoad...)
		list := decodeRange(code, 0, len(code), nil)
		found := false
		for _, x := range list {
			if x.off == len(code)-len(fsLoad) && (x.cls == clsFS) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: the %%fs instruction after it is not found in % x: %v", c.name, code, list)
		}
	}
}
