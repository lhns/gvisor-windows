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
	"fmt"
	"testing"

	pkgcontext "gvisor.dev/gvisor/pkg/context"
)

// TestSeamRedecode: text mapped in two pieces whose boundary is inside an instruction,
// in both mapping orders. When the upper piece comes first its first bytes are decoded
// from a guess; the piece below must put that right (reseam).
func TestSeamRedecode(t *testing.T) {
	pad := func(head, tail []byte) []byte {
		b := make([]byte, pageSz)
		for i := range b {
			b[i] = 0x90
		}
		copy(b, head)
		copy(b[pageSz-len(tail):], tail)
		return b
	}
	cases := []struct {
		name     string
		lo, hi   []byte
		start    uint64 // offset of the first instruction in the low page
		nr       uint64
		rdi      uint64
		syscalls []uint64
	}{
		// lea 0x????c081(%rip),%rbx is cut after its opcode; its displacement bytes read as
		// `add $imm32,%eax` from the middle, which swallows the real syscall behind it.
		{"guessed instruction swallows a syscall",
			pad([]byte{0xb8, 40, 0, 0, 0}, []byte{0x48, 0x8d, 0x1d}),
			pad([]byte{0x81, 0xc0, 0x12, 0x34, 0x0f, 0x05, 0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05}, nil), 0, 0, 0, []uint64{40, 500}},
		// movabs $0x50f,%rax is cut after its opcode; the bytes 0f 05 of the immediate look
		// like a syscall from the middle, and turning them into ud2 would change the value.
		{"guessed syscall inside an immediate",
			pad(nil, []byte{0x48, 0xb8}),
			pad([]byte{0x0f, 0x05, 0, 0, 0, 0, 0, 0, 0x48, 0x89, 0xc7, 0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05}, nil), pageSz - 2, 500, 0x050f, []uint64{500}},
	}
	for _, c := range cases {
		for _, lowFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, low first %v", c.name, lowFirst), func(t *testing.T) {
				h := newHarness(t)
				mm := h.newMM()
				lo, hi := h.pages(c.lo, 1), h.pages(c.hi, 1)
				if lowFirst {
					h.mapAt(mm, textBase, lo, rx)
					h.mapAt(mm, textBase+pageSz, hi, rx)
				} else {
					h.mapAt(mm, textBase+pageSz, hi, rx)
					h.mapAt(mm, textBase, lo, rx)
				}
				cx := h.p.NewContext(pkgcontext.Background())
				defer cx.Release()
				ac := newAC(textBase+c.start, 0)
				for _, want := range c.syscalls {
					if nr := h.syscall(cx, mm, ac); nr != want {
						t.Fatalf("syscall %d, want %d", nr, want)
					}
				}
				if c.rdi != 0 && ac.StateData().Regs.Rdi != c.rdi {
					t.Fatalf("rdi = %#x, want %#x (the immediate of the movabs was changed)", ac.StateData().Regs.Rdi, c.rdi)
				}
			})
		}
	}
}

// TestEntryAfterPadding: a function that begins after nop padding is called from another
// piece of text (a branch the scan of its own piece cannot see). The syscall in it must
// not sit in a patch window that the call lands inside of (Debian's __read_nocancel).
func TestEntryAfterPadding(t *testing.T) {
	for _, lowFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("low first %v", lowFirst), func(t *testing.T) {
			h := newHarness(t)
			mm := h.newMM()
			lo := make([]byte, pageSz)
			hi := make([]byte, pageSz)
			for i := range lo {
				lo[i], hi[i] = 0x90, 0x90
			}
			// low: call entry; mov $500,%eax; syscall
			rel := uint32(pageSz + 11 - 5)
			copy(lo, []byte{0xe8, byte(rel), byte(rel >> 8), byte(rel >> 16), byte(rel >> 24), 0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05})
			// high: ret; 10 bytes of padding; entry: xor %eax,%eax; syscall; cmp $-4096,%rax; ja +1; ret
			copy(hi, []byte{0xc3, 0x66, 0x2e, 0x0f, 0x1f, 0x84, 0, 0, 0, 0, 0, 0x31, 0xc0, 0x0f, 0x05, 0x48, 0x3d, 0, 0xf0, 0xff, 0xff, 0x77, 0x01, 0xc3})
			a, b := h.pages(lo, 1), h.pages(hi, 1)
			if lowFirst {
				h.mapAt(mm, textBase, a, rx)
				h.mapAt(mm, textBase+pageSz, b, rx)
			} else {
				h.mapAt(mm, textBase+pageSz, b, rx)
				h.mapAt(mm, textBase, a, rx)
			}
			h.mapAt(mm, tlsBase, h.pages(nil, 1), rw)
			cx := h.p.NewContext(pkgcontext.Background())
			defer cx.Release()
			ac := newAC(textBase, tlsBase+pageSz-64)
			if nr := h.syscall(cx, mm, ac); nr != 0 {
				t.Fatalf("syscall %d, want 0 (the call must reach the xor and the syscall)", nr)
			}
			if nr := h.syscall(cx, mm, ac); nr != 500 {
				t.Fatalf("syscall %d, want 500", nr)
			}
		})
	}
}
