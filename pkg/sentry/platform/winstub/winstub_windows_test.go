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
	"encoding/binary"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

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
	if mm.mmaps != 1 {
		t.Errorf("hole reservations = %d, want 1", mm.mmaps)
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
	start := a.emit(0xc5, 0xfe, 0x6f, 0x07) // vmovdqu ymm0,[rdi]
	a.emit(0xb8, 39, 0, 0, 0, 0x0f, 0x05)   // syscall
	a.emit(0xc5, 0xfe, 0x7f, 0x06)          // vmovdqu [rsi],ymm0
	a.emit(0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05) // syscall 500
	a.emit(0x48, 0x8b, 0x04, 0x25, 0x00, 0x00, 0x80, 0x00) // mov rax,[0x800000]
	a.emit(0xc5, 0xfe, 0x7f, 0x46, 0x20)    // vmovdqu [rsi+32],ymm0
	a.emit(0xb8, 0xf4, 1, 0, 0, 0x0f, 0x05) // syscall 500
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
