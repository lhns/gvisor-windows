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
	"unsafe"

	pkgcontext "gvisor.dev/gvisor/pkg/context"
)

// writeText overwrites bytes of the text mapped at addr behind the patcher's back.
func writeText(t *testing.T, as *addressSpace, addr uint64, b []byte) {
	t.Helper()
	as.mu.Lock()
	defer as.mu.Unlock()
	v := as.findView(addr)
	if v == nil || v.priv == nil {
		t.Fatalf("no private text at %#x", addr)
	}
	off := v.off + (addr - v.lo)
	base := off &^ 0xFFFF
	m, err := call(pMapViewOfFile, v.priv.h, fileMapWrite, uintptr(base>>32), uintptr(base&0xFFFFFFFF), uintptr(off-base+uint64(len(b))))
	if err != nil {
		t.Fatalf("MapViewOfFile: %v", err)
	}
	copy(unsafe.Slice((*byte)(ptr(m+uintptr(off-base))), len(b)), b)
	pUnmapViewOfFile.Call(m)
}

// TestTripwire: with the instrumentation callback on, ordinary syscalls and faults pass
// through it, and a `syscall` instruction that runs on Windows (here: patched text put
// back to its original bytes) ends the address space instead of going unnoticed.
func TestTripwire(t *testing.T) {
	h := newHarness(t)
	if !h.p.tripwire {
		t.Skip("WINSTUB_TRIPWIRE is not set")
	}
	code := []byte{0xb8, 40, 0, 0, 0, 0x0f, 0x05, 0xb8, 0xff, 0x07, 0, 0, 0x0f, 0x05, 0xc3}
	pg := make([]byte, pageSz)
	copy(pg, code)
	mm := h.newMM()
	h.mapAt(mm, textBase, h.pages(pg, 1), rx)
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	ac := newAC(textBase, 0)
	for i := 0; i < 100; i++ { // many kernel returns through the callback, all of them quiet
		ac.StateData().Regs.Rip = textBase
		if nr := h.syscall(c, mm, ac); nr != 40 {
			t.Fatalf("syscall %d, want 40", nr)
		}
	}
	as := mm.as.(*addressSpace)
	// The second site (mov $0x7ff,%eax; syscall: no such Windows service) back to its
	// original bytes: it runs on Windows and returns to guest text.
	writeText(t, as, textBase+7, code[7:14])
	ac.StateData().Regs.Rip = textBase + 7
	r := h.sw(c, mm, ac)
	if r.err == nil || !as.dead.Load() {
		t.Fatalf("an unpatched syscall went unnoticed: Switch returned %v, dead=%v", r.err, as.dead.Load())
	}
}
