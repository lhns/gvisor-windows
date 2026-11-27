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
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
)

// Rewrite mode (ADR 0020): on CPUs without FSGSBASE every %fs instruction of the
// guest becomes a jump to a veneer (see fsVeneer) that adds the guest's %fs base,
// kept in the thread's TEB, to the address by hand. This file holds the Windows
// side: placing veneers, and moving a stopped thread out of one.

// fsVenInfo describes a veneer in the address space.
type fsVenInfo struct {
	lo, hi uint64 // code range (the entry is lo)
	w0     uint64 // guest address of the window it replaces
	v      *fsVeneer
}

type fsKey struct {
	addr uint64
	h    [32]byte
}

// patchFS rewrites the %fs instruction of site into a jump to a veneer, or, if
// that is impossible, leaves it as ud2 for the debugger to run (fsRunInline).
func (as *addressSpace) patchFS(code []byte, base uint64, site *scanSite, cl *claims, st *siteStats, unsure bool) {
	win, tgt := site.win[:site.n], site.tgt[:site.n]
	x := win[site.s]
	if x.cls != clsFS {
		st.fsBad++
		debugf("unsupported %%fs instruction at %#x", base+uint64(x.off))
		return
	}
	// A jump rel32 reaches the veneer area from code below about 2 GiB; anything
	// else jumps through a pointer stored at the veneer (7 bytes).
	rel := int64(stubasm.FSVeneers) - int64(base) - int64(x.off)
	minLen := 7
	if rel > -(1<<31)+(1<<20) && rel < (1<<31)-(1<<20) {
		minLen = 5
	}
	a, b, ok := 0, 0, false
	if !unsure {
		a, b, ok = planFSWindow(win, tgt, site.s, minLen, func(lo, hi int) bool { return !cl.taken(lo, hi) })
	}
	if ok {
		ok = as.fsWindow(code, base, win, a, b, site.s, minLen, cl)
	}
	if ok {
		st.fs++
		return
	}
	if cl.taken(x.off, x.off+x.n) {
		st.fsBad++
		return
	}
	as.fsUD2[base+uint64(x.off)] = append([]byte(nil), code[x.off:x.off+x.n]...)
	code[x.off], code[x.off+1] = 0x0F, 0x0B
	cl.add(x.off, x.off+x.n)
	st.fsUD2++
	debugf("%%fs ud2 site %#x", base+uint64(x.off))
}

func (as *addressSpace) fsWindow(code []byte, base uint64, win []ins, a, b, s, minLen int, cl *claims) bool {
	v, ok := buildFSVeneer(code, base, win, a, b, s)
	if !ok {
		return false
	}
	w0off := win[a].off
	w0 := base + uint64(w0off)
	wlen := int(v.winLen)
	key := fsKey{addr: w0, h: sha256.Sum256(code[w0off : w0off+wlen])}
	va, cached := as.fsCache[key]
	if !cached {
		vlen := uint64(8+len(v.code)+15) &^ 15
		if as.fcur+vlen > stubasm.FSVeneers+stubasm.FSVeneersSize {
			return false
		}
		va = as.fcur
		as.fcur += vlen
		as.areaWrite(va, binary.LittleEndian.AppendUint64(nil, va+8))
		as.areaWrite(va+8, v.code)
		as.fsCache[key] = va
		as.fsVen = append(as.fsVen, &fsVenInfo{lo: va + 8, hi: va + 8 + uint64(len(v.code)), w0: w0, v: v})
		v.code = nil
	}
	entry := va + 8
	var j []byte
	if minLen == 5 {
		j = []byte{0xE9}
		j = binary.LittleEndian.AppendUint32(j, uint32(int32(int64(entry)-int64(w0+5))))
	} else {
		j = []byte{0xFF, 0x24, 0x25}
		j = binary.LittleEndian.AppendUint32(j, uint32(va))
	}
	for len(j) < wlen {
		j = append(j, 0x90)
	}
	copy(code[w0off:], j)
	cl.add(w0off, w0off+wlen)
	// A thread the Sentry hands back at one of the displaced instructions (after
	// a signal, say) re-enters the veneer there. The veneer of a cached window
	// was built from the same bytes, so its bounds are the ones just built.
	for _, bd := range v.bnd {
		if bd.o != 0 && bd.o < v.winLen {
			as.restartAt[w0+uint64(bd.o)] = entry + uint64(bd.v)
		}
	}
	return true
}

// fsUD2At returns the original bytes of a %fs instruction left as ud2 at a.
func (as *addressSpace) fsUD2At(a uint64) []byte {
	as.mu.Lock()
	defer as.mu.Unlock()
	return as.fsUD2[a]
}

// fsLocate finds the veneer a stopped thread's rip is in: one the debugger set
// up for the thread (t.dyn), or one in the shared area.
func (as *addressSpace) fsLocate(t *thread, rip uint64) *fsVenInfo {
	if d := t.dyn; d != nil && rip >= d.lo && rip < d.hi {
		return d
	}
	if rip < stubasm.FSVeneers || rip >= stubasm.FSVeneers+stubasm.FSVeneersSize {
		return nil
	}
	as.mu.Lock()
	defer as.mu.Unlock()
	i := sort.Search(len(as.fsVen), func(i int) bool { return as.fsVen[i].hi > rip })
	if i < len(as.fsVen) && as.fsVen[i].lo <= rip {
		return as.fsVen[i]
	}
	return nil
}

// unwind translates the instruction pointer of a thread stopped inside the veneer
// to the guest address the Sentry must see. restore is set when the guest's
// scratch register r1 is in the TEB save slot and must be put back.
func (info *fsVenInfo) unwind(rip uint64) (newRip uint64, r1 int, restore bool) {
	v := info.v
	off := uint32(rip - info.lo)
	switch {
	case off == v.restore:
		// the instruction has completed: continue after it
		return info.w0 + uint64(v.afterO), v.r1, true
	case off >= v.saved && off < v.restore:
		return info.w0 + uint64(v.fsO), v.r1, true
	}
	o := uint32(0)
	for _, bd := range v.bnd {
		if bd.v > off {
			break
		}
		o = bd.o
	}
	return info.w0 + uint64(o), 0, false
}

// fsFix is called with the stop address of a thread about to be parked. If the
// thread is inside a veneer, it returns the guest address to report and puts the
// guest's scratch register back in the thread's page.
func (as *addressSpace) fsFix(t *thread, rip uint64) (newRip uint64, ok bool) {
	if !as.p.fsRewrite {
		return rip, false
	}
	info := as.fsLocate(t, rip)
	if info == nil {
		return rip, false
	}
	newRip, r1, restore := info.unwind(rip)
	if restore {
		if v, ok := as.readTebSlot(t, stubasm.TebSave); ok {
			*t.reg(r1) = v
		} else {
			debugf("cannot read the TEB save slot of thread %d", t.idx)
		}
	}
	return newRip, true
}

// readTebSlot reads a qword of the stub thread's TEB.
func (as *addressSpace) readTebSlot(t *thread, off uint32) (uint64, bool) {
	if t.teb == 0 {
		var tbi [6]uintptr // ExitStatus, TebBaseAddress, ClientId (2), AffinityMask, Priority|BasePriority
		var n uint32
		if r, _, _ := pNtQueryInformationThread.Call(t.hThread, 0, uintptr(unsafe.Pointer(&tbi[0])), unsafe.Sizeof(tbi), uintptr(unsafe.Pointer(&n))); r != 0 {
			return 0, false
		}
		t.teb = uint64(tbi[1])
	}
	b, ok := as.readStub(t.teb+uint64(off), 8)
	if !ok {
		return 0, false
	}
	return binary.LittleEndian.Uint64(b), true
}

// fsRunInline runs the %fs instruction orig at rip, which was left as ud2, in a
// veneer of the thread's own page, and continues the thread there. It reports
// false if the instruction cannot be run that way.
func (as *addressSpace) fsRunInline(t *thread, ctx *ctxBuf, rip uint64, orig []byte) bool {
	list := []ins{{off: 0, n: len(orig), cls: clsFS}}
	v, ok := buildFSVeneer(orig, rip, list, 0, 0, 0)
	if !ok || len(v.code) > stubasm.PgDynSize {
		return false
	}
	copy(unsafe.Slice((*byte)(unsafe.Add(t.page, stubasm.PgDyn)), len(v.code)), v.code)
	lo := t.addr + stubasm.PgDyn
	t.dyn = &fsVenInfo{lo: lo, hi: lo + uint64(len(v.code)), w0: rip, v: v}
	v.code = nil
	*ctx.u64(ctxRipOff) = lo
	return ctx.set(t.hThread)
}
