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
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/x86asm"
)

// siteStats counts what the scanner found and did.
type siteStats struct {
	pairs    int // 0f 05 byte pairs in the scanned range
	syscalls int // decoded SYSCALL instructions
	imm      int // mov $imm32,%eax|%rax; syscall -> jmp *slot
	reloc    int // any other site -> jump to a per-site veneer
	ud2      int // left as ud2 (debugger path)
}

// Instruction classes for displacing an instruction into a veneer.
const (
	clsOK       = iota // can be copied byte for byte
	clsLastOnly        // ret or indirect jmp: only as the last displaced instruction
	clsNever           // rip-relative, relative branch, call, int, ud2, ...
)

type ins struct {
	off int
	n   int
	op  x86asm.Op
	cls uint8
}

func classify(in x86asm.Inst) uint8 {
	if in.PCRel != 0 {
		return clsNever
	}
	switch in.Op {
	case x86asm.SYSCALL, x86asm.SYSENTER, x86asm.INT, x86asm.CALL, x86asm.LCALL, x86asm.UD1, x86asm.UD2, x86asm.HLT, x86asm.INTO:
		return clsNever
	case x86asm.RET, x86asm.LRET, x86asm.JMP, x86asm.LJMP:
		return clsLastOnly
	}
	return clsOK
}

// siteInfo remembers a patched `mov $imm,%eax; syscall` site so that scanning the
// same text again (W^X rescans) reuses its table entry.
type siteInfo struct {
	nr    uint32
	entry uint64
}

// areaWrite writes into the low stub area (trap table, veneers).
func (as *addressSpace) areaWrite(addr uint64, b []byte) {
	copy(unsafe.Slice((*byte)(unsafe.Add(as.areaView, addr-stubasm.AreaLo)), len(b)), b)
}

// patchCode rewrites every syscall instruction in code (the bytes that will be
// mapped at base) before any of it can execute:
//   - `mov $imm32,%eax|%rax; syscall` becomes `jmp *slot` through the trap table;
//   - any other site becomes `jmp rel32` (or `jmp *slot` when the veneer is out of
//     rel32 reach) to a per-site veneer holding the displaced instructions (copied
//     unchanged) around a jump into the trampoline;
//   - a site that cannot be patched becomes ud2 (the debugger path).
//
// Bookkeeping for restarts and ud2 sites goes to as.restartAt and as.ud2Sites.
// The caller holds as.mu.
func (as *addressSpace) patchCode(code []byte, base uint64) siteStats {
	var st siteStats
	for i := 0; i+1 < len(code); i++ {
		if code[i] == 0x0F && code[i+1] == 0x05 {
			st.pairs++
		}
	}
	var list []ins
	targets := map[uint64]bool{}
	for off := 0; off < len(code); {
		inst, err := x86asm.Decode(code[off:], 64)
		if err != nil || inst.Len == 0 {
			off++
			continue
		}
		list = append(list, ins{off: off, n: inst.Len, op: inst.Op, cls: classify(inst)})
		if rel, ok := inst.Args[0].(x86asm.Rel); ok {
			targets[uint64(int64(base)+int64(off)+int64(inst.Len)+int64(rel))] = true
		}
		off += inst.Len
	}
	contig := func(i int) bool { return i > 0 && list[i-1].off+list[i-1].n == list[i].off }

	for s, x := range list {
		if x.op != x86asm.SYSCALL {
			continue
		}
		st.syscalls++
		a := base + uint64(x.off)
		if as.patchImm(code, base, list, s, targets, contig, &st) {
			continue
		}
		if as.patchReloc(code, base, list, s, targets, contig, &st) {
			continue
		}
		code[x.off], code[x.off+1] = 0x0F, 0x0B
		as.ud2Sites[a] = true
		st.ud2++
		debugf("ud2 site %#x", a)
	}
	return st
}

func (as *addressSpace) newSlot() (idx uint64, ok bool) {
	if as.nsites >= stubasm.MaxSites {
		return 0, false
	}
	idx = uint64(as.nsites)
	as.nsites++
	return idx, true
}

func (as *addressSpace) patchImm(code []byte, base uint64, list []ins, s int, targets map[uint64]bool, contig func(int) bool, st *siteStats) bool {
	x := list[s]
	a := base + uint64(x.off)
	if !contig(s) || targets[a] || list[s-1].op != x86asm.MOV {
		return false
	}
	pv := list[s-1]
	pc := code[pv.off : pv.off+pv.n]
	var sysno uint32
	kind := 0
	if pv.n == 5 && pc[0] == 0xB8 {
		kind, sysno = 32, binary.LittleEndian.Uint32(pc[1:])
	} else if pv.n == 7 && pc[0] == 0x48 && pc[1] == 0xC7 && pc[2] == 0xC0 {
		kind, sysno = 64, binary.LittleEndian.Uint32(pc[3:])
	} else {
		return false
	}
	var idx, entry uint64
	if si, ok := as.siteCache[a]; ok && si.nr == sysno {
		entry = si.entry
		idx = (entry - stubasm.TrapCode) / stubasm.EntrySize
	} else {
		var ok bool
		if idx, ok = as.newSlot(); !ok {
			return false
		}
		entry = stubasm.TrapCode + stubasm.EntrySize*idx
		e := []byte{0xB8}
		e = binary.LittleEndian.AppendUint32(e, sysno)
		e = append(e, 0x48, 0xB9)
		e = binary.LittleEndian.AppendUint64(e, a+2)
		e = append(e, 0x49, 0xBB)
		e = binary.LittleEndian.AppendUint64(e, stubasm.StubCode+stubasm.OffTramp)
		e = append(e, 0x41, 0xFF, 0xE3)
		as.areaWrite(entry, e)
		as.areaWrite(stubasm.TrapPtrs+8*idx, binary.LittleEndian.AppendUint64(nil, entry))
		as.siteCache[a] = siteInfo{sysno, entry}
	}
	j := []byte{0xFF, 0x24, 0x25}
	j = binary.LittleEndian.AppendUint32(j, uint32(stubasm.TrapPtrs+8*idx))
	if kind == 64 {
		j = append(j, 0x90, 0x90)
	}
	copy(code[pv.off:], j)
	// The Sentry restarts an interrupted syscall by moving the instruction pointer
	// back by 2 (to the syscall instruction, now the middle of the jump): send it to
	// the table entry instead, which re-enters the trampoline with the number.
	as.restartAt[a] = entry
	st.imm++
	return true
}

// patchReloc patches a site that is not `mov $imm,%eax; syscall`: the smallest
// window of whole instructions around the syscall in which no instruction except
// the first is a branch target and every instruction can be copied, becomes a jump
// to a veneer.
func (as *addressSpace) patchReloc(code []byte, base uint64, list []ins, s int, targets map[uint64]bool, contig func(int) bool, st *siteStats) bool {
	x := list[s]
	a := base + uint64(x.off)
	va := as.vcur
	near := func(from uint64) bool {
		rel := int64(va) - int64(from+5)
		return rel >= -1<<31 && rel < 1<<31
	}
	minLen := 5
	if !near(a) {
		minLen = 7 // out of rel32 reach: jmp *abs32 through a table slot
	}
	bestA, bestB, bestL := -1, -1, 1<<30
	for ia := s; ia >= 0 && ia >= s-3; ia-- {
		if ia < s && !contig(ia+1) {
			break
		}
		for ib := s; ib < len(list) && ib <= s+3; ib++ {
			if ib > s && !contig(ib) {
				break
			}
			l := list[ib].off + list[ib].n - list[ia].off
			if l < minLen {
				continue
			}
			ok := true
			for k := ia; k <= ib; k++ {
				if k != s && (list[k].cls == clsNever || (list[k].cls == clsLastOnly && k != ib)) {
					ok = false
					break
				}
				if k > ia && targets[base+uint64(list[k].off)] {
					ok = false
					break
				}
			}
			if ok && l < bestL {
				bestA, bestB, bestL = ia, ib, l
			}
		}
	}
	if bestA < 0 {
		return false
	}
	w0 := base + uint64(list[bestA].off)
	wEnd := w0 + uint64(bestL)
	var pre, post []byte
	for k := bestA; k < s; k++ {
		pre = append(pre, code[list[k].off:list[k].off+list[k].n]...)
	}
	for k := s + 1; k <= bestB; k++ {
		post = append(post, code[list[k].off:list[k].off+list[k].n]...)
	}
	vlen := uint64(len(pre)+23+len(post)+23+15) &^ 15
	if va+vlen > stubasm.Veneers+stubasm.VeneersSize {
		return false
	}
	var j []byte
	if minLen == 5 {
		if !near(w0) {
			return false
		}
		j = []byte{0xE9}
		j = binary.LittleEndian.AppendUint32(j, uint32(int32(int64(va)-int64(w0+5))))
	} else {
		idx, ok := as.newSlot()
		if !ok {
			return false
		}
		as.areaWrite(stubasm.TrapPtrs+8*idx, binary.LittleEndian.AppendUint64(nil, va))
		j = []byte{0xFF, 0x24, 0x25}
		j = binary.LittleEndian.AppendUint32(j, uint32(stubasm.TrapPtrs+8*idx))
	}
	for len(j) < bestL {
		j = append(j, 0x90)
	}
	postAddr := va + uint64(len(pre)) + 23
	v := append([]byte(nil), pre...)
	v = append(v, 0x48, 0xB9) // movabs $post,%rcx
	v = binary.LittleEndian.AppendUint64(v, postAddr)
	v = append(v, 0x49, 0xBB) // movabs $tramp,%r11
	v = binary.LittleEndian.AppendUint64(v, stubasm.StubCode+stubasm.OffTramp)
	v = append(v, 0x41, 0xFF, 0xE3) // jmp *%r11
	v = append(v, post...)
	v = append(v, 0x49, 0xBB) // movabs $end,%r11
	v = binary.LittleEndian.AppendUint64(v, wEnd)
	v = append(v, 0x41, 0xFF, 0xE3)
	as.areaWrite(va, v)
	as.vcur += vlen
	copy(code[list[bestA].off:], j)
	// A restarted syscall re-enters the trampoline from the veneer; the registers
	// before the syscall (rax in particular) are restored by the Sentry.
	as.restartAt[postAddr-2] = va + uint64(len(pre))
	st.reloc++
	return true
}
