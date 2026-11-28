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
	"testing"

	pkgcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
)

// TestSharedVeneerReclaim: the space of a shared veneer goes to another site once
// no address space holds it, and never while one does: here an address space with
// a thread stopped in the veneer (a syscall returned into it) whose site was
// unmapped. The area is shrunk so that it fills.
func TestSharedVeneerReclaim(t *testing.T) {
	h := newHarness(t)
	textCacheOn = false // (cached states hold their veneers too)
	defer func() { textCacheOn = true }()
	code, _, withPost, _ := relocPiece(41)
	fr := h.pages(code, relocMinLen/pageSz)
	const x1, x2, x3, x4, x5 = 0x1000_0000, 0x1100_0000, 0x1200_0000, 0x1300_0000, 0x1400_0000
	veneer := func(mm *testMM) (va, vlen uint64) {
		t.Helper()
		as := mm.as.(*addressSpace)
		as.mu.Lock()
		defer as.mu.Unlock()
		if len(as.shared) != 1 {
			return 0, 0
		}
		return as.shared[0].va, as.shared[0].vlen
	}

	a := h.newMM()
	h.mapAt(a, x1, fr, rx)
	va1, vlen := veneer(a)
	if va1 == 0 {
		t.Fatalf("the piece has no shared veneer")
	}
	r := &h.p.veneers
	r.mu.Lock()
	top := veneerTop
	veneerTop = r.next + vlen // room for one more
	r.mu.Unlock()
	defer func() { veneerTop = top }()

	// A thread of a stops in the veneer, and a unmaps the site.
	cx := h.p.NewContext(pkgcontext.Background())
	defer cx.Release()
	ac := newAC(x1+uint64(withPost), 0)
	if nr := h.syscall(cx, a, ac); nr != 0 {
		t.Fatalf("syscall %d, want 0", nr)
	}
	if rip := ac.StateData().Regs.Rip; rip < va1 || rip >= va1+vlen {
		t.Fatalf("the thread stopped at %#x, not in the veneer %#x-%#x", rip, va1, va1+vlen)
	}
	aas := a.as.(*addressSpace)
	saved, ok := aas.readStub(va1, int(vlen))
	if !ok {
		t.Fatalf("cannot read the veneer")
	}
	a.as.Unmap(hostarch.Addr(x1), relocMinLen)

	// Other sites take the rest of the area, not a's veneer; with none left, a site
	// gets none.
	b := h.newMM()
	h.mapAt(b, x2, fr, rx)
	vb, _ := veneer(b)
	if vb == 0 || vb == va1 {
		t.Fatalf("second site: veneer at %#x (the first at %#x)", vb, va1)
	}
	c := h.newMM()
	h.mapAt(c, x3, fr, rx)
	if v, _ := veneer(c); v != 0 {
		t.Fatalf("a site got a veneer at %#x in a full area", v)
	}
	// b's veneer is given back with b, and reused.
	b.as.Release()
	d := h.newMM()
	h.mapAt(d, x4, fr, rx)
	if v, _ := veneer(d); v != vb {
		t.Fatalf("after the release: veneer at %#x, want b's %#x", v, vb)
	}

	// a's veneer is intact; a maps the site again and the thread continues.
	if got, ok := aas.readStub(va1, int(vlen)); !ok || !bytes.Equal(got, saved) {
		t.Fatalf("the veneer of the stopped thread changed")
	}
	h.mapAt(a, x1, fr, rx)
	if v, _ := veneer(a); v != va1 {
		t.Fatalf("the site mapped again: veneer at %#x, want %#x", v, va1)
	}
	ac.StateData().Regs.Rax = 77
	if nr := h.syscall(cx, a, ac); nr != 501 || ac.StateData().Regs.Rdi != 77 {
		t.Fatalf("after the stop: syscall %d rdi %d, want 501 77", nr, ac.StateData().Regs.Rdi)
	}

	// Released, a gives its veneer up.
	a.as.Release()
	e := h.newMM()
	h.mapAt(e, x5, fr, rx)
	if v, _ := veneer(e); v != va1 {
		t.Fatalf("after a's release: veneer at %#x, want a's %#x", v, va1)
	}
	if used, held := r.stats(); held > used || used != veneerTop-stubasm.VeneersShared {
		t.Fatalf("registry: %d bytes used, %d held, area %d", used, held, veneerTop-stubasm.VeneersShared)
	}
}
