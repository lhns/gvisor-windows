package main

import (
	"bytes"
	"crypto/rand"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"sort"

	"stubproto/internal/x86asm"
	"stubproto/stubasm"
)

const (
	stubGuestMin = stubasm.GuestMin
	stubGuestMax = stubasm.GuestMax

	pieBase    = 0x5555_5555_4000
	stackTop   = 0x6F00_0000_0000
	stackSize  = 8 << 20
	mmapTop    = 0x6E00_0000_0000
	brkMaxSize = 1 << 30
)

type loadedELF struct {
	entry, phdr, phnum, phent uint64
	brk                       uint64
	exec                      []rng
}

// loadELF maps a static x86-64 ELF (ET_EXEC or static-PIE) into as, copying file
// content into private memory-file pages.
func loadELF(as *AS, path string) (*loadedELF, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if f.Machine != elf.EM_X86_64 || f.Class != elf.ELFCLASS64 {
		return nil, fmt.Errorf("%s: not an x86-64 ELF", path)
	}
	var bias uint64
	switch f.Type {
	case elf.ET_EXEC:
	case elf.ET_DYN:
		bias = pieBase
	default:
		return nil, fmt.Errorf("%s: unsupported ELF type %v", path, f.Type)
	}
	pageProt := map[uint64]int{}
	var maxEnd uint64
	var phdrAddr uint64
	havePhdr := false
	for _, p := range f.Progs {
		switch p.Type {
		case elf.PT_INTERP:
			return nil, fmt.Errorf("%s: dynamically linked (PT_INTERP); only static binaries are supported", path)
		case elf.PT_PHDR:
			phdrAddr, havePhdr = p.Vaddr+bias, true
		case elf.PT_LOAD:
			pr := 0
			if p.Flags&elf.PF_R != 0 {
				pr |= 1
			}
			if p.Flags&elf.PF_W != 0 {
				pr |= 2
			}
			if p.Flags&elf.PF_X != 0 {
				pr |= 4
			}
			lo, hi := pageDown(p.Vaddr+bias), pageUp(p.Vaddr+bias+p.Memsz)
			for a := lo; a < hi; a += pageSize {
				pageProt[a] |= pr
			}
			if hi > maxEnd {
				maxEnd = hi
			}
		}
	}
	if len(pageProt) == 0 {
		return nil, fmt.Errorf("%s: no PT_LOAD", path)
	}
	addrs := make([]uint64, 0, len(pageProt))
	for a := range pageProt {
		addrs = append(addrs, a)
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i] < addrs[j] })
	ld := &loadedELF{entry: f.Entry + bias, phnum: uint64(len(f.Progs)), phent: 56, brk: maxEnd}
	for i := 0; i < len(addrs); {
		j := i
		for j+1 < len(addrs) && addrs[j+1] == addrs[j]+pageSize && pageProt[addrs[j+1]] == pageProt[addrs[i]] {
			j++
		}
		lo, hi, pr := addrs[i], addrs[j]+pageSize, pageProt[addrs[i]]
		if err := as.addMapped(lo, hi, pr, "elf"); err != nil {
			return nil, err
		}
		if pr&4 != 0 {
			ld.exec = append(ld.exec, rng{lo, hi})
		}
		i = j + 1
	}
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || p.Filesz == 0 {
			continue
		}
		if !as.write(p.Vaddr+bias, data[p.Off:p.Off+p.Filesz]) {
			return nil, fmt.Errorf("%s: cannot write segment at %#x", path, p.Vaddr+bias)
		}
		if !havePhdr && p.Off == 0 {
			phdrAddr = p.Vaddr + bias + f.FileHeader.ByteOrder.Uint64(data[32:])
		}
	}
	ld.phdr = phdrAddr
	return ld, nil
}

// buildStack creates the stack VMA and the Linux initial stack; it returns rsp.
func buildStack(as *AS, ld *loadedELF, args, env []string) (uint64, error) {
	if err := as.addVMA(stackTop-stackSize, stackTop, 3, "stack"); err != nil {
		return 0, err
	}
	p := uint64(stackTop - 64)
	var rnd [16]byte
	rand.Read(rnd[:])
	push := func(b []byte) uint64 {
		p -= uint64(len(b))
		as.write(p, b)
		return p
	}
	str := func(s string) uint64 { return push(append([]byte(s), 0)) }
	execfn := str(args[0])
	var envp, argp []uint64
	for i := len(env) - 1; i >= 0; i-- {
		envp = append([]uint64{str(env[i])}, envp...)
	}
	for i := len(args) - 1; i >= 0; i-- {
		argp = append([]uint64{str(args[i])}, argp...)
	}
	plat := str("x86_64")
	rndp := push(rnd[:])
	aux := [][2]uint64{
		{3, ld.phdr}, {4, ld.phent}, {5, ld.phnum}, {6, pageSize}, {7, 0}, {8, 0}, {9, ld.entry},
		{11, 1000}, {12, 1000}, {13, 1000}, {14, 1000}, {15, plat}, {16, 0}, {17, 100}, {23, 0},
		{25, rndp}, {26, 0}, {31, execfn}, {0, 0},
	}
	words := 1 + len(argp) + 1 + len(envp) + 1 + 2*len(aux)
	sp := (p - uint64(words)*8) &^ 15
	var buf []byte
	w := func(v uint64) { buf = binary.LittleEndian.AppendUint64(buf, v) }
	w(uint64(len(argp)))
	for _, a := range argp {
		w(a)
	}
	w(0)
	for _, a := range envp {
		w(a)
	}
	w(0)
	for _, a := range aux {
		w(a[0])
		w(a[1])
	}
	if !as.write(sp, buf) {
		return 0, fmt.Errorf("cannot write the initial stack")
	}
	return sp, nil
}

// siteStats counts what the scanner found.
type siteStats struct {
	pairs     int // 0f 05 byte pairs in the scanned ranges
	syscalls  int // decoded SYSCALL instructions
	patched   int
	ud2       int
	imm32     int
	imm64     int
	nonSyscal int // pairs inside other instructions or data (pairs - syscalls)
}

func (s *siteStats) add(o siteStats) {
	s.pairs += o.pairs
	s.syscalls += o.syscalls
	s.patched += o.patched
	s.ud2 += o.ud2
	s.imm32 += o.imm32
	s.imm64 += o.imm64
	s.nonSyscal += o.nonSyscal
}

// scanAndPatch rewrites every syscall instruction in [lo,hi) of as, before any of it
// can execute: `mov $imm32,%eax; syscall` and `mov $imm32,%rax; syscall` become a
// 7-byte `jmp *slot` through the trap table, every other site becomes ud2.
func (p *Proc) scanAndPatch(lo, hi uint64) (siteStats, error) {
	var st siteStats
	as := p.as
	code, ok := as.read(lo, int(hi-lo))
	if !ok {
		return st, fmt.Errorf("cannot read %#x-%#x", lo, hi)
	}
	for i := 0; i+1 < len(code); i++ {
		if code[i] == 0x0F && code[i+1] == 0x05 {
			st.pairs++
		}
	}
	var prev x86asm.Inst
	prevOff := -1
	for off := 0; off < len(code); {
		inst, err := x86asm.Decode(code[off:], 64)
		if err != nil || inst.Len == 0 {
			off++
			prevOff = -1
			continue
		}
		if inst.Op == x86asm.SYSCALL {
			st.syscalls++
			a := lo + uint64(off)
			var start uint64
			var sysno uint32
			kind := 0
			if prevOff >= 0 && prevOff+prev.Len == off && prev.Op == x86asm.MOV {
				pc := code[prevOff : prevOff+prev.Len]
				if prev.Len == 5 && pc[0] == 0xB8 {
					kind, start, sysno = 32, a-5, binary.LittleEndian.Uint32(pc[1:])
				} else if prev.Len == 7 && pc[0] == 0x48 && pc[1] == 0xC7 && pc[2] == 0xC0 {
					kind, start, sysno = 64, a-7, binary.LittleEndian.Uint32(pc[3:])
				}
			}
			if kind != 0 && p.nsites < stubasm.MaxSites {
				idx := uint64(p.nsites)
				p.nsites++
				slot := stubasm.TrapPtrs + 8*idx
				entry := stubasm.TrapCode + stubasm.EntrySize*idx
				e := []byte{0xB8}
				e = binary.LittleEndian.AppendUint32(e, sysno)
				e = append(e, 0x48, 0xB9)
				e = binary.LittleEndian.AppendUint64(e, a+2)
				e = append(e, 0x49, 0xBB)
				e = binary.LittleEndian.AppendUint64(e, stubasm.StubCode+stubasm.OffTramp)
				e = append(e, 0x41, 0xFF, 0xE3)
				j := []byte{0xFF, 0x24, 0x25}
				j = binary.LittleEndian.AppendUint32(j, uint32(slot))
				if kind == 64 {
					j = append(j, 0x90, 0x90)
				}
				if !as.write(entry, e) || !as.write(slot, binary.LittleEndian.AppendUint64(nil, entry)) || !as.write(start, j) {
					return st, fmt.Errorf("patch write failed at %#x", a)
				}
				st.patched++
				if kind == 32 {
					st.imm32++
				} else {
					st.imm64++
				}
			} else {
				as.write(a, []byte{0x0F, 0x0B})
				st.ud2++
			}
		}
		prev, prevOff = inst, off
		off += inst.Len
	}
	st.nonSyscal = st.pairs - st.syscalls
	return st, nil
}
