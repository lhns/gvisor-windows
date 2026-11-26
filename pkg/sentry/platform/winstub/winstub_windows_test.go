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
	"encoding/binary"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
	"os"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"gvisor.dev/gvisor/pkg/abi/linux"
	pkgcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/usermem"
)

const (
	textBase  = 0x400000
	pageSz    = 4096
	switchMax = 60 * time.Second
)

// testMM is the part of a memory manager the platform uses.
type testMM struct {
	usermem.IO
	as    platform.AddressSpace
	mmaps int
}

func (m *testMM) MMap(ctx pkgcontext.Context, opts memmap.MMapOpts) (hostarch.Addr, error) {
	m.mmaps++
	return opts.Addr, nil
}
func (m *testMM) AddressSpace() platform.AddressSpace { return m.as }
func (m *testMM) FindVMAByName(ar hostarch.AddrRange, hint string) (hostarch.Addr, uint64, error) {
	return 0, 0, errors.New("none")
}

type harness struct {
	t  *testing.T
	p  *Platform
	mf *pgalloc.MemoryFile
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	p, err := New(platform.Options{})
	if err != nil {
		t.Skipf("platform unavailable: %v", err)
	}
	t.Cleanup(p.Close)
	mf, err := pgalloc.NewMemoryFile(nil, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionManual,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		t.Fatalf("NewMemoryFile: %v", err)
	}
	t.Cleanup(mf.Destroy)
	return &harness{t: t, p: p, mf: mf}
}

func (h *harness) newMM() *testMM {
	as, err := h.p.NewAddressSpace(platform.AddressSpaceOptions{})
	if err != nil {
		h.t.Fatalf("NewAddressSpace: %v", err)
	}
	h.t.Cleanup(as.Release)
	return &testMM{as: as}
}

// pages allocates memory-file pages holding data (zero padded to whole pages).
func (h *harness) pages(data []byte, n int) memmap.FileRange {
	h.t.Helper()
	size := uint64(n) * pageSz
	fr, err := h.mf.Allocate(size, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		h.t.Fatalf("Allocate: %v", err)
	}
	bs, err := h.mf.MapInternal(fr, hostarch.ReadWrite)
	if err != nil {
		h.t.Fatalf("MapInternal: %v", err)
	}
	if _, err := safemem.CopySeq(bs, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(data))); err != nil {
		h.t.Fatalf("copy: %v", err)
	}
	return fr
}

func (h *harness) mapAt(mm *testMM, addr uint64, fr memmap.FileRange, at hostarch.AccessType) {
	h.t.Helper()
	if err := mm.as.MapFile(hostarch.Addr(addr), h.mf, fr, at, false); err != nil {
		h.t.Fatalf("MapFile(%#x, %v, %v): %v", addr, fr, at, err)
	}
}

var (
	rx = hostarch.AccessType{Read: true, Execute: true}
	rw = hostarch.ReadWrite
)

type result struct {
	si  *linux.SignalInfo
	at  hostarch.AccessType
	err error
}

func (h *harness) sw(c platform.Context, mm *testMM, ac *arch.Context64) result {
	h.t.Helper()
	ch := make(chan result, 1)
	go func() {
		si, at, err := c.Switch(pkgcontext.Background(), mm, ac, -1)
		ch <- result{si, at, err}
	}()
	select {
	case r := <-ch:
		return r
	case <-time.After(switchMax):
		h.t.Fatalf("Switch did not return")
		return result{}
	}
}

func (h *harness) syscall(c platform.Context, mm *testMM, ac *arch.Context64) uint64 {
	h.t.Helper()
	r := h.sw(c, mm, ac)
	if r.err != nil {
		h.t.Fatalf("Switch: want a syscall, got %v (si %+v)", r.err, r.si)
	}
	return ac.StateData().Regs.Orig_rax
}

func newAC(rip, rsp uint64) *arch.Context64 {
	ac := arch.New(arch.AMD64)
	r := &ac.StateData().Regs
	r.Rip, r.Rsp = rip, rsp
	return ac
}

func args(ac *arch.Context64) [6]uint64 {
	r := &ac.StateData().Regs
	return [6]uint64{r.Rdi, r.Rsi, r.Rdx, r.R10, r.R8, r.R9}
}

// asm builds guest code and remembers label addresses.
type asm struct {
	base uint64
	b    []byte
}

func (a *asm) pc() uint64 { return a.base + uint64(len(a.b)) }
func (a *asm) emit(b ...byte) uint64 {
	p := a.pc()
	a.b = append(a.b, b...)
	return p
}
func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
func le64(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }

// guest is the program the tests drive step by step.
type guest struct {
	code                                    []byte
	start, afterFirst, spin, exit, readLoop uint64
	udSite, udAfter, loop                   uint64
	faultInsn                               uint64
}

func buildGuest() *guest {
	a := &asm{base: textBase}
	g := &guest{}
	g.start = a.pc()
	a.emit(0xbf, 1, 0, 0, 0)       // mov edi,1
	a.emit(0xbe, 2, 0, 0, 0)       // mov esi,2
	a.emit(0xba, 3, 0, 0, 0)       // mov edx,3
	a.emit(0x41, 0xba, 4, 0, 0, 0) // mov r10d,4
	a.emit(0x41, 0xb8, 5, 0, 0, 0) // mov r8d,5
	a.emit(0x41, 0xb9, 6, 0, 0, 0) // mov r9d,6
	a.emit(0xb8, 39, 0, 0, 0)      // mov eax,39
	a.emit(0x0f, 0x05)             // syscall (jmp *slot)
	g.afterFirst = a.pc()
	a.emit(0x48, 0x89, 0xc7)                                             // mov rdi,rax
	a.emit(0xb8, 0xe7, 0, 0, 0)                                          // mov eax,231
	a.emit(0x0f, 0x05)                                                   // syscall
	a.emit(0xbf, 7, 0, 0, 0)                                             // mov edi,7
	a.emit(0x48, 0x89, 0xf8)                                             // mov rax,rdi
	a.emit(0x0f, 0x05)                                                   // syscall (veneer)
	g.faultInsn = a.emit(0x48, 0x8b, 0x04, 0x25, 0x00, 0x00, 0x80, 0x00) // mov rax,[0x800000]
	a.emit(0x48, 0x89, 0xc7)                                             // mov rdi,rax
	a.emit(0xb8, 0xf4, 1, 0, 0)                                          // mov eax,500
	a.emit(0x0f, 0x05)                                                   // syscall
	g.spin = a.emit(0xeb, 0xfe)                                          // jmp .
	g.exit = a.emit(0xb8, 60, 0, 0, 0, 0x0f, 0x05)
	// a syscall that cannot be patched: a branch target between a jmp and a jmp
	a.emit(0xb8, 40, 0, 0, 0) // mov eax,40
	a.emit(0xeb, 0x00)        // jmp +0
	g.udSite = a.emit(0x0f, 0x05)
	g.udAfter = a.emit(0xeb, 0xfe)
	// read loop: report the 8 bytes at [rdi] as the first argument
	g.readLoop = a.emit(0x48, 0x8b, 0x1f) // mov rbx,[rdi]
	a.emit(0x48, 0x89, 0xdf)              // mov rdi,rbx
	a.emit(0xb8, 0xf4, 1, 0, 0)           // mov eax,500
	a.emit(0x0f, 0x05)
	// a tight syscall loop
	g.loop = a.emit(0xb8, 39, 0, 0, 0) // mov eax,39
	a.emit(0x0f, 0x05)
	a.emit(0xeb, 0xf7) // jmp loop
	g.code = a.b
	return g
}

func (h *harness) setup(mm *testMM, g *guest) memmap.FileRange {
	fr := h.pages(g.code, 4)
	h.mapAt(mm, textBase, fr, rx)
	return fr
}

func TestSyscallsFaultsInterrupt(t *testing.T) {
	h := newHarness(t)
	g := buildGuest()
	mm := h.newMM()
	h.setup(mm, g)
	stack := h.pages(nil, 4)
	h.mapAt(mm, 0x700000, stack, rw)
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	ac := newAC(g.start, 0x700000+4*pageSz-64)
	r := &ac.StateData().Regs

	// syscall number and arguments
	if nr := h.syscall(c, mm, ac); nr != 39 {
		t.Fatalf("first syscall = %d, want 39", nr)
	}
	if got, want := args(ac), [6]uint64{1, 2, 3, 4, 5, 6}; got != want {
		t.Fatalf("args = %v, want %v", got, want)
	}
	if r.Rip != g.afterFirst {
		t.Fatalf("rip = %#x, want %#x", r.Rip, g.afterFirst)
	}
	if r.Rcx != r.Rip || r.R11 != eflagsUser {
		t.Errorf("rcx/r11 = %#x/%#x, want rip/%#x", r.Rcx, r.R11, eflagsUser)
	}

	// restart: the Sentry moves rip back by 2 and restores rax
	r.Rip -= 2
	r.Rax = r.Orig_rax
	if nr := h.syscall(c, mm, ac); nr != 39 {
		t.Fatalf("restarted syscall = %d, want 39", nr)
	}
	if got, want := args(ac), [6]uint64{1, 2, 3, 4, 5, 6}; got != want {
		t.Fatalf("restarted args = %v, want %v", got, want)
	}

	// the result reaches the guest
	r.Rax = 100
	if nr := h.syscall(c, mm, ac); nr != 231 || r.Rdi != 100 {
		t.Fatalf("second syscall = %d(%d), want 231(100)", nr, r.Rdi)
	}

	// a syscall from a relocated site
	r.Rax = 0
	if nr := h.syscall(c, mm, ac); nr != 7 {
		t.Fatalf("relocated site syscall = %d, want 7", nr)
	}

	// a fault on an unmapped address
	r.Rax = 0
	res := h.sw(c, mm, ac)
	if res.err != platform.ErrContextSignal || res.si == nil || res.si.Signo != int32(linux.SIGSEGV) {
		t.Fatalf("want SIGSEGV, got %v %+v", res.err, res.si)
	}
	if res.si.Addr() != 0x800000 || !res.at.Read || res.at.Write || res.si.Code != 1 {
		t.Fatalf("fault: addr %#x at %v code %d", res.si.Addr(), res.at, res.si.Code)
	}
	if r.Rip != g.faultInsn {
		t.Fatalf("fault rip = %#x, want %#x", r.Rip, g.faultInsn)
	}

	// map the page and resume at the same instruction
	data := h.pages(le64(0x1122334455667788), 1)
	h.mapAt(mm, 0x800000, data, rw)
	if nr := h.syscall(c, mm, ac); nr != 500 || r.Rdi != 0x1122334455667788 {
		t.Fatalf("after the fault: syscall %d(%#x), want 500(0x1122334455667788)", nr, r.Rdi)
	}

	// interrupt a spinning guest
	r.Rip, r.Rax = g.spin, 0
	go func() {
		time.Sleep(300 * time.Millisecond)
		c.Interrupt()
	}()
	res = h.sw(c, mm, ac)
	if res.err != platform.ErrContextInterrupt {
		t.Fatalf("want an interrupt, got %v", res.err)
	}
	if r.Rip != g.spin {
		t.Fatalf("interrupted at %#x, want %#x", r.Rip, g.spin)
	}
	// an interrupt while not running makes the next Switch return at once
	c.Interrupt()
	if res = h.sw(c, mm, ac); res.err != platform.ErrContextInterrupt {
		t.Fatalf("pending interrupt: got %v", res.err)
	}
	// resume at another address after the interrupt
	r.Rip = g.exit
	if nr := h.syscall(c, mm, ac); nr != 60 {
		t.Fatalf("exit syscall = %d, want 60", nr)
	}

	// an unpatched syscall site goes through the debugger
	r.Rip, r.Rax = g.udSite-7, 0 // mov eax,40; jmp +0; syscall
	if nr := h.syscall(c, mm, ac); nr != 40 {
		t.Fatalf("ud2 site syscall = %d, want 40", nr)
	}
	if r.Rip != g.udAfter {
		t.Fatalf("ud2 site resume at %#x, want %#x", r.Rip, g.udAfter)
	}
	if mm.mmaps != 2 {
		t.Errorf("hole reservations = %d, want 2", mm.mmaps)
	}
}

func TestExecuteFault(t *testing.T) {
	h := newHarness(t)
	g := buildGuest()
	mm := h.newMM()
	h.setup(mm, g)
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	ac := newAC(0x900000, 0)
	res := h.sw(c, mm, ac)
	if res.err != platform.ErrContextSignal || !res.at.Execute || res.si.Addr() != 0x900000 {
		t.Fatalf("want an execute fault at 0x900000, got %v at=%v %+v", res.err, res.at, res.si)
	}
	// text mapped without execute permission faults on fetch, with ACCERR
	h.mapAt(mm, 0x900000, h.pages(g.code, 1), rw)
	res = h.sw(c, mm, ac)
	if res.err != platform.ErrContextSignal || !res.at.Execute || res.si.Code != 2 {
		t.Fatalf("want an execute access error, got %v at=%v %+v", res.err, res.at, res.si)
	}
}

func TestUnmapAndRemap(t *testing.T) {
	h := newHarness(t)
	g := buildGuest()
	mm := h.newMM()
	h.setup(mm, g)
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	ac := newAC(g.readLoop, 0)
	r := &ac.StateData().Regs
	const base, n = 0x1000000, 16
	data := make([]byte, n*pageSz)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint64(data[i*pageSz:], uint64(0xA000+i))
	}
	fr := h.pages(data, n)
	h.mapAt(mm, base, fr, rw)
	read := func(page int) (uint64, bool) {
		r.Rip, r.Rdi, r.Rax = g.readLoop, base+uint64(page)*pageSz, 0
		res := h.sw(c, mm, ac)
		if res.err == platform.ErrContextSignal {
			if res.si.Addr() != r.Rdi {
				t.Fatalf("fault at %#x, want %#x", res.si.Addr(), r.Rdi)
			}
			return 0, false
		}
		if res.err != nil {
			t.Fatalf("read page %d: %v", page, res.err)
		}
		return r.Rdi, true
	}
	check := func(page int, want uint64, mapped bool) {
		t.Helper()
		v, ok := read(page)
		if ok != mapped || (ok && v != want) {
			t.Fatalf("page %d: got %#x mapped=%v, want %#x mapped=%v", page, v, ok, want, mapped)
		}
	}
	for i := 0; i < n; i++ {
		check(i, uint64(0xA000+i), true)
	}
	mm.as.Unmap(base+5*pageSz, pageSz)
	check(4, 0xA004, true)
	check(5, 0, false)
	check(6, 0xA006, true)
	mm.as.Unmap(base+8*pageSz, 3*pageSz)
	check(7, 0xA007, true)
	check(8, 0, false)
	check(10, 0, false)
	check(11, 0xA00B, true)
	// map the hole again (different memory), over a neighbour as well
	other := h.pages(le64(0xBEEF), 2)
	h.mapAt(mm, base+4*pageSz, other, rw) // replaces page 4, fills page 5
	check(4, 0xBEEF, true)
	check(5, 0, true) // the second page of `other` is zero
	check(3, 0xA003, true)
	check(6, 0xA006, true)
	// a mapping that spans several existing views and holes
	mm.as.Unmap(base, n*pageSz)
	check(0, 0, false)
	check(15, 0, false)
	h.mapAt(mm, base+2*pageSz, fr, rw)
	check(2, 0xA000, true)
	check(17, 0xA00F, true)
}

func TestNewThreadsAndSpeed(t *testing.T) {
	h := newHarness(t)
	g := buildGuest()
	mm := h.newMM()
	h.setup(mm, g)
	const workers, iters = 4, 3000
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			c := h.p.NewContext(pkgcontext.Background())
			defer c.Release()
			ac := newAC(g.loop, 0)
			start := time.Now()
			for i := 0; i < iters; i++ {
				si, _, err := c.Switch(pkgcontext.Background(), mm, ac, -1)
				if err != nil || si != nil {
					errs <- errors.New("switch failed")
					return
				}
				if nr := ac.StateData().Regs.Orig_rax; nr != 39 {
					errs <- errors.New("wrong syscall")
					return
				}
				ac.StateData().Regs.Rax = uint64(i)
			}
			if w == 0 {
				t.Logf("%d concurrent threads: %v per syscall round trip", workers, time.Since(start)/iters)
			}
		}(w)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(switchMax):
		t.Fatal("workers did not finish")
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestForkedAddressSpace(t *testing.T) {
	h := newHarness(t)
	g := buildGuest()
	mm1 := h.newMM()
	fr := h.setup(mm1, g)
	c1 := h.p.NewContext(pkgcontext.Background())
	defer c1.Release()
	ac1 := newAC(g.start, 0)
	if nr := h.syscall(c1, mm1, ac1); nr != 39 {
		t.Fatalf("parent syscall %d", nr)
	}

	mm1.as.PreFork()
	mm2 := h.newMM()
	mm1.as.PostFork()
	h.mapAt(mm2, textBase, fr, rx)
	c2 := h.p.NewContext(pkgcontext.Background())
	defer c2.Release()
	ac2 := newAC(g.start, 0)
	if nr := h.syscall(c2, mm2, ac2); nr != 39 {
		t.Fatalf("child syscall %d", nr)
	}
	if got, want := args(ac2), [6]uint64{1, 2, 3, 4, 5, 6}; got != want {
		t.Fatalf("child args = %v", got)
	}
	// the parent keeps running
	ac1.StateData().Regs.Rax = 5
	if nr := h.syscall(c1, mm1, ac1); nr != 231 {
		t.Fatalf("parent second syscall %d", nr)
	}
	// execve: the context moves to another address space
	mm3 := h.newMM()
	h.mapAt(mm3, textBase, fr, rx)
	c1.PrepareExecve()
	ac3 := newAC(g.start, 0)
	if nr := h.syscall(c1, mm3, ac3); nr != 39 {
		t.Fatalf("after execve syscall %d", nr)
	}
}

func TestWriteExecute(t *testing.T) {
	h := newHarness(t)
	g := buildGuest()
	mm := h.newMM()
	h.setup(mm, g)
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	// a read-write-execute page: mov eax,39; syscall; the guest runs it after the
	// Sentry wrote it (a JIT)
	code := []byte{0xb8, 39, 0, 0, 0, 0x0f, 0x05, 0xeb, 0xfe}
	fr := h.pages(code, 1)
	h.mapAt(mm, 0xA00000, fr, hostarch.AnyAccess)
	ac := newAC(0xA00000, 0)
	if nr := h.syscall(c, mm, ac); nr != 39 {
		t.Fatalf("jit syscall %d", nr)
	}
	if got := ac.StateData().Regs.Rip; got != 0xA00007 {
		t.Fatalf("jit resume at %#x", got)
	}
	// rewrite the code in the shared memory and run it again: it must be rescanned
	bs, _ := h.mf.MapInternal(fr, hostarch.ReadWrite)
	copy(bs.Head().ToSlice(), []byte{0xb8, 110, 0, 0, 0, 0x0f, 0x05, 0xeb, 0xfe})
	ac = newAC(0xA00000, 0)
	// the stub still has the old copy: the platform learns about the write only
	// through a write fault, so remap as the memory manager does after a COW
	h.mapAt(mm, 0xA00000, fr, hostarch.AnyAccess)
	if nr := h.syscall(c, mm, ac); nr != 110 {
		t.Fatalf("rewritten jit syscall %d", nr)
	}
}

func TestMain(m *testing.M) {
	cpuid.Initialize()
	os.Exit(m.Run())
}

// TestFPState checks that vector registers survive syscalls, faults and
// interrupts, and that the Sentry can read and change them.
func TestFPState(t *testing.T) {
	h := newHarness(t)
	mm := h.newMM()
	a := &asm{base: textBase}
	start := a.emit(0xc5, 0xfe, 0x6f, 0x07)                // vmovdqu ymm0,[rdi]
	a.emit(0xb8, 39, 0, 0, 0, 0x0f, 0x05)                  // syscall
	a.emit(0xc5, 0xfe, 0x7f, 0x06)                         // vmovdqu [rsi],ymm0
	a.emit(0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05)                // syscall 500
	a.emit(0x48, 0x8b, 0x04, 0x25, 0x00, 0x00, 0x80, 0x00) // mov rax,[0x800000]
	a.emit(0xc5, 0xfe, 0x7f, 0x46, 0x20)                   // vmovdqu [rsi+32],ymm0
	a.emit(0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05)                // syscall 500
	spin := a.emit(0xeb, 0xfe)
	store := a.emit(0xc5, 0xfe, 0x7f, 0x46, 0x40) // vmovdqu [rsi+64],ymm0
	a.emit(0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05)
	h.mapAt(mm, textBase, h.pages(a.b, 1), rx)
	pat := make([]byte, 32)
	for i := range pat {
		pat[i] = byte(i + 1)
	}
	h.mapAt(mm, 0x500000, h.pages(pat, 1), rw)
	outFR := h.pages(nil, 1)
	h.mapAt(mm, 0x510000, outFR, rw)
	out := func() []byte {
		bs, err := h.mf.MapInternal(outFR, hostarch.Read)
		if err != nil {
			t.Fatal(err)
		}
		return append([]byte(nil), bs.Head().ToSlice()[:96]...)
	}
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	ac := newAC(start, 0)
	r := &ac.StateData().Regs
	r.Rdi, r.Rsi = 0x500000, 0x510000

	if nr := h.syscall(c, mm, ac); nr != 39 {
		t.Fatalf("syscall %d", nr)
	}
	// the Sentry changes xmm0's low half
	if err := c.PullFullState(mm.as, ac); err != nil {
		t.Fatal(err)
	}
	fp := ac.FloatingPointData().Slice()
	for i := 0; i < 16; i++ {
		fp[160+i] = 0xAA
	}
	c.FullStateChanged()
	if nr := h.syscall(c, mm, ac); nr != 500 {
		t.Fatalf("syscall %d", nr)
	}
	want := append([]byte(nil), pat...)
	for i := 0; i < 16; i++ {
		want[i] = 0xAA
	}
	if got := out()[:32]; string(got) != string(want) {
		t.Fatalf("after a Sentry modification ymm0 = %x, want %x", got, want)
	}
	// across a fault
	res := h.sw(c, mm, ac)
	if res.err != platform.ErrContextSignal {
		t.Fatalf("want a fault, got %v", res.err)
	}
	h.mapAt(mm, 0x800000, h.pages(nil, 1), rw)
	if nr := h.syscall(c, mm, ac); nr != 500 {
		t.Fatalf("syscall %d", nr)
	}
	if got := out()[32:64]; string(got) != string(want) {
		t.Fatalf("after a fault ymm0 = %x, want %x", got, want)
	}
	// across an interrupt
	r.Rip = spin
	go func() {
		time.Sleep(300 * time.Millisecond)
		c.Interrupt()
	}()
	if res := h.sw(c, mm, ac); res.err != platform.ErrContextInterrupt {
		t.Fatalf("want an interrupt, got %v", res.err)
	}
	r.Rip = store
	if nr := h.syscall(c, mm, ac); nr != 500 {
		t.Fatalf("syscall %d", nr)
	}
	if got := out()[64:96]; string(got) != string(want) {
		t.Fatalf("after an interrupt ymm0 = %x, want %x", got, want)
	}
}

// TestSyscallLatency logs the syscall round trip per thread count: every thread
// runs its own context in one address space.
func TestSyscallLatency(t *testing.T) {
	h := newHarness(t)
	g := buildGuest()
	mm := h.newMM()
	h.setup(mm, g)
	const iters = 20000
	for _, n := range []int{1, 2, 4, 8} {
		var wg sync.WaitGroup
		durs := make([]time.Duration, n)
		for w := 0; w < n; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				c := h.p.NewContext(pkgcontext.Background())
				defer c.Release()
				ac := newAC(g.loop, 0)
				// warm up: the thread exists and the code is mapped
				if _, _, err := c.Switch(pkgcontext.Background(), mm, ac, -1); err != nil {
					return
				}
				start := time.Now()
				for i := 0; i < iters; i++ {
					if _, _, err := c.Switch(pkgcontext.Background(), mm, ac, -1); err != nil {
						return
					}
				}
				durs[w] = time.Since(start) / iters
			}(w)
		}
		wg.Wait()
		var sum time.Duration
		for _, d := range durs {
			sum += d
		}
		t.Logf("LATENCY threads=%d: %v per syscall round trip per thread (%v each)", n, sum/time.Duration(n), durs)
	}
}

// TestStubStart logs how long a new address space takes (process creation to a
// parked anchor thread) and how long releasing one takes.
func TestStubStart(t *testing.T) {
	h := newHarness(t)
	var starts, rels []time.Duration
	for i := 0; i < 8; i++ {
		t0 := time.Now()
		as, err := h.p.NewAddressSpace(platform.AddressSpaceOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t1 := time.Now()
		as.Release()
		starts = append(starts, t1.Sub(t0))
		rels = append(rels, time.Since(t1))
	}
	t.Logf("STUBSTART new address space: %v; release: %v", starts, rels)
}

// bareStart creates the stub suspended under a debugger, optionally reserves the
// guest range, resumes it and returns the time until the loader breakpoint.
func bareStart(p *Platform, reserve, nodebug bool, holes ...rng) (time.Duration, error) {
	type res struct {
		d   time.Duration
		err error
	}
	ch := make(chan res, 1)
	go func() {
		runtime.LockOSThread()
		cl, _ := syscall.UTF16PtrFromString(`"` + p.stubPath + `"`)
		var si syscall.StartupInfo
		si.Cb = uint32(unsafe.Sizeof(si))
		var pi syscall.ProcessInformation
		flags := uint32(0x4 | 0x08000000)
		if !nodebug {
			flags |= 0x2
		}
		t0 := time.Now()
		if err := syscall.CreateProcess(nil, cl, nil, nil, false, flags, nil, nil, &si, &pi); err != nil {
			ch <- res{0, err}
			return
		}
		defer pTerminateProcess.Call(uintptr(pi.Process), 0)
		if reserve {
			as := newAddressSpace(p)
			as.h = uintptr(pi.Process)
			used := queryUsed(as.h, stubasm.ReserveMin, stubasm.ReserveMax)
			used = append(used, holes...)
			sort.Slice(used, func(i, j int) bool { return used[i].lo < used[j].lo })
			if err := as.reserve(used); err != nil {
				ch <- res{0, err}
				return
			}
		}
		pResumeThread.Call(uintptr(pi.Thread))
		if nodebug {
			windows.WaitForSingleObject(windows.Handle(pi.Process), 400)
			ch <- res{time.Since(t0), nil}
			return
		}
		var ev debugEvent
		for {
			if r, _, _ := pWaitDebugEvent.Call(ev.base(), 5000); r == 0 {
				ch <- res{0, errors.New("no event")}
				return
			}
			st := uint32(dbgContinue)
			if ev.code() == 1 && ev.u32(16) == excBreakpoint {
				ch <- res{time.Since(t0), nil}
				return
			}
			pContinueDebug.Call(uintptr(ev.pid()), uintptr(ev.tid()), uintptr(st))
		}
	}()
	r := <-ch
	return r.d, r.err
}

func TestStubStartExperiment(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 1; i++ {
		for _, c := range []struct {
			name  string
			holes []rng
		}{
			{"no hole", nil},
			{"window", []rng{{stubasm.WinWindow, stubasm.WinWindow + stubasm.WinWindowSize}}},
		} {
			d, err := bareStart(h.p, true, false, c.holes...)
			t.Logf("EXPERIMENT %s: loader breakpoint after %v (%v)", c.name, d, err)
		}
	}
}

// TestDecodeAllMatchesSerial checks the chunked parallel sweep against the serial
// one on real code (this test binary), and times both.
func TestDecodeAllMatchesSerial(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Skip(err)
	}
	if len(b) > 6<<20 {
		b = b[:6<<20]
	}
	t0 := time.Now()
	serial := decodeRange(b, 0, len(b), nil)
	t1 := time.Now()
	par := decodeAll(b)
	t2 := time.Now()
	if len(serial) != len(par) {
		t.Fatalf("instruction counts differ: serial %d, parallel %d", len(serial), len(par))
	}
	for i := range serial {
		if serial[i] != par[i] {
			t.Fatalf("instruction %d differs: %+v vs %+v", i, serial[i], par[i])
		}
	}
	t.Logf("DECODE %d bytes, %d instructions: serial %v, parallel %v", len(b), len(serial), t1.Sub(t0), t2.Sub(t1))
}

// TestSplitSyscall: a syscall instruction whose 0x0f and 0x05 lie in two
// separately mapped ranges must not reach the NT kernel, whichever range is
// mapped first; a 0x0f that ends another instruction must not change the next
// range.
func TestSplitSyscall(t *testing.T) {
	pad := func(head []byte, tail []byte) []byte {
		b := make([]byte, pageSz)
		for i := range b {
			b[i] = 0x90
		}
		copy(b, head)
		copy(b[pageSz-len(tail):], tail)
		return b
	}
	pageA := pad([]byte{0xb8, 40, 0, 0, 0}, []byte{0x0f})
	pageB := pad([]byte{0x05, 0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05}, nil)
	for _, order := range []string{"low first", "high first"} {
		t.Run(order, func(t *testing.T) {
			h := newHarness(t)
			mm := h.newMM()
			a, b := h.pages(pageA, 1), h.pages(pageB, 1)
			if order == "low first" {
				h.mapAt(mm, textBase, a, rx)
				h.mapAt(mm, textBase+pageSz, b, rx)
			} else {
				h.mapAt(mm, textBase+pageSz, b, rx)
				h.mapAt(mm, textBase, a, rx)
			}
			c := h.p.NewContext(pkgcontext.Background())
			defer c.Release()
			ac := newAC(textBase, 0)
			if nr := h.syscall(c, mm, ac); nr != 40 {
				t.Fatalf("split syscall = %d, want 40", nr)
			}
			if got := ac.StateData().Regs.Rip; got != textBase+pageSz+1 {
				t.Fatalf("resume at %#x, want %#x", got, textBase+pageSz+1)
			}
			if nr := h.syscall(c, mm, ac); nr != 500 {
				t.Fatalf("next syscall = %d, want 500", nr)
			}
		})
	}
	t.Run("not a boundary", func(t *testing.T) {
		h := newHarness(t)
		mm := h.newMM()
		// A ends with mov eax,0x0f0f0f0f (its last byte is 0x0f but not an opcode);
		// B starts with add eax,1 (05 01 00 00 00).
		a := h.pages(pad(nil, []byte{0xb8, 0x0f, 0x0f, 0x0f, 0x0f}), 1)
		b := h.pages(pad([]byte{0x05, 1, 0, 0, 0, 0x89, 0xc7, 0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05}, nil), 1)
		h.mapAt(mm, textBase, a, rx)
		h.mapAt(mm, textBase+pageSz, b, rx)
		c := h.p.NewContext(pkgcontext.Background())
		defer c.Release()
		ac := newAC(textBase+pageSz-5-16, 0)
		ac.StateData().Regs.Rip = textBase + pageSz - 5 // the mov
		if nr := h.syscall(c, mm, ac); nr != 500 {
			t.Fatalf("syscall = %d, want 500", nr)
		}
		if got := ac.StateData().Regs.Rdi; got != 0x0f0f0f10 {
			t.Fatalf("rdi = %#x, want 0x0f0f0f10 (the add must run)", got)
		}
	})
}

// TestInterruptStorm interrupts threads that make syscalls in a tight loop, so
// that some interrupts land inside the trampoline, a trap-table entry or a
// veneer, and checks that no register is lost: rbx counts the loop iterations.
func TestInterruptStorm(t *testing.T) {
	h := newHarness(t)
	mm := h.newMM()
	a := &asm{base: textBase}
	imm := a.emit(0x48, 0xff, 0xc3)       // inc rbx
	a.emit(0xb8, 39, 0, 0, 0, 0x0f, 0x05) // mov eax,39; syscall
	a.emit(0xeb, 0xf4)                    // jmp imm
	rel := a.emit(0x48, 0xff, 0xc3)       // inc rbx
	a.emit(0xbf, 39, 0, 0, 0)             // mov edi,39
	a.emit(0x48, 0x89, 0xf8)              // mov rax,rdi
	a.emit(0x0f, 0x05)                    // syscall (veneer)
	a.emit(0xeb, 0xf1)                    // jmp rel
	h.mapAt(mm, textBase, h.pages(a.b, 1), rx)
	const dur = 4 * time.Second
	type stats struct{ syscalls, interrupts, low int }
	run := func(start uint64) (stats, error) {
		c := h.p.NewContext(pkgcontext.Background())
		defer c.Release()
		ac := newAC(start, 0)
		r := &ac.StateData().Regs
		var st stats
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { // the interrupter
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				c.Interrupt()
				for t0 := time.Now(); time.Since(t0) < 30*time.Microsecond; {
				}
			}
		}()
		defer func() { close(stop); wg.Wait() }()
		deadline := time.Now().Add(dur)
		for time.Now().Before(deadline) {
			_, _, err := c.Switch(pkgcontext.Background(), mm, ac, -1)
			switch {
			case err == nil:
				st.syscalls++
				if r.Orig_rax != 39 || r.Rbx != uint64(st.syscalls) {
					return st, fmt.Errorf("syscall %d: nr %d rbx %d", st.syscalls, r.Orig_rax, r.Rbx)
				}
				r.Rax = 0
			case err == platform.ErrContextInterrupt:
				st.interrupts++
				if r.Rip < textBase {
					st.low++
				}
				if r.Rbx != uint64(st.syscalls) && r.Rbx != uint64(st.syscalls)+1 {
					return st, fmt.Errorf("interrupt at %#x after %d syscalls: rbx %d", r.Rip, st.syscalls, r.Rbx)
				}
			default:
				return st, fmt.Errorf("Switch: %v", err)
			}
		}
		return st, nil
	}
	var wg sync.WaitGroup
	for _, s := range []struct {
		name  string
		start uint64
	}{{"trap table", imm}, {"veneer", rel}} {
		wg.Add(1)
		go func(name string, start uint64) {
			defer wg.Done()
			st, err := run(start)
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			t.Logf("STORM %s: %d syscalls, %d interrupts (%d outside the guest text)", name, st.syscalls, st.interrupts, st.low)
		}(s.name, s.start)
	}
	wg.Wait()
}

// bytesFile is a memmap.File that is not a memory file: its pages are a Go slice
// (like the erofs image mapping), with no section to map.
type bytesFile struct {
	memmap.DefaultMemoryType
	memmap.NoBufferedIOFallback
	data []byte
}

func (*bytesFile) IncRef(memmap.FileRange, uint32) {}
func (*bytesFile) DecRef(memmap.FileRange)         {}
func (b *bytesFile) MapInternal(fr memmap.FileRange, at hostarch.AccessType) (safemem.BlockSeq, error) {
	return safemem.BlockSeqOf(safemem.BlockFromSafeSlice(b.data[fr.Start:fr.End])), nil
}
func (*bytesFile) DataFD(memmap.FileRange) (int, error) { return -1, errors.New("no fd") }
func (*bytesFile) FD() int                              { return -1 }

// TestMapNonSectionFile maps text and data from a file that is not the memory
// file and runs the guest.
func TestMapNonSectionFile(t *testing.T) {
	h := newHarness(t)
	g := buildGuest()
	mm := h.newMM()
	text := &bytesFile{data: append(append([]byte(nil), g.code...), make([]byte, 4*pageSz-len(g.code))...)}
	if err := mm.as.MapFile(textBase, text, memmap.FileRange{Start: 0, End: 4 * pageSz}, rx, false); err != nil {
		t.Fatalf("MapFile of text: %v", err)
	}
	data := &bytesFile{data: make([]byte, pageSz)}
	binary.LittleEndian.PutUint64(data.data, 0xfeedface12345678)
	if err := mm.as.MapFile(0x1000000, data, memmap.FileRange{Start: 0, End: pageSz}, hostarch.Read, false); err != nil {
		t.Fatalf("MapFile of data: %v", err)
	}
	if err := mm.as.MapFile(0x1100000, data, memmap.FileRange{Start: 0, End: pageSz}, rw, false); err == nil {
		t.Errorf("a writable mapping of a file without sections must fail")
	}
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	ac := newAC(g.readLoop, 0)
	ac.StateData().Regs.Rdi = 0x1000000
	if nr := h.syscall(c, mm, ac); nr != 500 || ac.StateData().Regs.Rdi != 0xfeedface12345678 {
		t.Fatalf("syscall %d rdi %#x", nr, ac.StateData().Regs.Rdi)
	}
}

type perfInfo struct {
	cb                                                                                                  uint32
	_                                                                                                   uint32
	commitTotal, commitLimit, commitPeak, physTotal, physAvail, sysCache, kTotal, kPaged, kNP, pageSize uintptr
	handles, procs, threads                                                                             uint32
}

type procMem struct {
	cb, faults                      uint32
	peakWS, ws                      uintptr
	qpp, qp, qnpp, qnp              uintptr
	pagefile, peakPagefile, private uintptr
}

// memState returns the system commit charge and this process's working set, in MB.
func memState() (commitMB, wsMB uint64) {
	var pi perfInfo
	pi.cb = uint32(unsafe.Sizeof(pi))
	k := windows.NewLazySystemDLL("kernel32.dll")
	k.NewProc("K32GetPerformanceInfo").Call(uintptr(unsafe.Pointer(&pi)), uintptr(pi.cb))
	var pm procMem
	pm.cb = uint32(unsafe.Sizeof(pm))
	k.NewProc("K32GetProcessMemoryInfo").Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&pm)), uintptr(pm.cb))
	return uint64(pi.commitTotal) * uint64(pi.pageSize) >> 20, uint64(pm.ws) >> 20
}

// TestMemoryRelease allocates and touches 1 GB of guest memory, maps it into a
// stub, unmaps and frees it, and checks that the working set and the system
// commit charge go back down.
func TestMemoryRelease(t *testing.T) {
	h := newHarness(t)
	mm := h.newMM()
	const size = 1 << 30
	c0, w0 := memState()
	t0 := time.Now()
	fr, err := h.mf.Allocate(size, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	fillRange(t, h.mf, fr, 0x5a)
	h.mapAt(mm, 0x10000000, fr, rw)
	c1, w1 := memState()
	t.Logf("RELEASE allocated, touched and mapped 1 GB in %v: commit %d -> %d MB, working set %d -> %d MB", time.Since(t0).Round(time.Millisecond), c0, c1, w0, w1)
	if c1 < c0+900 || w1 < w0+900 {
		t.Fatalf("the allocation did not show up: commit %d -> %d, working set %d -> %d", c0, c1, w0, w1)
	}
	mm.as.Unmap(0x10000000, size)
	h.mf.DecRef(fr)
	t1 := time.Now()
	var c2, w2 uint64
	for time.Since(t1) < 30*time.Second {
		c2, w2 = memState()
		if c2 < c0+100 && w2 < w0+100 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("RELEASE freed after %v: commit %d MB (baseline %d), working set %d MB (baseline %d); extents released %d", time.Since(t1).Round(time.Millisecond), c2, c0, w2, w0, pgalloc.ExtentsReleased())
	if c2 > c0+100 {
		t.Errorf("commit charge did not come back: %d MB, baseline %d MB", c2, c0)
	}
	if w2 > w0+100 {
		t.Errorf("working set did not come back: %d MB, baseline %d MB", w2, w0)
	}
	// the memory is usable again, reads as zero
	fr2, err := h.mf.Allocate(1<<20, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatal(err)
	}
	checkRange(t, h.mf, fr2, 0)
}

func fillRange(t *testing.T, f *pgalloc.MemoryFile, fr memmap.FileRange, b byte) {
	t.Helper()
	bs, err := f.MapInternal(fr, hostarch.ReadWrite)
	if err != nil {
		t.Fatalf("MapInternal(%v): %v", fr, err)
	}
	for ; !bs.IsEmpty(); bs = bs.Tail() {
		s := bs.Head().ToSlice()
		for i := range s {
			s[i] = b
		}
	}
}

func checkRange(t *testing.T, f *pgalloc.MemoryFile, fr memmap.FileRange, b byte) {
	t.Helper()
	bs, err := f.MapInternal(fr, hostarch.Read)
	if err != nil {
		t.Fatalf("MapInternal(%v): %v", fr, err)
	}
	for ; !bs.IsEmpty(); bs = bs.Tail() {
		for i, v := range bs.Head().ToSlice() {
			if v != b {
				t.Fatalf("range %v: byte %d is %#x, want %#x", fr, i, v, b)
			}
		}
	}
}

// splitPage returns a page of NOPs with head at its start and tail at its end.
func splitPage(head, tail []byte) []byte {
	b := bytes.Repeat([]byte{0x90}, pageSz)
	copy(b, head)
	copy(b[pageSz-len(tail):], tail)
	return b
}

// TestSplitSyscallAfterUnmap: the ends a partial unmap leaves behind take part in
// the split-syscall handling.
func TestSplitSyscallAfterUnmap(t *testing.T) {
	later := []byte{0x05, 0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05}
	t.Run("left remainder", func(t *testing.T) {
		h := newHarness(t)
		mm := h.newMM()
		// One mapping of two pages; the first ends with 0f 1f 00 (a nop) across
		// the page boundary.
		a := splitPage([]byte{0xb8, 40, 0, 0, 0}, []byte{0x0f})
		b := splitPage([]byte{0x1f, 0x00}, nil)
		h.mapAt(mm, textBase, h.pages(append(a, b...), 2), rx)
		mm.as.Unmap(textBase+pageSz, pageSz)
		h.mapAt(mm, textBase+pageSz, h.pages(splitPage(later, nil), 1), rx)
		c := h.p.NewContext(pkgcontext.Background())
		defer c.Release()
		ac := newAC(textBase, 0)
		if nr := h.syscall(c, mm, ac); nr != 40 {
			t.Fatalf("split syscall = %d, want 40", nr)
		}
		if nr := h.syscall(c, mm, ac); nr != 500 {
			t.Fatalf("next syscall = %d, want 500", nr)
		}
	})
	t.Run("right remainder", func(t *testing.T) {
		h := newHarness(t)
		mm := h.newMM()
		// The second page starts with 05 (add eax,imm32 as the range was mapped).
		a := splitPage(nil, nil)
		b := splitPage(later, nil)
		h.mapAt(mm, textBase, h.pages(append(a, b...), 2), rx)
		mm.as.Unmap(textBase, pageSz)
		h.mapAt(mm, textBase, h.pages(splitPage([]byte{0xb8, 41, 0, 0, 0}, []byte{0x0f}), 1), rx)
		c := h.p.NewContext(pkgcontext.Background())
		defer c.Release()
		ac := newAC(textBase, 0)
		if nr := h.syscall(c, mm, ac); nr != 41 {
			t.Fatalf("split syscall = %d, want 41", nr)
		}
		if nr := h.syscall(c, mm, ac); nr != 500 {
			t.Fatalf("next syscall = %d, want 500", nr)
		}
	})
}

// TestSplitSyscallWX: a syscall split by the boundary of two W^X windows.
func TestSplitSyscallWX(t *testing.T) {
	h := newHarness(t)
	mm := h.newMM()
	data := bytes.Repeat([]byte{0x90}, 2<<20)
	copy(data, []byte{0xb8, 40, 0, 0, 0})
	data[wxWindow-1] = 0x0f
	copy(data[wxWindow:], []byte{0x05, 0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05})
	h.mapAt(mm, 0xA00000, h.pages(data, 512), hostarch.AnyAccess)
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	ac := newAC(0xA00000, 0)
	if nr := h.syscall(c, mm, ac); nr != 40 {
		t.Fatalf("split syscall = %d, want 40", nr)
	}
	if got := ac.StateData().Regs.Rip; got != 0xA00000+wxWindow+1 {
		t.Fatalf("resume at %#x", got)
	}
	if nr := h.syscall(c, mm, ac); nr != 500 {
		t.Fatalf("next syscall = %d, want 500", nr)
	}
}

// TestOversubscribed runs many more busy guest threads than there are vCPUs, all
// making syscalls in a tight loop, and checks that none starves and that the
// round trip of one more thread stays bounded.
func TestOversubscribed(t *testing.T) {
	h := newHarness(t)
	g := buildGuest()
	mm := h.newMM()
	h.setup(mm, g)
	threads := 8 * runtime.NumCPU()
	const dur = 4 * time.Second
	counts := make([]int, threads)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < threads; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			c := h.p.NewContext(pkgcontext.Background())
			defer c.Release()
			ac := newAC(g.loop, 0)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, _, err := c.Switch(pkgcontext.Background(), mm, ac, -1); err != nil {
					return
				}
				counts[w]++
			}
		}(w)
	}
	// the probe: one more thread, its round trips timed one by one
	var lat []time.Duration
	probe := h.p.NewContext(pkgcontext.Background())
	pac := newAC(g.loop, 0)
	end := time.Now().Add(dur)
	for time.Now().Before(end) {
		t0 := qpc()
		if _, _, err := probe.Switch(pkgcontext.Background(), mm, pac, -1); err != nil {
			t.Fatalf("probe: %v", err)
		}
		lat = append(lat, time.Duration(qpc()-t0))
	}
	close(stop)
	wg.Wait()
	probe.Release()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	total, lo, hi := 0, counts[0], counts[0]
	for _, c := range counts {
		total += c
		lo, hi = min(lo, c), max(hi, c)
	}
	n := len(lat)
	t.Logf("OVERSUB %d busy threads on %d vCPUs: %d syscalls in %v (%.0f per second); per thread min %d, mean %d, max %d; probe round trip p50 %v p99 %v p99.9 %v max %v over %d",
		threads, runtime.NumCPU(), total, dur, float64(total)/dur.Seconds(), lo, total/threads, hi, lat[n/2], lat[n*99/100], lat[n*999/1000], lat[n-1], n)
	if lo < total/threads/20 {
		t.Errorf("a thread starved: %d syscalls against a mean of %d", lo, total/threads)
	}
	if lat[n*99/100] > 50*time.Millisecond {
		t.Errorf("probe p99 %v", lat[n*99/100])
	}
}

// sectionFile is a file whose pages are a read-only file mapping, like an erofs
// image.
type sectionFile struct {
	bytesFile
	h windows.Handle
}

func (s *sectionFile) ReadOnlySection() windows.Handle { return s.h }

func availPhysMB() uint64 {
	var m struct {
		length               uint32
		memoryLoad           uint32
		totalPhys, availPhys uint64
		totalPF, availPF     uint64
		totalVirt, availVirt uint64
		availExt             uint64
	}
	m.length = uint32(unsafe.Sizeof(m))
	windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx").Call(uintptr(unsafe.Pointer(&m)))
	return m.availPhys >> 20
}

// TestShareReadOnlySection maps a read-only image section into 20 address spaces
// whose guests read all of it, with and without sharing, and compares the memory.
func TestShareReadOnlySection(t *testing.T) {
	const imgSize, procs = 16 << 20, 20
	f, err := os.CreateTemp("", "winstub-image-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	img := make([]byte, imgSize)
	var want uint64
	for p := 0; p < imgSize/pageSz; p++ {
		binary.LittleEndian.PutUint64(img[p*pageSz:], uint64(p)+1)
		want += uint64(p) + 1
	}
	if _, err := f.Write(img); err != nil {
		t.Fatal(err)
	}
	sec, err := windows.CreateFileMapping(windows.Handle(f.Fd()), nil, windows.PAGE_READONLY, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(sec)
	view, err := windows.MapViewOfFile(sec, windows.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.UnmapViewOfFile(view)
	file := &sectionFile{bytesFile{data: unsafe.Slice((*byte)(unsafe.Pointer(view)), imgSize)}, sec}

	a := &asm{base: textBase}
	touch := a.emit(0x48, 0x8b, 0x07) // L: mov rax,[rdi]
	a.emit(0x48, 0x01, 0xc3)          // add rbx,rax
	a.emit(0x48, 0x81, 0xc7, 0x00, 0x10, 0, 0) // add rdi,4096
	a.emit(0x48, 0xff, 0xc9)          // dec rcx
	a.emit(0x75, 0xee)                // jnz L
	a.emit(0x48, 0x89, 0xdf)          // mov rdi,rbx
	a.emit(0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05)

	h := newHarness(t)
	for _, share := range []bool{true, false} {
		shareReadOnly = share
		runtime.GC()
		c0, _ := memState()
		p0 := availPhysMB()
		var mapT, runT time.Duration
		var mms []*testMM
		for i := 0; i < procs; i++ {
			mm := h.newMM()
			mms = append(mms, mm)
			h.mapAt(mm, textBase, h.pages(a.b, 1), rx)
			t0 := time.Now()
			if err := mm.as.MapFile(0x4000000, file, memmap.FileRange{Start: 0, End: imgSize}, hostarch.Read, false); err != nil {
				t.Fatalf("MapFile: %v", err)
			}
			mapT += time.Since(t0)
			c := h.p.NewContext(pkgcontext.Background())
			ac := newAC(touch, 0)
			ac.StateData().Regs.Rdi, ac.StateData().Regs.Rcx = 0x4000000, imgSize/pageSz
			t1 := time.Now()
			if nr := h.syscall(c, mm, ac); nr != 500 || ac.StateData().Regs.Rdi != want {
				t.Fatalf("share=%v: syscall %d, sum %d, want %d", share, nr, ac.StateData().Regs.Rdi, want)
			}
			runT += time.Since(t1)
			c.Release()
		}
		c1, _ := memState()
		p1 := availPhysMB()
		t.Logf("SHARE share=%v: %d address spaces each mapping and reading %d MiB: MapFile %v each, read %v each; system commit +%d MB, available physical memory -%d MB",
			share, procs, imgSize>>20, (mapT / procs).Round(time.Microsecond), (runT / procs).Round(time.Microsecond), int64(c1)-int64(c0), int64(p0)-int64(p1))
		for _, mm := range mms {
			mm.as.Release()
		}
	}
	shareReadOnly = true
}

// TestPolicyMatrix compares the spin policies at several thread counts: `threads`
// busy contexts in a syscall loop plus one probe context whose round trips are
// timed one by one.
func TestPolicyMatrix(t *testing.T) {
	if os.Getenv("WINSTUB_MATRIX") == "" {
		t.Skip("set WINSTUB_MATRIX=1")
	}
	h := newHarness(t)
	g := buildGuest()
	mm := h.newMM()
	h.setup(mm, g)
	defer func(p int) { spinPolicy = p }(spinPolicy)
	names := map[int]string{policyAdaptive: "adaptive", policyLimit: "limit", policyAlways: "always", policyTokens: "tokens"}
	for _, pol := range []int{policyAlways, policyLimit, policyAdaptive, policyTokens} {
		spinPolicy = pol
		for _, threads := range []int{1, 2, 4, 8, 16, 32} {
			const dur = 1500 * time.Millisecond
			counts := make([]int, threads)
			stop := make(chan struct{})
			var wg sync.WaitGroup
			for w := 0; w < threads; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					c := h.p.NewContext(pkgcontext.Background())
					defer c.Release()
					ac := newAC(g.loop, 0)
					for {
						select {
						case <-stop:
							return
						default:
						}
						if _, _, err := c.Switch(pkgcontext.Background(), mm, ac, -1); err != nil {
							return
						}
						counts[w]++
					}
				}(w)
			}
			var lat []time.Duration
			probe := h.p.NewContext(pkgcontext.Background())
			pac := newAC(g.loop, 0)
			end := time.Now().Add(dur)
			for time.Now().Before(end) {
				t0 := qpc()
				if _, _, err := probe.Switch(pkgcontext.Background(), mm, pac, -1); err != nil {
					t.Fatalf("probe: %v", err)
				}
				lat = append(lat, time.Duration(qpc()-t0))
			}
			close(stop)
			wg.Wait()
			probe.Release()
			sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
			total, lo, hi := len(lat), len(lat), len(lat)
			if threads > 0 {
				total, lo, hi = 0, counts[0], counts[0]
			}
			for _, c := range counts {
				total += c
				lo, hi = min(lo, c), max(hi, c)
			}
			n := len(lat)
			t.Logf("MATRIX %-8s threads=%2d: %7.0f k syscalls/s; per thread min/mean/max %d/%d/%d; probe p50 %v p99 %v p99.9 %v max %v",
				names[pol], threads, float64(total+n)/dur.Seconds()/1000, lo, total/threads, hi, lat[n/2], lat[n*99/100], lat[n*999/1000], lat[n-1])
		}
	}
}

// TestManyThreads keeps 600 guest threads alive in one address space.
func TestManyThreads(t *testing.T) {
	h := newHarness(t)
	g := buildGuest()
	mm := h.newMM()
	h.setup(mm, g)
	const n = 600
	ctxs := make([]platform.Context, n)
	acs := make([]*arch.Context64, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		ctxs[i] = h.p.NewContext(pkgcontext.Background())
		acs[i] = newAC(g.loop, 0)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for r := 0; r < 3; r++ {
				if _, _, err := ctxs[i].Switch(pkgcontext.Background(), mm, acs[i], -1); err != nil {
					errs <- fmt.Errorf("thread %d round %d: %v", i, r, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, c := range ctxs {
		c.Release()
	}
	// the slots come back
	c := h.p.NewContext(pkgcontext.Background())
	defer c.Release()
	if nr := h.syscall(c, mm, newAC(g.loop, 0)); nr != 39 {
		t.Fatalf("syscall %d", nr)
	}
}
