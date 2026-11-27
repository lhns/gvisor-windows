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
	"debug/elf"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
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

// TestFSSeam: the %fs counterpart of TestSeamRedecode, in both mapping orders and
// near and far from the veneers. Text mapped in two pieces whose boundary is inside
// an instruction; when the upper piece comes first its first bytes are decoded from
// a guess, and the guess must be put right once the lower piece arrives:
//   - a guessed instruction swallows the start of a real %fs instruction (the site
//     would run unrewritten, with a zero base);
//   - the bytes of an immediate look like an %fs instruction (the immediate must not
//     be changed);
//   - an %fs instruction itself is cut by the boundary.
//
// In native mode the same code must give the same results (no rewriting at all).
func TestFSSeam(t *testing.T) {
	pad := func(head, tail []byte) []byte {
		b := make([]byte, pageSz)
		for i := range b {
			b[i] = 0x90
		}
		copy(b, head)
		copy(b[pageSz-len(tail):], tail)
		return b
	}
	report := []byte{0x48, 0x89, 0xc7, 0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05} // mov %rax,%rdi; mov $500,%eax; syscall
	cat := func(parts ...[]byte) []byte {
		var out []byte
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	cases := []struct {
		name   string
		lo, hi []byte
		cut    uint64 // where execution starts, counted back from the end of the low page
		want   uint64 // rdi at the syscall
	}{
		// lea 0x????c081(%rip),%rbx is cut after its opcode; its displacement bytes (81 c0 12 34)
		// read as `add $imm32,%eax` from the middle, which swallows the first bytes of the
		// real `mov %fs:0x28,%rax` behind it.
		{"guessed instruction swallows an %fs instruction",
			pad(nil, []byte{0x48, 0x8d, 0x1d}),
			pad(cat([]byte{0x81, 0xc0, 0x12, 0x34, 0x64, 0x48, 0x8b, 0x04, 0x25, 0x28, 0, 0, 0}, report), nil), 3, 0x1111},
		// movabs $imm64,%rax is cut after its opcode; the immediate 64 8b 04 25 28 00 00 00
		// is `mov %fs:0x28,%eax` from the middle.
		{"guessed %fs instruction inside an immediate",
			pad(nil, []byte{0x48, 0xb8}),
			pad(cat([]byte{0x64, 0x8b, 0x04, 0x25, 0x28, 0, 0, 0}, report), nil), 2, 0x0000002825048b64},
		// mov %fs:0x28,%rax cut after its first three bytes
		{"%fs instruction cut by the boundary",
			pad(nil, []byte{0x64, 0x48, 0x8b}),
			pad(cat([]byte{0x04, 0x25, 0x28, 0, 0, 0}, report), nil), 3, 0x1111},
	}
	for _, base := range []uint64{textBase, 0x1_0000_0000} {
		for _, c := range cases {
			for _, lowFirst := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s, base %#x, low first %v", c.name, base, lowFirst), func(t *testing.T) {
					h := newHarness(t)
					mm := h.newMM()
					lo, hi := h.pages(c.lo, 1), h.pages(c.hi, 1)
					if lowFirst {
						h.mapAt(mm, base, lo, rx)
						h.mapAt(mm, base+pageSz, hi, rx)
					} else {
						h.mapAt(mm, base+pageSz, hi, rx)
						h.mapAt(mm, base, lo, rx)
					}
					h.mapAt(mm, tlsBase, h.pages(tlsData(), 1), rw)
					cx := h.p.NewContext(pkgcontext.Background())
					defer cx.Release()
					ac := newAC(base+pageSz-c.cut, 0)
					ac.StateData().Regs.Fs_base = tlsBase
					if nr := h.syscall(cx, mm, ac); nr != 500 || ac.StateData().Regs.Rdi != c.want {
						t.Fatalf("syscall %d, rdi %#x; want 500, %#x", nr, ac.StateData().Regs.Rdi, c.want)
					}
				})
			}
		}
	}
}

// TestFSSeamBinary maps the text of a real Linux program (a Go binary built here) in
// pieces cut at random pages, mapped in random order, and checks that afterwards every
// %fs instruction and every syscall instruction that a decoding of the whole text finds
// has been patched: a site that no piece's decoding saw would run unrewritten (and as
// such crash or corrupt data in rewrite mode).
func TestFSSeamBinary(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "gohello")
	cmd := exec.Command("go", "build", "-o", exe, ".")
	cmd.Dir = "../../../../test/guests/gohello"
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build a Linux guest: %v\n%s", err, out)
	}
	f, err := elf.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var code []byte
	var vaddr uint64
	for _, p := range f.Progs {
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_X != 0 {
			code = make([]byte, p.Filesz)
			if _, err := p.ReadAt(code, 0); err != nil {
				t.Fatal(err)
			}
			vaddr = p.Vaddr
			break
		}
	}
	if len(code) == 0 || vaddr%pageSz != 0 {
		t.Fatalf("no text segment (%d bytes at %#x)", len(code), vaddr)
	}
	truth := scanText(code, 0, true)
	pages := (len(code) + pageSz - 1) / pageSz
	nearSite := make([]bool, len(code))
	for _, st := range truth.sites {
		for k := max(0, st.off-24); k < min(len(code), st.off+40); k++ {
			nearSite[k] = true
		}
	}
	for trial := 0; trial < 12; trial++ {
		rnd := rand.New(rand.NewSource(int64(trial)))
		// cut points: pieces of 1 to 40 pages
		type piece struct{ lo, hi int } // in pages
		var ps []piece
		for p := 0; p < pages; {
			n := 1 + rnd.Intn(40)
			if trial == 0 {
				n = 1 // every page a piece of its own
			}
			ps = append(ps, piece{p, min(p+n, pages)})
			p += n
		}
		rnd.Shuffle(len(ps), func(i, j int) { ps[i], ps[j] = ps[j], ps[i] })
		if trial == 1 { // from the top down
			for i := range ps {
				ps[i] = piece{ps[i].lo, ps[i].hi}
			}
			for i := 0; i < len(ps); i++ {
				for j := i + 1; j < len(ps); j++ {
					if ps[j].lo > ps[i].lo {
						ps[i], ps[j] = ps[j], ps[i]
					}
				}
			}
		}
		h := newHarness(t)
		mm := h.newMM()
		for _, p := range ps {
			chunk := code[p.lo*pageSz : min(p.hi*pageSz, len(code))]
			h.mapAt(mm, vaddr+uint64(p.lo)*pageSz, h.pages(chunk, p.hi-p.lo), rx)
		}
		as := mm.as.(*addressSpace)
		got, ok := as.readStub(vaddr, len(code))
		if !ok {
			t.Fatal("cannot read the patched text")
		}
		near := func(off int) bool {
			for _, p := range ps {
				if o := p.lo * pageSz; off >= o && off < o+96 {
					return true
				}
			}
			return false
		}
		missed := map[string]int{}
		var first []string
		for _, s := range truth.sites {
			x := s.win[s.s]
			// (An %fs instruction the veneers cannot express is left alone by design;
			// the ELF header page of the Go guest, whose build ID changes with every
			// commit, sometimes decodes as one: `64 6f`, outsd.)
			if s.fs && (!h.p.fsRewrite || x.cls == clsFSBad) {
				continue
			}
			if bytes.Equal(got[s.off:s.off+x.n], code[s.off:s.off+x.n]) {
				kind := "syscall"
				if s.fs {
					kind = "%fs"
				}
				missed[kind]++
				if len(first) < 8 {
					first = append(first, fmt.Sprintf("%s site %#x (page offset %#x): % x", kind, vaddr+uint64(s.off), s.off%pageSz, code[s.off:s.off+x.n]))
				}
			}
		}
		// Bytes that changed far from any site, or that the whole text does not change,
		// are code damaged by a patch of something that is no instruction (a bogus site).
		// (A piece begins with a few bytes of guessed decoding: those are left out.)
		var odd []string
		for pos := 0; pos < len(code); pos++ {
			if got[pos] == code[pos] || nearSite[pos] || near(pos) {
				continue
			}
			if len(odd) < 8 {
				odd = append(odd, fmt.Sprintf("byte %#x (page offset %#x) changed from %02x to %02x, no site within 24 bytes", vaddr+uint64(pos), pos%pageSz, code[pos], got[pos]))
			}
			break
		}
		if len(odd) > 0 {
			t.Errorf("trial %d (%d pieces): damaged code%s", trial, len(ps), joinLines(append([]string{""}, odd...)))
		}
		if len(missed) > 0 {
			t.Errorf("trial %d (%d pieces): sites left unpatched %v\n  %s", trial, len(ps), missed, joinLines(first))
		}
	}
}

func joinLines(l []string) string {
	out := ""
	for i, s := range l {
		if i > 0 {
			out += "\n  "
		}
		out += s
	}
	return out
}
