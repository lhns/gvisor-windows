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
	"fmt"
	"reflect"
	"slices"
	"testing"

	pkgcontext "gvisor.dev/gvisor/pkg/context"
)

// relocPiece is position-independent code of relocMinLen bytes: an imm site, a
// site whose syscall returns into the text, one that returns into a shared veneer,
// and (rewrite mode) an %fs instruction whose window displaces a rip-relative load.
// nr distinguishes the pieces. It returns the code and the offsets of the starts.
func relocPiece(nr byte) (code []byte, noPost, withPost, imm int) {
	a := &asm{}
	for range 64 {
		a.emit(0x90) // (sites in the first bytes of a piece get only the safe patch: zoneNoContext)
	}
	noPost = len(a.b)
	a.emit(0xbf, nr, 0, 0, 0)  // mov edi,nr
	a.emit(0x48, 0x89, 0xf8)   // mov rax,rdi
	a.emit(0x0f, 0x05)         // syscall (veneer)
	a.emit(0x48, 0x89, 0xc7)   // mov rdi,rax
	a.emit(0xb8, 0xf4, 1, 0, 0) // mov eax,500
	a.emit(0x0f, 0x05)         // syscall (imm)
	a.emit(0xeb, 0x00)         // jmp +0: the next instruction is a branch target
	withPost = len(a.b)
	a.emit(0x31, 0xc0)         // xor eax,eax
	a.emit(0x0f, 0x05)         // syscall (shared veneer: returns into it)
	a.emit(0x48, 0x89, 0xc7)   // mov rdi,rax
	a.emit(0xb8, 0xf5, 1, 0, 0, 0x0f, 0x05)
	a.emit(0xeb, 0x0a)                           // jmp L (not taken by the tests)
	a.emit(0x48, 0x8b, 0x05, 0x00, 0x01, 0, 0)   // mov rax,[rip+0x100]
	a.emit(0x64, 0x8b, 0x01)                     // L-3: mov eax,%fs:(%rcx)
	a.emit(0x90)                                 // L
	imm = len(a.b)
	a.emit(0xb8, 39, 0, 0, 0, 0x0f, 0x05) // mov eax,39; syscall
	code = a.b
	for len(code) < relocMinLen {
		code = append(code, 0x90)
	}
	return code, noPost, withPost, imm
}

// patchState is the patch state of as with the parts a comparison can see.
type patchState struct {
	text      [][]byte
	area      []byte
	nsites    int
	vcur      uint64
	fcur      uint64
	ftop      uint64
	gcur      uint64
	siteCache map[uint64]siteInfo
	restartAt map[uint64]uint64
	ud2Sites  map[uint64]bool
	fsCache   map[fsKey]uint64
	fsUD2     map[uint64][]byte
	tails     map[uint64][]byte
	heads     map[uint64][]byte
	edges     map[uint64]edge
	fsVen     []string
	relAt     []relRec
	absAt     []absRec
	shared    []string
}

func stateOf(t *testing.T, mm *testMM, pieces ...uint64) patchState {
	t.Helper()
	as := mm.as.(*addressSpace)
	as.mu.Lock()
	defer as.mu.Unlock()
	s := patchState{nsites: as.nsites, vcur: as.vcur, fcur: as.fcur, ftop: as.ftop, gcur: as.gcur,
		siteCache: as.siteCache, restartAt: as.restartAt, ud2Sites: as.ud2Sites, fsCache: as.fsCache,
		fsUD2: as.fsUD2, tails: as.tails, heads: as.heads, edges: as.edges,
		relAt: slices.Clone(as.relAt), absAt: slices.Clone(as.absAt)}
	for _, lo := range pieces {
		b, ok := as.readStub(lo, relocMinLen)
		if !ok {
			t.Fatalf("cannot read the text at %#x", lo)
		}
		s.text = append(s.text, b)
	}
	for _, r := range as.areaUsed() {
		b, ok := as.readStub(r.lo, int(r.hi-r.lo))
		if !ok {
			t.Fatalf("cannot read the stub area %#x-%#x", r.lo, r.hi)
		}
		s.area = append(s.area, b...)
	}
	for _, v := range as.fsVen {
		s.fsVen = append(s.fsVen, fmt.Sprintf("%#x-%#x w0 %#x jlen %d", v.lo, v.hi, v.w0, v.jlen))
	}
	for _, sh := range as.shared {
		s.shared = append(s.shared, fmt.Sprintf("%+v", sh))
	}
	slices.SortFunc(s.relAt, func(a, b relRec) int { return int(a.at - b.at) })
	slices.SortFunc(s.absAt, func(a, b absRec) int { return int(a.at - b.at) })
	return s
}

// TestTextRelocate: a piece in each region patched in two address spaces (the
// second keeps the state) is mapped elsewhere in a third, each region by its own
// delta: the cached state is relocated, and it must equal what patching at the new
// places gives (a fourth space with the cache off). The relocated space runs the
// syscalls, a fifth one (another relocation) continues a thread stopped in the
// shared veneer of the third (a fork), and a piece placed out of rel32 reach is
// patched rather than relocated.
func TestTextRelocate(t *testing.T) {
	h := newHarness(t)
	codeLo, noPost, withPost, imm := relocPiece(41)
	codeHi, _, _, _ := relocPiece(42)
	frLo, frHi := h.pages(codeLo, relocMinLen/pageSz), h.pages(codeHi, relocMinLen/pageSz)
	const (
		lo0, hi0 = 0x1000_0000, 0x5000_1230_0000
		lo1, hi1 = 0x1234_5000, 0x5000_2b00_0000 // the new places: other deltas per region
	)
	place := func(mm *testMM, lo, hi uint64) {
		t.Helper()
		h.mapAt(mm, lo, frLo, rx)
		h.mapAt(mm, hi, frHi, rx)
	}
	for i := 0; i < 2; i++ {
		place(h.newMM(), lo0, hi0)
	}
	relocs := h.p.texts.relocs.Load()
	moved := h.newMM()
	place(moved, lo1, hi1)
	if n := h.p.texts.relocs.Load() - relocs; n != 2 {
		t.Fatalf("%d pieces relocated, want 2", n)
	}
	textCacheOn = false
	fresh := h.newMM()
	place(fresh, lo1, hi1)
	textCacheOn = true
	got, want := stateOf(t, moved, lo1, hi1), stateOf(t, fresh, lo1, hi1)
	if len(want.relAt) == 0 || len(want.absAt) == 0 || len(want.shared) == 0 {
		t.Fatalf("the pieces were not patched as the test needs: %d rel32 jumps, %d addresses in the stub area, %d shared veneers", len(want.relAt), len(want.absAt), len(want.shared))
	}
	if h.p.fsRewrite && len(want.fsVen) != 2 {
		t.Fatalf("%d %%fs veneers, want one per piece", len(want.fsVen))
	}
	for _, c := range []struct {
		name string
		g, w any
	}{
		{"cursors", []any{got.nsites, got.vcur, got.fcur, got.ftop, got.gcur}, []any{want.nsites, want.vcur, want.fcur, want.ftop, want.gcur}},
		{"siteCache", got.siteCache, want.siteCache}, {"restartAt", got.restartAt, want.restartAt},
		{"ud2Sites", got.ud2Sites, want.ud2Sites}, {"fsCache", got.fsCache, want.fsCache}, {"fsUD2", got.fsUD2, want.fsUD2},
		{"tails", got.tails, want.tails}, {"heads", got.heads, want.heads}, {"edges", got.edges, want.edges},
		{"fsVen", got.fsVen, want.fsVen}, {"relAt", got.relAt, want.relAt}, {"absAt", got.absAt, want.absAt},
		{"shared", got.shared, want.shared},
	} {
		if !reflect.DeepEqual(c.g, c.w) && !(reflect.ValueOf(c.g).Len() == 0 && reflect.ValueOf(c.w).Len() == 0) {
			t.Errorf("%s differs: relocated %v, patched %v", c.name, c.g, c.w)
		}
	}
	for i := range got.text {
		if !bytes.Equal(got.text[i], want.text[i]) {
			t.Errorf("text of piece %d differs", i)
		}
	}
	if a, b := got.area, want.area; !bytes.Equal(a, b) {
		t.Errorf("the used stub area differs (%d and %d bytes)", len(a), len(b))
		for j := range min(len(a), len(b)) {
			if a[j] != b[j] {
				t.Logf("first difference at byte %d: % x vs % x", j, a[j:min(j+16, len(a))], b[j:min(j+16, len(b))])
				break
			}
		}
	}
	if !bytes.Equal(got.text[0], want.text[0]) {
		t.Logf("low piece: % x vs % x", got.text[0][:48], want.text[0][:48])
	}

	// The relocated space runs both pieces.
	cx := h.p.NewContext(pkgcontext.Background())
	defer cx.Release()
	for i, base := range []uint64{lo1, hi1} {
		ac := newAC(base+uint64(noPost), 0)
		for _, w := range []uint64{41 + uint64(i), 500} {
			if nr := h.syscall(cx, moved, ac); nr != w {
				t.Fatalf("piece at %#x: syscall %d, want %d", base, nr, w)
			}
		}
		ac = newAC(base+uint64(imm), 0)
		if nr := h.syscall(cx, moved, ac); nr != 39 {
			t.Fatalf("piece at %#x: imm syscall %d, want 39", base, nr)
		}
	}
	// A thread stopped in the shared veneer continues in another address space
	// that got the same relocation.
	ac := newAC(lo1+uint64(withPost), 0)
	if nr := h.syscall(cx, moved, ac); nr != 0 {
		t.Fatalf("syscall %d, want 0", nr)
	}
	child := h.newMM()
	place(child, lo1, hi1)
	c2 := h.p.NewContext(pkgcontext.Background())
	defer c2.Release()
	ac2 := newAC(0, 0)
	ac2.StateData().Regs = ac.StateData().Regs
	ac2.StateData().Regs.Rax = 77
	if nr := h.syscall(c2, child, ac2); nr != 501 || ac2.StateData().Regs.Rdi != 77 {
		t.Fatalf("child: syscall %d rdi %d, want 501 77", nr, ac2.StateData().Regs.Rdi)
	}

	// Out of rel32 reach of the stub area (still the low region): patched.
	far := h.newMM()
	relocs = h.p.texts.relocs.Load()
	h.mapAt(far, 0xA000_0000, frLo, rx)
	h.mapAt(far, hi1, frHi, rx)
	if n := h.p.texts.relocs.Load() - relocs; n != 0 {
		t.Fatalf("a piece out of reach was relocated")
	}
	ac = newAC(0xA000_0000+uint64(noPost), 0)
	for _, w := range []uint64{41, 500} {
		if nr := h.syscall(cx, far, ac); nr != w {
			t.Fatalf("piece out of reach: syscall %d, want %d", nr, w)
		}
	}
}
