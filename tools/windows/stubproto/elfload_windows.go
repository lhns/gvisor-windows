package main

import (
	"bytes"
	"crypto/rand"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"sort"

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

