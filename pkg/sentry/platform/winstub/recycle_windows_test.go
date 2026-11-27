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
	"testing"
	"time"

	pkgcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

// TestRecycle: a released address space whose thread waits for a reply is emptied
// and handed out again. The new owner sees nothing of the old one (no mapping, the
// patch state of a new space), runs its own code, and a context that still holds the
// old owner's thread (it was not released before the address space) cannot touch the
// thread the new owner got in the same slot.
func TestRecycle(t *testing.T) {
	if !recycleOn {
		t.Skip("WINSTUB_RECYCLE=0")
	}
	h := newHarness(t)
	code := func(nr byte) []byte { return []byte{0xb8, nr, 0, 0, 0, 0x0f, 0x05, 0xb8, nr + 1, 0, 0, 0, 0x0f, 0x05} }
	newAS := func() *addressSpace {
		as, err := h.p.NewAddressSpace(platform.AddressSpaceOptions{})
		if err != nil {
			t.Fatalf("NewAddressSpace: %v", err)
		}
		return as.(*addressSpace)
	}
	as1 := newAS()
	mm1 := &testMM{as: as1}
	h.mapAt(mm1, textBase, h.pages(code(40), 1), rx)
	old := h.p.NewContext(pkgcontext.Background())
	ac1 := newAC(textBase, 0)
	if nr := h.syscall(old, mm1, ac1); nr != 40 {
		t.Fatalf("syscall %d, want 40", nr)
	}
	gen := as1.gen.Load()
	as1.Release() // the thread waits for its reply: the stub is emptied in the background
	var as2 *addressSpace
	for deadline := time.Now().Add(10 * time.Second); ; {
		h.p.spareMu.Lock()
		n := len(h.p.recycled)
		h.p.spareMu.Unlock()
		if n > 0 {
			as2 = newAS()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the address space was not recycled (%d failures)", h.p.nRecycleFail.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer as2.Release()
	if as2 != as1 || as2.gen.Load() != gen+1 {
		t.Fatalf("got a new stub (same %v, generation %d after %d)", as2 == as1, as2.gen.Load(), gen)
	}
	as2.mu.Lock()
	for _, v := range as2.views {
		if !v.sys {
			t.Errorf("view %#x-%#x left", v.lo, v.hi)
		}
	}
	if as2.nsites != 0 || len(as2.restartAt) != 0 || as2.hist != ([32]byte{}) {
		t.Errorf("patch state left: %d sites, %d restart entries", as2.nsites, len(as2.restartAt))
	}
	as2.mu.Unlock()
	if _, ok := as2.readStub(textBase, 1); ok {
		t.Errorf("the old text is still readable")
	}
	mm2 := &testMM{as: as2}
	h.mapAt(mm2, textBase, h.pages(code(60), 1), rx)
	cx := h.p.NewContext(pkgcontext.Background())
	defer cx.Release()
	ac2 := newAC(textBase, 0)
	if nr := h.syscall(cx, mm2, ac2); nr != 60 {
		t.Fatalf("new owner: syscall %d, want 60", nr)
	}
	old.Release() // must not end the new owner's thread (the same slot)
	if nr := h.syscall(cx, mm2, ac2); nr != 61 {
		t.Fatalf("new owner after the old context's release: syscall %d, want 61", nr)
	}
}
