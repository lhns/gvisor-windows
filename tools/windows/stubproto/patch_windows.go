package main

import (
	"encoding/binary"
	"fmt"

	"stubproto/internal/x86asm"
	"stubproto/stubasm"
)

// siteStats counts what the scanner found and did.
type siteStats struct {
	pairs     int // 0f 05 byte pairs in the scanned ranges
	syscalls  int // decoded SYSCALL instructions
	patched   int // imm32 + imm64 + reloc
	imm32     int // mov $imm32,%eax; syscall   -> jmp *slot
	imm64     int // mov $imm32,%rax; syscall   -> jmp *slot
	reloc     int // any other site            -> jmp rel32 to a per-site veneer
	relocXor  int // subset of reloc: xor %eax,%eax; syscall
	ud2       int // left as ud2 (debugger path)
	noWindow  int // ud2 reasons
	onTarget  int
	nonReloc  int
	noVeneer  int
	nonSyscal int // 0f 05 pairs inside other instructions or data
}

func (s *siteStats) add(o siteStats) {
	s.pairs += o.pairs
	s.syscalls += o.syscalls
	s.patched += o.patched
	s.imm32 += o.imm32
	s.imm64 += o.imm64
	s.reloc += o.reloc
	s.relocXor += o.relocXor
	s.ud2 += o.ud2
	s.noWindow += o.noWindow
	s.onTarget += o.onTarget
	s.nonReloc += o.nonReloc
	s.noVeneer += o.noVeneer
	s.nonSyscal += o.nonSyscal
}

type ins struct {
	off  int
	inst x86asm.Inst
}

// relocatable reports whether an instruction can be copied byte for byte to another
// address. Direct branches and rip-relative operands cannot. A ret or an indirect
// jmp ends the displaced sequence and is allowed only in last place.
func relocatable(in x86asm.Inst, last bool) bool {
	if in.PCRel != 0 {
		return false
	}
	switch in.Op.String() {
	case "SYSCALL", "SYSENTER", "INT", "CALL", "LCALL", "UD1", "UD2", "HLT", "INTO":
		return false
	case "RET", "LRET", "JMP", "LJMP":
		return last
	}
	return true
}

func isXorEAX(in x86asm.Inst) bool {
	return in.Op == x86asm.XOR && in.Args[0] == x86asm.EAX && in.Args[1] == x86asm.EAX
}

// scanRange scans every run of mapped pages in [lo,hi).
func (p *Proc) scanRange(lo, hi uint64) (siteStats, error) {
	var st siteStats
	for a := pageDown(lo); a < hi; {
		pgs := p.as.pages[a]
		if pgs == nil || pgs.state != 2 {
			a += pageSize
			continue
		}
		b := a
		for b < hi {
			q := p.as.pages[b]
			if q == nil || q.state != 2 {
				break
			}
			b += pageSize
		}
		s, err := p.scanAndPatch(a, b)
		st.add(s)
		if err != nil {
			return st, err
		}
		a = b
	}
	return st, nil
}

// scanAndPatch rewrites every syscall instruction in [lo,hi) before any of it can
// execute:
//   - `mov $imm32,%eax|%rax; syscall` becomes `jmp *slot` through the trap table;
//   - any other site becomes a 5-byte `jmp rel32` to a per-site veneer holding the
//     displaced instructions (copied unchanged) around a jump into the trampoline;
//   - a site that cannot be patched becomes ud2 (the debugger path).
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
	var list []ins
	targets := map[uint64]bool{}
	for off := 0; off < len(code); {
		inst, err := x86asm.Decode(code[off:], 64)
		if err != nil || inst.Len == 0 {
			off++
			continue
		}
		list = append(list, ins{off, inst})
		if rel, ok := inst.Args[0].(x86asm.Rel); ok {
			targets[uint64(int64(lo)+int64(off)+int64(inst.Len)+int64(rel))] = true
		}
		off += inst.Len
	}
	contig := func(i int) bool { return i > 0 && list[i-1].off+list[i-1].inst.Len == list[i].off }

	for s, x := range list {
		if x.inst.Op != x86asm.SYSCALL {
			continue
		}
		st.syscalls++
		a := lo + uint64(x.off)
		patched := false
		why := ""

		// imm32 / imm64 into eax/rax directly before the syscall
		if contig(s) && !targets[a] && list[s-1].inst.Op == x86asm.MOV && p.nsites < stubasm.MaxSites {
			pv := list[s-1]
			pc := code[pv.off : pv.off+pv.inst.Len]
			var start uint64
			var sysno uint32
			kind := 0
			if pv.inst.Len == 5 && pc[0] == 0xB8 {
				kind, start, sysno = 32, a-5, binary.LittleEndian.Uint32(pc[1:])
			} else if pv.inst.Len == 7 && pc[0] == 0x48 && pc[1] == 0xC7 && pc[2] == 0xC0 {
				kind, start, sysno = 64, a-7, binary.LittleEndian.Uint32(pc[3:])
			}
			if kind != 0 {
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
				patched = true
			}
		}
		if patched {
			continue
		}

		// relocating jump: smallest window of >= 5 bytes around the syscall
		bestA, bestB, bestL := -1, -1, 1<<30
		sawShort, sawTarget, sawNonReloc := false, false, false
		for ia := s; ia >= 0 && ia >= s-3; ia-- {
			if ia < s && !contig(ia+1) {
				break
			}
			for ib := s; ib < len(list) && ib <= s+3; ib++ {
				if ib > s && !contig(ib) {
					break
				}
				l := list[ib].off + list[ib].inst.Len - list[ia].off
				if l < 5 {
					sawShort = true
					continue
				}
				ok := true
				for k := ia; k <= ib; k++ {
					if k != s && !relocatable(list[k].inst, k == ib) {
						ok, sawNonReloc = false, true
						break
					}
					if k > ia && targets[lo+uint64(list[k].off)] {
						ok, sawTarget = false, true
						break
					}
				}
				if ok && l < bestL {
					bestA, bestB, bestL = ia, ib, l
				}
			}
		}
		if bestA >= 0 {
			w0 := lo + uint64(list[bestA].off)
			wEnd := w0 + uint64(bestL)
			var pre, post []byte
			for k := bestA; k < s; k++ {
				pre = append(pre, code[list[k].off:list[k].off+list[k].inst.Len]...)
			}
			for k := s + 1; k <= bestB; k++ {
				post = append(post, code[list[k].off:list[k].off+list[k].inst.Len]...)
			}
			vlen := uint64(len(pre)+23+len(post)+23+15) &^ 15
			if p.vcur != 0 && p.vcur+vlen <= p.vhi {
				va := p.vcur
				rel := int64(va) - int64(w0+5)
				if rel >= -1<<31 && rel < 1<<31 {
					v := append([]byte(nil), pre...)
					postAddr := va + uint64(len(pre)) + 23
					v = append(v, 0x48, 0xB9)
					v = binary.LittleEndian.AppendUint64(v, postAddr)
					v = append(v, 0x49, 0xBB)
					v = binary.LittleEndian.AppendUint64(v, stubasm.StubCode+stubasm.OffTramp)
					v = append(v, 0x41, 0xFF, 0xE3)
					v = append(v, post...)
					v = append(v, 0x49, 0xBB)
					v = binary.LittleEndian.AppendUint64(v, wEnd)
					v = append(v, 0x41, 0xFF, 0xE3)
					j := []byte{0xE9}
					j = binary.LittleEndian.AppendUint32(j, uint32(int32(rel)))
					for len(j) < bestL {
						j = append(j, 0x90)
					}
					if !as.write(va, v) || !as.write(w0, j) {
						return st, fmt.Errorf("veneer write failed at %#x", a)
					}
					p.vcur += vlen
					st.patched++
					st.reloc++
					if bestA < s && isXorEAX(list[s-1].inst) {
						st.relocXor++
					}
					continue
				}
				why = "far"
			} else {
				why = "veneer"
			}
			st.noVeneer++
		} else {
			switch {
			case sawTarget:
				st.onTarget++
				why = "branch target"
			case sawNonReloc:
				st.nonReloc++
				why = "non-relocatable neighbour"
			default:
				st.noWindow++
				why = "no window"
				_ = sawShort
			}
		}
		as.write(a, []byte{0x0F, 0x0B})
		st.ud2++
		lo2 := max(0, x.off-12)
		p.c.logf("pid %d: ud2 site %#x (%s): ...% x | 0f 05 | % x", p.pid, a, why, code[lo2:x.off], code[x.off+2:min(len(code), x.off+10)])
	}
	st.nonSyscal = st.pairs - st.syscalls
	return st, nil
}
