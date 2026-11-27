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
)

// TestTextCache: the text of TestSeamRedecode's first case, mapped upper piece first in
// several address spaces of one platform. The first two patch it themselves (the
// second keeps the state), the later ones take the cached patch, including the reseam
// the lower piece makes in the upper one. Every address space must run the syscalls,
// and a cached section patched in place (reseam) must be copied first: a space that
// maps only the upper piece gets it as it was before the reseam.
func TestTextCache(t *testing.T) {
	pad := func(head, tail []byte) []byte {
		b := make([]byte, pageSz)
		for i := range b {
			b[i] = 0x90
		}
		copy(b, head)
		copy(b[pageSz-len(tail):], tail)
		return b
	}
	h := newHarness(t)
	lo := h.pages(pad([]byte{0xb8, 40, 0, 0, 0}, []byte{0x48, 0x8d, 0x1d}), 1)
	hi := h.pages(pad([]byte{0x81, 0xc0, 0x12, 0x34, 0x0f, 0x05, 0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05}, nil), 1)
	run := func(mm *testMM) {
		t.Helper()
		cx := h.p.NewContext(pkgcontext.Background())
		defer cx.Release()
		ac := newAC(textBase, 0)
		for _, want := range []uint64{40, 500} {
			if nr := h.syscall(cx, mm, ac); nr != want {
				t.Fatalf("syscall %d, want %d", nr, want)
			}
		}
	}
	upper := func(mm *testMM) []byte {
		t.Helper()
		b, ok := mm.as.(*addressSpace).readStub(textBase+pageSz, 16)
		if !ok {
			t.Fatalf("cannot read the upper piece")
		}
		return b
	}
	var mms []*testMM
	for i := 0; i < 4; i++ {
		mm := h.newMM()
		h.mapAt(mm, textBase+pageSz, hi, rx)
		h.mapAt(mm, textBase, lo, rx)
		run(mm)
		mms = append(mms, mm)
	}
	if n := h.p.texts.hits.Load(); n < 4 {
		t.Fatalf("%d patches taken from the cache, want at least 4 (2 pieces in each of the last 2 spaces)", n)
	}
	for _, mm := range mms {
		run(mm) // still right after the others
	}
	// The upper piece alone: the bytes before the reseam, not those of mms[3].
	only := h.newMM()
	h.mapAt(only, textBase+pageSz, hi, rx)
	before, after := upper(only), upper(mms[3])
	if bytes.Equal(before, after) {
		t.Fatalf("the upper piece reads the same before and after the reseam (% x): the reseam did not happen or wrote into the shared section", before)
	}
	if b := upper(mms[0]); !bytes.Equal(b, after) {
		t.Fatalf("the space that patched itself has % x, the one with the cached patch % x", b, after)
	}
}
