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
	"encoding/binary"
	"math/rand"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/cpuid"
	pkgcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
)

const (
	tlsBase    = 0x900000
	fsFaultOff = 0x2000000
)

// fsGuest is code full of %fs accesses; it reports results with syscall 500.
type fsGuest struct {
	start     uint64
	code      []byte
	faultInsn uint64
}

func buildFSGuest(base uint64) *fsGuest {
	a := &asm{base: base}
	g := &fsGuest{}
	a.emit(bytes.Repeat([]byte{0x90}, 64)...) // (the sites in the first bytes of a text range are handled by the debugger)
	g.start = a.pc()
	report := func(src ...byte) {
		a.emit(src...)                // mov rdi,<src>
		a.emit(0xb8, 0xf4, 1, 0, 0)   // mov eax,500
		a.emit(0x0f, 0x05)            // syscall
	}
	rax := []byte{0x48, 0x89, 0xc7} // mov rdi,rax
	a.emit(0x64, 0x48, 0x8b, 0x04, 0x25, 0x28, 0, 0, 0) // mov %fs:0x28,%rax
	report(rax...)
	a.emit(0xb9, 8, 0, 0, 0)          // mov $8,%ecx
	a.emit(0x64, 0x48, 0x8b, 0x01)    // mov %fs:(%rcx),%rax: 4 bytes, needs neighbours
	report(rax...)
	a.emit(0xbe, 0, 1, 0, 0)                      // mov $0x100,%esi
	a.emit(0xbf, 0x20, 0, 0, 0)                   // mov $0x20,%edi
	a.emit(0x64, 0x48, 0x8b, 0x44, 0x7e, 0x10)    // mov %fs:0x10(%rsi,%rdi,2),%rax
	report(rax...)
	a.emit(0x64, 0x48, 0x83, 0x04, 0x25, 0x40, 0, 0, 0, 5) // addq $5,%fs:0x40
	a.emit(0x64, 0x48, 0x8b, 0x04, 0x25, 0x40, 0, 0, 0)    // mov %fs:0x40,%rax
	report(rax...)
	a.emit(0x64, 0xc7, 0x04, 0x25, 0x48, 0, 0, 0, 0x34, 0x12, 0, 0) // movl $0x1234,%fs:0x48
	a.emit(0x64, 0x8b, 0x04, 0x25, 0x48, 0, 0, 0)                   // mov %fs:0x48,%eax
	report(rax...)
	a.emit(0x48, 0xbb, 0xaa, 0x99, 0xff, 0xee, 0xdd, 0xcc, 0xbb, 0xaa) // movabs $0xaabbccddeeff99aa,%rbx
	a.emit(0x64, 0x8a, 0x3c, 0x25, 0x28, 0, 0, 0)                      // mov %fs:0x28,%bh
	report(0x48, 0x89, 0xdf)                                            // mov rdi,rbx
	a.emit(0xbb, 0xef, 0xbe, 0, 0)                                      // mov $0xbeef,%ebx
	g.faultInsn = a.emit(0x64, 0x48, 0x8b, 0x04, 0x25, 0, 0, 0, 2)      // mov %fs:0x2000000,%rax
	report(rax...)
	a.emit(0xb9, 0x28, 0, 0, 0) // mov $0x28,%ecx
	a.emit(0xeb, 0x00)          // jmp +0: no neighbour to take along
	a.emit(0x64, 0x8b, 0x01)    // mov %fs:(%rcx),%eax
	a.emit(0xeb, 0x00)
	report(rax...)
	a.emit(0xeb, 0xfe)
	g.code = a.b
	return g
}

func tlsData() []byte {
	d := make([]byte, pageSz)
	put := func(off int, v uint64) { copy(d[off:], le64(v)) }
	put(0x28, 0x1111)
	put(8, 0x2222)
	put(0x150, 0x3333)
	put(0x40, 0x10)
	return d
}

// TestFSInstructions runs %fs accesses of every shape the veneers handle, near
// the veneer area and far above it (a pointer jump). It works in both modes.
func TestFSInstructions(t *testing.T) {
	// full: the veneer area is used up, so the veneers go to the end of the
	// syscall veneers' space.
	for _, c := range []struct {
		base uint64
		full bool
	}{{0xC00000, false}, {0x3_0000_0000, false}, {0xC00000, true}, {0x3_0000_0000, true}} {
		base := c.base
		t.Run(fmt.Sprintf("%#x full=%v", base, c.full), func(t *testing.T) {
			h := newHarness(t)
			mm := h.newMM()
			if c.full {
				as := mm.as.(*addressSpace)
				as.mu.Lock()
				as.gcur = stubasm.FSBig + stubasm.FSBigSize
				as.fcur = stubasm.FSVeneers + stubasm.FSVeneersSize
				as.mu.Unlock()
			}
			g := buildFSGuest(base)
			h.mapAt(mm, base, h.pages(g.code, 2), rx)
			h.mapAt(mm, 0x700000, h.pages(nil, 4), rw)
			h.mapAt(mm, tlsBase, h.pages(tlsData(), 1), rw)
			c := h.p.NewContext(pkgcontext.Background())
			defer c.Release()
			ac := newAC(g.start, 0x700000+4*pageSz-64)
			r := &ac.StateData().Regs
			r.Fs_base = tlsBase
			for i, want := range []uint64{0x1111, 0x2222, 0x3333, 0x15, 0x1234, 0xaabbccddeeff11aa} {
				if nr := h.syscall(c, mm, ac); nr != 500 || r.Rdi != want {
					t.Fatalf("step %d: syscall %d(%#x), want 500(%#x)", i, nr, r.Rdi, want)
				}
			}
			res := h.sw(c, mm, ac)
			if res.err != platform.ErrContextSignal || res.si == nil || res.si.Signo != int32(linux.SIGSEGV) {
				t.Fatalf("want SIGSEGV, got %v %+v", res.err, res.si)
			}
			if res.si.Addr() != tlsBase+fsFaultOff || !res.at.Read || res.at.Write {
				t.Fatalf("fault: addr %#x at %v", res.si.Addr(), res.at)
			}
			if r.Rip != g.faultInsn || r.Rbx != 0xbeef {
				t.Fatalf("fault: rip %#x (want %#x), rbx %#x (want 0xbeef)", r.Rip, g.faultInsn, r.Rbx)
			}
			// map the page and resume at the instruction
			h.mapAt(mm, tlsBase+fsFaultOff, h.pages(le64(0x7777), 1), rw)
			for i, want := range []uint64{0x7777, 0x1111} {
				if nr := h.syscall(c, mm, ac); nr != 500 || r.Rdi != want {
					t.Fatalf("after the fault, step %d: syscall %d(%#x), want 500(%#x)", i, nr, r.Rdi, want)
				}
			}
			if r.Rbx != 0xbeef {
				t.Errorf("rbx = %#x, want 0xbeef", r.Rbx)
			}
		})
	}
}

// TestFSInterrupt interrupts a loop of %fs accesses over and over: the thread
// is stopped inside veneers at all kinds of places, and must come out with its
// registers and its progress intact, never at an address under the jump to a
// veneer, and soon (a thread that is mostly in veneers is still caught).
func TestFSInterrupt(t *testing.T) {
	h := newHarness(t)
	mm := h.newMM()
	const base = 0xC00000
	a := &asm{base: base}
	a.emit(bytes.Repeat([]byte{0x90}, 64)...) // (the sites in the first bytes of a text range are handled by the debugger)
	loop := a.emit(0x64, 0x48, 0x8b, 0x04, 0x25, 0x28, 0, 0, 0) // mov %fs:0x28,%rax
	a.emit(0x64, 0x48, 0x01, 0x0c, 0x25, 0x40, 0, 0, 0)         // add %rcx,%fs:0x40
	win := a.emit(0x64, 0x48, 0x8b, 0x02)                       // mov %fs:(%rdx),%rax: first in a window, so
	inc := a.emit(0x48, 0xff, 0xc1)                             // inc %rcx is under the jump in rewrite mode (a stop there goes back to win)
	win2 := a.emit(0x64, 0x4c, 0x03, 0x02)                      // add %fs:(%rdx),%r8: it cannot run again, so
	inc2 := a.emit(0x49, 0xff, 0xc1)                            // a stop at inc %r9 under its jump waits
	jmp := a.emit(0xeb, byte(256-int(a.pc()+2-loop)))           // jmp loop
	end := a.pc()
	for _, minLen := range []int{5, 7} { // (the incs lie under a jump of either length)
		wins, _ := FSWindows(a.b, minLen)
		planned, planned2 := false, false
		for _, w := range wins {
			planned = planned || uint64(w.Lo) == win-base && uint64(w.Hi) == win2-base
			planned2 = planned2 || uint64(w.Lo) == win2-base && uint64(w.Hi) == jmp-base
		}
		if !planned || !planned2 {
			t.Fatalf("windows [mov %%fs:(%%rdx); inc] %v, [add %%fs:(%%rdx); inc] %v planned (jump of %d bytes): %+v", planned, planned2, minLen, wins)
		}
	}
	h.mapAt(mm, base, h.pages(a.b, 1), rx)
	tfr := h.pages(tlsData(), 1)
	h.mapAt(mm, tlsBase, tfr, rw)
	tls, err := h.mf.MapInternal(tfr, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	ac := newAC(loop, 0)
	r := &ac.StateData().Regs
	r.Fs_base, r.Rdx = tlsBase, 0x28
	r.Rbx, r.Rsi, r.Rdi = 0xb1b1b1b1b1b1b1b1, 0x5151515151515151, 0xd1d1d1d1d1d1d1d1
	var last uint64
	t0 := time.Now()
	for i := 0; i < 400; i++ {
		go func() {
			time.Sleep(time.Duration(20+rand.Intn(300)) * time.Microsecond)
			c.Interrupt()
		}()
		res := h.sw(c, mm, ac)
		if res.err != platform.ErrContextInterrupt {
			t.Fatalf("interrupt %d: got %v (si %+v)", i, res.err, res.si)
		}
		if r.Rbx != 0xb1b1b1b1b1b1b1b1 || r.Rsi != 0x5151515151515151 || r.Rdi != 0xd1d1d1d1d1d1d1d1 {
			t.Fatalf("interrupt %d: scratch registers clobbered: rbx %#x rsi %#x rdi %#x (rip %#x)", i, r.Rbx, r.Rsi, r.Rdi, r.Rip)
		}
		if r.Rip < loop || r.Rip >= end {
			t.Fatalf("interrupt %d: rip %#x outside the loop [%#x,%#x)", i, r.Rip, loop, end)
		}
		if h.p.fsRewrite && (r.Rip > win && r.Rip <= inc || r.Rip > win2 && r.Rip <= inc2) {
			t.Fatalf("interrupt %d: rip %#x is under the jump of a window (%#x, %#x)", i, r.Rip, win, win2)
		}
		if r.Rcx < last {
			t.Fatalf("interrupt %d: rcx went back from %d to %d", i, last, r.Rcx)
		}
		last = r.Rcx
		// The add of this iteration has happened exactly when rip is past it: no
		// instruction is lost or repeated, whichever way the thread was moved out.
		// (rcx passes 2^32 within seconds: halve the even factor, or the product wraps.)
		want := uint64(0x10)
		if r.Rcx%2 == 0 {
			want += r.Rcx / 2 * (r.Rcx - 1)
		} else {
			want += (r.Rcx - 1) / 2 * r.Rcx
		}
		if r.Rip >= win && r.Rip < win2 { // (past the add, not yet past the inc)
			want += r.Rcx
		}
		if got := binary.LittleEndian.Uint64(tls.Head().ToSlice()[0x40:]); got != want {
			t.Fatalf("interrupt %d: the counter in memory is %#x, want %#x (rcx %d, rip %#x)", i, got, want, r.Rcx, r.Rip)
		}
	}
	if last == 0 {
		t.Fatalf("no progress")
	}
	if d := time.Since(t0); d > 10*time.Second {
		t.Errorf("400 interrupts took %v (0.3 s when no veneer refuses them)", d)
	}
}

// TestFSReturnIntoWindow: the %fs access of a window faults, so the thread
// stops inside the veneer and the platform reports the address of the %fs
// instruction, an inner instruction start of the window. A signal handler that
// returns there with a plain ret (Go's asyncPreempt: it pushes the reported pc
// and jumps to its handler) must continue with that instruction, not with the
// bytes of the jump that replaced the window (a Podman engine died that way,
// docs/results/1q-window-resume.md).
func TestFSReturnIntoWindow(t *testing.T) {
	h := newHarness(t)
	mm := h.newMM()
	const base, stackAt = 0xC00000, 0x700000
	a := &asm{base: base}
	a.emit(bytes.Repeat([]byte{0x90}, 64)...) // (the sites in the first bytes of a text range are handled by the debugger)
	start := a.emit(append([]byte{0xba}, le32(fsFaultOff)...)...) // mov $fsFaultOff,%edx: the window's first instruction
	addInsn := a.emit(0x64, 0x48, 0x01, 0x0a)                     // add %rcx,%fs:(%rdx)
	report := a.emit(0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05)             // mov $500,%eax; syscall
	a.emit(0xeb, byte(256-int(a.pc()+2-report)))                 // (never run: makes report a branch target, so
	// the window cannot take it along and begins at the mov: the add is an inner instruction start)
	retInsn := a.emit(0xc3) // the "handler": ret
	h.mapAt(mm, base, h.pages(a.b, 1), rx)
	h.mapAt(mm, tlsBase, h.pages(tlsData(), 1), rw)
	sfr := h.pages(nil, 1)
	h.mapAt(mm, stackAt, sfr, rw)
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	ac := newAC(start, stackAt+pageSz-64)
	r := &ac.StateData().Regs
	r.Fs_base, r.Rcx = tlsBase, 7
	res := h.sw(c, mm, ac)
	if res.err != platform.ErrContextSignal || res.si.Addr() != tlsBase+fsFaultOff {
		t.Fatalf("want the fault of the add, got %v %+v", res.err, res.si)
	}
	if r.Rip != addInsn {
		t.Fatalf("fault at rip %#x, want the add at %#x", r.Rip, addInsn)
	}
	cfr := h.pages(le64(0x100), 1)
	h.mapAt(mm, tlsBase+fsFaultOff, cfr, rw)
	// The handler's call: push the reported pc, continue at the handler.
	r.Rsp -= 8
	st, err := h.mf.MapInternal(sfr, hostarch.ReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(st.Head().ToSlice()[r.Rsp-stackAt:], r.Rip)
	r.Rip = retInsn
	if nr := h.syscall(c, mm, ac); nr != 500 {
		t.Fatalf("after the ret: syscall %d at %#x, want 500", nr, r.Rip)
	}
	cnt, err := h.mf.MapInternal(cfr, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint64(cnt.Head().ToSlice()); got != 0x107 || r.Rdx != fsFaultOff {
		t.Errorf("after the ret: counter %#x (want 0x107: the add ran once), rdx %#x", got, r.Rdx)
	}
}

// TestFSFaultUnderJump: a displaced instruction that is not the %fs one faults
// where it lies under the bytes of the jump that replaced its window. The
// platform must report an address a signal handler can return to: with a
// plain ret, and through sigreturn after repairing the fault in the context.
// That is the window start when the instructions before it can run again and
// write nothing it reads; otherwise the planner must not put a faulting
// instruction there (docs/results/1s-veneers.md).
func TestFSFaultUnderJump(t *testing.T) {
	const base, stackAt, dataAt = 0xC00000, 0x700000, 0x3000000
	cases := []struct {
		name     string
		fs       []byte
		rollBack bool   // rewrite mode reports the window start
		repair   bool   // the handler sets rbx in the context and sigreturns (else: maps the page, ret)
		r8       uint64 // after the handler
	}{
		// mov %fs:(%rdx),%r8 reads only rdx and the load does not read r8: it can run again.
		{"reexecutable", []byte{0x64, 0x4c, 0x8b, 0x02}, true, false, 0x2222},
		// add %fs:(%rdx),%r8 cannot (it reads what it writes): no window puts the
		// load under the jump.
		{"not reexecutable", []byte{0x64, 0x4c, 0x03, 0x02}, false, false, 0x2222 + 5},
		// mov %fs:(%rdx),%rbx writes the load's base: a handler that repairs rbx
		// and resumes at the window start would load it again and fault forever.
		{"repair", []byte{0x64, 0x48, 0x8b, 0x1a}, false, true, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			mm := h.newMM()
			a := &asm{base: base}
			a.emit(bytes.Repeat([]byte{0x90}, 64)...) // (the sites in the first bytes of a text range are handled by the debugger)
			fsInsn := a.emit(tc.fs...)
			load := a.emit(0x48, 0x8b, 0x13)                  // mov (%rbx),%rdx: faults, under the jump of [fs; load]
			report := a.emit(0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05) // mov $500,%eax; syscall
			a.emit(0xeb, byte(256-int(a.pc()+2-report)))      // (never run: makes report a branch target)
			retInsn := a.emit(0xc3)                           // the "handler": ret
			for _, minLen := range []int{5, 7} {
				wins, _ := FSWindows(a.b, minLen)
				under := false
				for _, w := range wins {
					for _, o := range w.Insn {
						under = under || uint64(o) == load-base && o-w.Lo > 0 && o-w.Lo < minLen
					}
				}
				if under != tc.rollBack {
					t.Fatalf("jump of %d bytes: the load under the jump in a planned window: %v, want %v (%+v)", minLen, under, tc.rollBack, wins)
				}
			}
			h.mapAt(mm, base, h.pages(a.b, 1), rx)
			h.mapAt(mm, tlsBase, h.pages(tlsData(), 1), rw)
			sfr := h.pages(nil, 1)
			h.mapAt(mm, stackAt, sfr, rw)
			c := h.p.NewContext(pkgcontext.Background())
			defer c.Release()
			ac := newAC(fsInsn, stackAt+pageSz-64)
			r := &ac.StateData().Regs
			r.Fs_base, r.Rdx, r.R8, r.Rbx = tlsBase, 8, 5, dataAt
			faultAt := uint64(dataAt)
			if tc.repair {
				faultAt = 0x2222 // (rbx comes from the TLS word at 8)
			}
			res := h.sw(c, mm, ac)
			if res.err != platform.ErrContextSignal || res.si.Addr() != faultAt {
				t.Fatalf("want the fault of the load at %#x, got %v %+v", faultAt, res.err, res.si)
			}
			want := load
			if h.p.fsRewrite && tc.rollBack {
				want = fsInsn
			}
			if r.Rip != want {
				t.Fatalf("fault at rip %#x, want %#x (the %%fs instruction %#x, the load %#x)", r.Rip, want, fsInsn, load)
			}
			h.mapAt(mm, dataAt, h.pages(le64(0x4444), 1), rw)
			if tc.repair {
				r.Rbx = dataAt // the handler's repair; sigreturn resumes at the reported rip
			} else {
				// The handler's call: push the reported pc, continue at the handler.
				r.Rsp -= 8
				st, err := h.mf.MapInternal(sfr, hostarch.ReadWrite)
				if err != nil {
					t.Fatal(err)
				}
				binary.LittleEndian.PutUint64(st.Head().ToSlice()[r.Rsp-stackAt:], r.Rip)
				r.Rip = retInsn
			}
			res = h.sw(c, mm, ac)
			if res.err != nil || r.Orig_rax != 500 {
				t.Fatalf("after the handler: %v %+v at %#x, want syscall 500", res.err, res.si, r.Rip)
			}
			if r.Rdx != 0x4444 || r.R8 != tc.r8 {
				t.Errorf("after the handler: rdx %#x (want 0x4444), r8 %#x (want %#x: the %%fs instruction ran once)", r.Rdx, r.R8, tc.r8)
			}
		})
	}
}

// TestFSSpeed times a loop of %fs loads against the same loop with a plain
// load: the cost of a veneer in the mode the test runs in (the CI legs compare).
func TestFSSpeed(t *testing.T) {
	h := newHarness(t)
	mm := h.newMM()
	const base = 0xC00000
	const iters = 5_000_000
	build := func(load ...byte) (code []byte) {
		a := &asm{base: base}
		a.emit(bytes.Repeat([]byte{0x90}, 64)...) // (the sites in the first bytes of a text range are handled by the debugger)
		a.emit(append([]byte{0xb9}, le32(iters)...)...) // mov $iters,%ecx
		loop := a.emit(load...)
		a.emit(0xff, 0xc9) // dec %ecx
		a.emit(0x75, byte(256-int(a.pc()+2-loop)))
		a.emit(0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05)
		return a.b
	}
	h.mapAt(mm, tlsBase, h.pages(tlsData(), 1), rw)
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	run := func(name string, at uint64, code []byte) time.Duration {
		h.mapAt(mm, at, h.pages(code, 1), rx)
		ac := newAC(at+64, 0)
		r := &ac.StateData().Regs
		r.Fs_base, r.Rdx = tlsBase, tlsBase
		start := time.Now()
		if nr := h.syscall(c, mm, ac); nr != 500 {
			t.Fatalf("%s: syscall %d", name, nr)
		}
		return time.Since(start)
	}
	plain := run("plain", base, build(0x48, 0x8b, 0x42, 0x28))                                         // mov 0x28(%rdx),%rax
	fs := run("fs", base+0x10000, build(0x64, 0x48, 0x8b, 0x04, 0x25, 0x28, 0, 0, 0))                 // mov %fs:0x28,%rax
	t.Logf("FSSPEED rewrite=%v plain %.2f ns/iter, %%fs %.2f ns/iter", h.p.fsRewrite,
		float64(plain.Nanoseconds())/iters, float64(fs.Nanoseconds())/iters)
}

// TestAVX512State checks that zmm registers (the low and the high half of the
// register file) and an opmask register survive a syscall, a fault and an
// interrupt: it needs AVX-512 on the host and skips without.
func TestAVX512State(t *testing.T) {
	if !cpuid.HostFeatureSet().HasFeature(cpuid.X86FeatureAVX512F) {
		t.Skip("no AVX-512")
	}
	h := newHarness(t)
	mm := h.newMM()
	a := &asm{base: textBase}
	start := a.emit(0x62, 0xf1, 0xfe, 0x48, 0x6f, 0x07)                  // vmovdqu64 (%rdi),%zmm0
	a.emit(0x62, 0xe1, 0xfe, 0x48, 0x6f, 0x47, 0x01)                     // vmovdqu64 64(%rdi),%zmm16
	a.emit(0xc4, 0xe1, 0xf8, 0x90, 0x8f, 0x80, 0, 0, 0)                  // kmovq 128(%rdi),%k1
	a.emit(0xb8, 39, 0, 0, 0, 0x0f, 0x05)                                // syscall
	store := func(off int) uint64 {
		p := a.emit(0x62, 0xf1, 0xfe, 0x48, 0x7f, 0x86)                  // vmovdqu64 %zmm0,off(%rsi)
		a.emit(le32(uint32(off))...)
		a.emit(0x62, 0xe1, 0xfe, 0x48, 0x7f, 0x86)                       // vmovdqu64 %zmm16,off+64(%rsi)
		a.emit(le32(uint32(off + 64))...)
		a.emit(0xc4, 0xe1, 0xf8, 0x91, 0x8e)                             // kmovq %k1,off+128(%rsi)
		a.emit(le32(uint32(off + 128))...)
		a.emit(0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05)                          // syscall 500
		return p
	}
	store(0)
	a.emit(0x48, 0x8b, 0x04, 0x25, 0x00, 0x00, 0x80, 0x00) // mov [0x800000],%rax: a fault
	store(256)
	spin := a.emit(0xeb, 0xfe)
	third := store(512)
	h.mapAt(mm, textBase, h.pages(a.b, 1), rx)
	pat := make([]byte, 192)
	for i := range pat {
		pat[i] = byte(i*7 + 3)
	}
	h.mapAt(mm, 0x500000, h.pages(pat, 1), rw)
	outFR := h.pages(nil, 1)
	h.mapAt(mm, 0x510000, outFR, rw)
	out := func(off int) []byte {
		bs, err := h.mf.MapInternal(outFR, hostarch.Read)
		if err != nil {
			t.Fatal(err)
		}
		return append([]byte(nil), bs.Head().ToSlice()[off:off+136]...)
	}
	check := func(what string, off int) {
		t.Helper()
		got := out(off)
		want := append(append(append([]byte(nil), pat[:64]...), pat[64:128]...), pat[128:136]...)
		if string(got) != string(want) {
			t.Fatalf("%s: zmm0, zmm16, k1 = %x, want %x", what, got, want)
		}
	}
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	ac := newAC(start, 0)
	r := &ac.StateData().Regs
	r.Rdi, r.Rsi = 0x500000, 0x510000
	if nr := h.syscall(c, mm, ac); nr != 39 {
		t.Fatalf("syscall %d", nr)
	}
	if nr := h.syscall(c, mm, ac); nr != 500 {
		t.Fatalf("syscall %d", nr)
	}
	check("across a syscall", 0)
	res := h.sw(c, mm, ac)
	if res.err != platform.ErrContextSignal {
		t.Fatalf("want a fault, got %v", res.err)
	}
	h.mapAt(mm, 0x800000, h.pages(nil, 1), rw)
	if nr := h.syscall(c, mm, ac); nr != 500 {
		t.Fatalf("syscall %d", nr)
	}
	check("across a fault", 256)
	r.Rip = spin
	go func() {
		time.Sleep(300 * time.Millisecond)
		c.Interrupt()
	}()
	if res := h.sw(c, mm, ac); res.err != platform.ErrContextInterrupt {
		t.Fatalf("want an interrupt, got %v", res.err)
	}
	r.Rip = third
	if nr := h.syscall(c, mm, ac); nr != 500 {
		t.Fatalf("syscall %d", nr)
	}
	check("across an interrupt", 512)
}

// TestFSInstructionAfterSplit maps text in two pieces, the second beginning in the
// middle of an instruction (xorps %xmm15,%xmm15) that is followed by a Go-style
// `mov %fs:-8,%r14`: the sweep over the second piece starts at a wrong boundary,
// unless it is given the last bytes of the first.
func TestFSInstructionAfterSplit(t *testing.T) {
	h := newHarness(t)
	mm := h.newMM()
	const base = 0x1_0000_0000
	page1 := make([]byte, pageSz)
	for i := range page1 {
		page1[i] = 0x90 // (nops: a decoder started in them stays in step)
	}
	copy(page1[pageSz-3:], []byte{0x45, 0x0f, 0x57}) // xorps %xmm15,%xmm15, first three bytes
	page2 := []byte{0xff,
		0x64, 0x4c, 0x8b, 0x34, 0x25, 0xf8, 0xff, 0xff, 0xff, // mov %fs:-8,%r14
		0x4c, 0x89, 0xf7, // mov %r14,%rdi
		0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05} // syscall 500
	h.mapAt(mm, base, h.pages(page1, 1), rx)
	h.mapAt(mm, base+pageSz, h.pages(page2, 1), rx)
	d := tlsData()
	copy(d[0xf8:], le64(0x5eed))
	h.mapAt(mm, tlsBase, h.pages(d, 1), rw)
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	ac := newAC(base+pageSz-3, 0)
	ac.StateData().Regs.Fs_base = tlsBase + 0x100
	if nr := h.syscall(c, mm, ac); nr != 500 || ac.StateData().Regs.Rdi != 0x5eed {
		t.Fatalf("syscall %d, rdi %#x, want 500 and 0x5eed", nr, ac.StateData().Regs.Rdi)
	}
}
