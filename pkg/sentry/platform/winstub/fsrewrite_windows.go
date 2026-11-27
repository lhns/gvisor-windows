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
	"fmt"
	"log"
	"os"
	"sort"
	"unsafe"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/x86asm"
	"gvisor.dev/gvisor/pkg/sentry/profkeys"
)

// Rewrite mode (ADR 0020): on CPUs without FSGSBASE every %fs instruction of the
// guest becomes a jump to a veneer (see fsVeneer) that adds the guest's %fs base,
// kept in the thread's TEB, to the address by hand. This file holds the Windows
// side: placing veneers, and moving a stopped thread out of one.

// fsForceUD2 (WINSTUB_FSUD2=1, a diagnostic) leaves every %fs instruction to the debugger path.
var fsForceUD2 = os.Getenv("WINSTUB_FSUD2") != ""

// fsLog (WINSTUB_FSLOG=1) logs the threads moved out of veneers.
var fsLog = os.Getenv("WINSTUB_FSLOG") != ""

// fsVenInfo describes a veneer in the address space.
type fsVenInfo struct {
	lo, hi uint64 // code range (the entry is lo)
	w0     uint64 // guest address of the window it replaces
	v      *fsVeneer
	jlen   uint64 // length of the jump at w0 (0: the thread's own veneer, fsRunInline)
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
	target := stubasm.FSVeneers
	if as.fbigView != nil {
		target = stubasm.FSBig
	}
	rel := int64(target) - int64(base) - int64(x.off)
	lens := []int{7}
	if rel > -(1<<31)+(1<<20) && rel < (1<<31)-(1<<20) {
		lens = []int{5, 7} // (7 if the veneer ends up out of rel32 reach after all)
	}
	why := "start of a range"
	switch {
	case fsForceUD2:
		why = "WINSTUB_FSUD2"
	case !unsure:
		for _, minLen := range lens {
			a, b, ok := planFSWindow(win, tgt, site.s, minLen, func(lo, hi int) bool { return !cl.taken(lo, hi) })
			if !ok {
				why = fmt.Sprintf("no window of %d bytes", minLen)
				break // (a longer jump needs a longer window)
			}
			if as.fsWindow(code, base, win, a, b, site.s, minLen, cl) {
				st.fs++
				if profkeys.On {
					profkeys.Count(fmt.Sprintf("winstub %%fs site: veneer, jump of %d bytes", minLen), 0)
				}
				return
			}
			why = "veneer area full or veneer not buildable"
		}
	}
	if profkeys.On {
		profkeys.Count("winstub %fs site: debugger, "+why, 0)
	}
	if cl.taken(x.off, x.off+x.n) {
		st.fsBad++
		return
	}
	as.fsUD2[base+uint64(x.off)] = append([]byte(nil), code[x.off:x.off+x.n]...)
	as.fsWhy[base+uint64(x.off)] = why
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
		vlen := uint64(8+len(v.code)+7) &^ 7
		if va = as.fsAlloc(vlen); va == 0 {
			return false
		}
		if minLen == 5 {
			if d := int64(va+8) - int64(w0+5); d != int64(int32(d)) {
				return false // (the veneer ended up out of reach of this code: only the low area can do that)
			}
		}
		as.fsPut(va, binary.LittleEndian.AppendUint64(nil, va+8))
		as.fsPut(va+8, v.code)
		as.fsCache[key] = va
		vi := &fsVenInfo{lo: va + 8, hi: va + 8 + uint64(len(v.code)), w0: w0, v: v, jlen: 7}
		if minLen == 5 {
			vi.jlen = 5
		}
		i := sort.Search(len(as.fsVen), func(i int) bool { return as.fsVen[i].lo > vi.lo })
		as.fsVen = append(as.fsVen, nil)
		copy(as.fsVen[i+1:], as.fsVen[i:])
		as.fsVen[i] = vi
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
		j = append(j, 0xCC) // (planFSWindow: the other instruction starts are here)
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

// windowResume handles the int3 a thread hits when it enters a patched %fs
// window at one of its inner instruction starts, which the platform reports
// as the thread's address when it stops inside the veneer (fsFix) and which a
// signal handler may return to with a plain ret: the thread continues in the
// veneer at that instruction.
func (as *addressSpace) windowResume(t *thread, ctx *ctxBuf, a uint64) bool {
	as.mu.Lock()
	target, ok := as.restartAt[a]
	as.mu.Unlock()
	if !ok || a < stubasm.GuestMin {
		return false
	}
	if b, ok := as.readStub(a, 1); !ok || b[0] != 0xCC {
		return false
	}
	*ctx.u64(ctxRipOff) = target
	return ctx.set(t.hThread)
}

// fsUD2At returns the original bytes of a %fs instruction left as ud2 at a.
func (as *addressSpace) fsUD2At(a uint64) []byte {
	as.mu.Lock()
	defer as.mu.Unlock()
	if b := as.fsUD2[a]; b != nil {
		as.fsHits[a]++
		if profkeys.On {
			profkeys.Count("winstub %fs debugger run: "+as.fsWhy[a], 0)
		}
		return b
	}
	return nil
}

// fsLocate finds the veneer a stopped thread's rip is in: one the debugger set
// up for the thread (t.dyn), or one in the shared area.
func (as *addressSpace) fsLocate(t *thread, rip uint64) *fsVenInfo {
	if d := t.dyn; d != nil && rip >= d.lo && rip < d.hi {
		return d
	}
	inBig := rip >= stubasm.FSBig && rip < stubasm.FSBig+stubasm.FSBigSize
	if !inBig && (rip < stubasm.Veneers || rip >= stubasm.FSVeneers+stubasm.FSVeneersSize) {
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

// inJump reports whether a thread stopped at rip inside the veneer would be
// reported at an address inside the bytes of the jump that replaced the window
// (a displaced instruction there, or the end of the %fs instruction): a thread
// returning there with a plain ret would run the jump's bytes. Every other inner
// instruction start lies in the int3 filler (windowResume).
func (info *fsVenInfo) inJump(rip uint64) bool {
	r, _, _ := info.unwind(rip)
	return r > info.w0 && r < info.w0+info.jlen
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
	n := as.nFSFix.Add(1)
	newRip, r1, restore := info.unwind(rip)
	// The Sentry hands newRip back, inside the window the jump replaced: it must lead
	// into the veneer again (patchText registered it, unless the entry was purged since).
	if newRip > info.w0 && newRip < info.w0+uint64(info.v.winLen) {
		as.mu.Lock()
		_, had := as.restartAt[newRip]
		for _, bd := range info.v.bnd {
			if bd.o == uint32(newRip-info.w0) {
				as.restartAt[newRip] = info.lo + uint64(bd.v)
				break
			}
		}
		as.mu.Unlock()
		if fsLog && !had {
			log.Printf("winstub: FSFIX: no restart entry for %#x (registered now)", newRip)
		}
	}
	if fsLog && n <= 200 {
		log.Printf("winstub: FSFIX stub pid %d thread %d: stopped at %#x in the veneer of %#x, back to %#x (restore r%d: %v)", as.wpid, t.idx, rip, info.w0, newRip, r1, restore)
	}
	if restore {
		if v, ok := as.readTebSlot(t, stubasm.TebSave); ok {
			if fsLog && n <= 200 {
				log.Printf("winstub: FSFIX r%d was %#x in the context, the save slot holds %#x (teb %#x, flags %#x)", r1, *t.reg(r1), v, t.teb, *t.u64(stubasm.PgFlags))
			}
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
	as.nFSInline.Add(1)
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

// fsSplitRun handles the int3 that stands for an %fs instruction split between
// two pieces of text (see patchText): the whole instruction is read from the
// guest now and run in the thread's own veneer, as for a ud2 site.
func (as *addressSpace) fsSplitRun(t *thread, ctx *ctxBuf, a uint64) bool {
	as.mu.Lock()
	head := as.fsSplit[a]
	as.mu.Unlock()
	if head == nil {
		return false
	}
	if profkeys.On {
		profkeys.Count("winstub %fs debugger run: split between pieces", 0)
	}
	*ctx.u64(ctxRipOff) = a // the int3 itself (the exception reports the next byte)
	next := a + uint64(len(head))
	rest, ok := as.readStub(next, 16)
	if !ok {
		// The piece that holds the rest is not mapped: an instruction fetch fault
		// there, the Sentry maps it and the thread comes back to the int3.
		as.park(t, ctx, request{kind: reqFault, sig: 11, code: 1, addr: next, at: hostarch.AccessType{Execute: true}})
		return true
	}
	joined := append(append([]byte(nil), head...), rest...)
	n := 0
	func() {
		defer func() { recover() }()
		if inst, err := x86asm.Decode(joined, 64); err == nil && inst.Len >= len(head) {
			n = inst.Len
		}
	}()
	if n == 0 || !as.fsRunInline(t, ctx, a, joined[:n]) {
		as.park(t, ctx, request{kind: reqFault, sig: 4, code: 1, addr: a})
	}
	return true
}

// fsAlloc returns the address of n bytes for a veneer, 0 if there is no room: in
// the big area (committing it as it fills), else in the low area and the end of
// the syscall veneers' space.
func (as *addressSpace) fsAlloc(n uint64) uint64 {
	if as.fbigView != nil && as.gcur+n <= stubasm.FSBig+stubasm.FSBigSize {
		ok := true
		for as.gcommit < as.gcur+n {
			if _, err := windows.VirtualAlloc(uintptr(as.fbigView)+uintptr(as.gcommit-stubasm.FSBig), uintptr(stubasm.FSBigChunk), windows.MEM_COMMIT, windows.PAGE_READWRITE); err != nil {
				debugf("commit of the %%fs veneer area: %v", err)
				ok = false
				break
			}
			as.gcommit += stubasm.FSBigChunk
		}
		if ok {
			va := as.gcur
			as.gcur += n
			return va
		}
	}
	switch {
	case as.fcur+n <= stubasm.FSVeneers+stubasm.FSVeneersSize:
		va := as.fcur
		as.fcur += n
		return va
	case as.ftop-n >= as.vcur:
		// The area is full: take room from the end of the private syscall
		// veneers, which use little of theirs.
		as.ftop -= n
		return as.ftop
	}
	return 0
}

// fsPut writes veneer code at an address fsAlloc returned.
func (as *addressSpace) fsPut(va uint64, b []byte) {
	if va >= stubasm.FSBig && va < stubasm.FSBig+stubasm.FSBigSize {
		copy(unsafe.Slice((*byte)(unsafe.Add(as.fbigView, va-stubasm.FSBig)), len(b)), b)
		return
	}
	as.areaWrite(va, b)
}
