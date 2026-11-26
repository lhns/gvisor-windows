package main

import (
	"fmt"
	"sort"
	"unsafe"
)

const pageSize = 4096

func pageDown(a uint64) uint64 { return a &^ (pageSize - 1) }
func pageUp(a uint64) uint64   { return (a + pageSize - 1) &^ (pageSize - 1) }

// memFile is the pagefile-backed section all guest memory comes from. The controller
// keeps the whole section mapped, so it can read and write any guest page directly.
type memFile struct {
	sec   uintptr
	base  unsafe.Pointer
	size  uint64
	next  uint64
	free  []uint64
	peak  uint64
	inUse uint64
}

func newMemFile(size uint64) (*memFile, error) {
	s, _, e := pCreateMapping.Call(^uintptr(0), 0, pageExecRW, uintptr(size>>32), uintptr(size&0xFFFFFFFF), 0)
	if s == 0 {
		return nil, fmt.Errorf("CreateFileMapping: %v", e)
	}
	b, _, e := pMapViewOfFile.Call(s, fileMapAllExec, 0, 0, 0)
	if b == 0 {
		return nil, fmt.Errorf("MapViewOfFile: %v", e)
	}
	return &memFile{sec: s, base: ptr(b), size: size}, nil
}

func (m *memFile) page(off uint64) []byte {
	return unsafe.Slice((*byte)(unsafe.Add(m.base, off)), pageSize)
}

func (m *memFile) alloc() (uint64, error) {
	m.inUse++
	if m.inUse > m.peak {
		m.peak = m.inUse
	}
	if n := len(m.free); n > 0 {
		off := m.free[n-1]
		m.free = m.free[:n-1]
		clear(m.page(off))
		return off, nil
	}
	if m.next+pageSize > m.size {
		return 0, fmt.Errorf("memory file exhausted")
	}
	off := m.next
	m.next += pageSize
	return off, nil
}

func (m *memFile) release(off uint64) {
	m.inUse--
	m.free = append(m.free, off)
}

type vma struct {
	lo, hi uint64
	prot   int
	kind   string
}

// pstate: 0 = inside a larger placeholder, 1 = a 4 KB placeholder of its own, 2 = mapped.
type pg struct {
	off   uint64
	prot  int
	state uint8
}

// AS is one stub's guest address space: VMAs plus 4 KB views of the memory file.
type AS struct {
	mf    *memFile
	h     uintptr
	vmas  []vma
	gaps  []rng
	pages map[uint64]*pg
}

func newAS(mf *memFile, h uintptr) *AS {
	return &AS{mf: mf, h: h, pages: map[uint64]*pg{}}
}

// winWindow is left unreserved so Windows can place late allocations (thread
// stacks, heaps) inside the low 8 TB if it insists on it.
var winWindow = rng{0x7F0_0000_0000, 0x7F0_4000_0000}

// reserve turns every free, 64 KB-aligned gap of the guest range into a placeholder.
func (as *AS) reserve(used []rng) (reserved uint64, err error) {
	lo := stubGuestMin
	cuts := append([]rng(nil), used...)
	cuts = append(cuts, winWindow)
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].lo < cuts[j].lo })
	cur := lo
	add := func(s, e uint64) error {
		s = (s + 0xFFFF) &^ 0xFFFF
		e &^= 0xFFFF
		if e <= s {
			return nil
		}
		r, _, er := pVirtualAlloc2.Call(as.h, uintptr(s), uintptr(e-s), memReserve|memPlaceholder, pageNoAccess, 0, 0)
		if r == 0 {
			return fmt.Errorf("VirtualAlloc2 placeholder %#x-%#x: %v", s, e, er)
		}
		as.gaps = append(as.gaps, rng{s, e})
		reserved += e - s
		return nil
	}
	for _, c := range cuts {
		if c.hi <= cur {
			continue
		}
		if c.lo > cur {
			end := c.lo
			if end > stubGuestMax {
				end = stubGuestMax
			}
			if err := add(cur, end); err != nil {
				return reserved, err
			}
		}
		if c.hi > cur {
			cur = c.hi
		}
	}
	if cur < stubGuestMax {
		if err := add(cur, stubGuestMax); err != nil {
			return reserved, err
		}
	}
	return reserved, nil
}

func (as *AS) inGap(lo, hi uint64) bool {
	for _, g := range as.gaps {
		if lo >= g.lo && hi <= g.hi {
			return true
		}
	}
	return false
}

func (as *AS) findVMA(a uint64) *vma {
	i := sort.Search(len(as.vmas), func(i int) bool { return as.vmas[i].hi > a })
	if i < len(as.vmas) && as.vmas[i].lo <= a {
		return &as.vmas[i]
	}
	return nil
}

func (as *AS) overlaps(lo, hi uint64) bool {
	i := sort.Search(len(as.vmas), func(i int) bool { return as.vmas[i].hi > lo })
	return i < len(as.vmas) && as.vmas[i].lo < hi
}

func (as *AS) insertVMA(v vma) {
	i := sort.Search(len(as.vmas), func(i int) bool { return as.vmas[i].lo >= v.lo })
	as.vmas = append(as.vmas, vma{})
	copy(as.vmas[i+1:], as.vmas[i:])
	as.vmas[i] = v
}

// addVMA registers a lazily mapped region (it must lie in a gap and be free).
func (as *AS) addVMA(lo, hi uint64, prot int, kind string) error {
	if !as.inGap(lo, hi) {
		return fmt.Errorf("%s %#x-%#x is not inside the reserved guest range", kind, lo, hi)
	}
	if as.overlaps(lo, hi) {
		return fmt.Errorf("%s %#x-%#x overlaps another mapping", kind, lo, hi)
	}
	as.insertVMA(vma{lo, hi, prot, kind})
	return nil
}

// addMapped is addVMA plus eager mapping of every page.
func (as *AS) addMapped(lo, hi uint64, prot int, kind string) error {
	if err := as.addVMA(lo, hi, prot, kind); err != nil {
		return err
	}
	for a := lo; a < hi; a += pageSize {
		if err := as.mapPage(a, prot); err != nil {
			return err
		}
	}
	return nil
}

func (as *AS) mapPage(a uint64, prot int) error {
	p := as.pages[a]
	if p == nil {
		p = &pg{}
		as.pages[a] = p
	}
	if p.state == 2 {
		return nil
	}
	if prot == 0 {
		return nil
	}
	if p.state == 0 {
		// Fails harmlessly when the placeholder already is exactly this page.
		pVirtualFreeEx.Call(as.h, uintptr(a), pageSize, memRelease|memPreservePH)
		p.state = 1
	}
	off, err := as.mf.alloc()
	if err != nil {
		return err
	}
	r, _, e := pMapViewOfFile3.Call(as.mf.sec, as.h, uintptr(a), uintptr(off), pageSize, memReplacePH, winProt(prot), 0, 0)
	if r == 0 {
		as.mf.release(off)
		return fmt.Errorf("MapViewOfFile3 at %#x: %v", a, e)
	}
	p.state, p.off, p.prot = 2, off, prot
	return nil
}

func (as *AS) unmapPage(a uint64) {
	p := as.pages[a]
	if p == nil || p.state != 2 {
		return
	}
	pUnmapViewOfFile2.Call(as.h, uintptr(a), memPreservePH)
	as.mf.release(p.off)
	p.state = 1
}

// removeRange unmaps [lo,hi) and cuts it out of the VMAs.
func (as *AS) removeRange(lo, hi uint64) {
	var out []vma
	for _, v := range as.vmas {
		if v.hi <= lo || v.lo >= hi {
			out = append(out, v)
			continue
		}
		if v.lo < lo {
			out = append(out, vma{v.lo, lo, v.prot, v.kind})
		}
		if v.hi > hi {
			out = append(out, vma{hi, v.hi, v.prot, v.kind})
		}
	}
	as.vmas = out
	for a, p := range as.pages {
		if a >= lo && a < hi && p.state == 2 {
			as.unmapPage(a)
		}
	}
}

// protect changes the protection of [lo,hi), which must be fully covered by VMAs.
func (as *AS) protect(lo, hi uint64, prot int) bool {
	for a := lo; a < hi; {
		v := as.findVMA(a)
		if v == nil {
			return false
		}
		a = v.hi
	}
	var out []vma
	for _, v := range as.vmas {
		if v.hi <= lo || v.lo >= hi {
			out = append(out, v)
			continue
		}
		if v.lo < lo {
			out = append(out, vma{v.lo, lo, v.prot, v.kind})
		}
		a, b := max(v.lo, lo), min(v.hi, hi)
		out = append(out, vma{a, b, prot, v.kind})
		if v.hi > hi {
			out = append(out, vma{hi, v.hi, v.prot, v.kind})
		}
	}
	as.vmas = out
	for a := lo; a < hi; a += pageSize {
		p := as.pages[a]
		if p == nil || p.state != 2 || p.prot == prot {
			continue
		}
		var old uint32
		pVirtualProtectEx.Call(as.h, uintptr(a), pageSize, winProt(prot), uintptr(unsafe.Pointer(&old)))
		p.prot = prot
	}
	return true
}

const faultAround = 8 // pages mapped per lazy fault

// fault maps the page(s) around a (a guest access to an unmapped page). It reports
// false if a is not in a VMA or the VMA does not allow the access.
func (as *AS) fault(a uint64, write, exec bool) bool {
	v := as.findVMA(a)
	if v == nil || v.prot == 0 {
		return false
	}
	if (write && v.prot&2 == 0) || (exec && v.prot&4 == 0) || (!write && !exec && v.prot&5 == 0) {
		return false
	}
	pa := pageDown(a)
	if p := as.pages[pa]; p != nil && p.state == 2 {
		return false // already mapped: a genuine protection fault
	}
	lo := max(pa&^(faultAround*pageSize-1), v.lo)
	hi := min((pa&^(faultAround*pageSize-1))+faultAround*pageSize, v.hi)
	for x := lo; x < hi; x += pageSize {
		if err := as.mapPage(x, v.prot); err != nil {
			if x == pa {
				fmt.Printf("stubproto: fault map failed: %v\n", err)
				return false
			}
			break
		}
	}
	return true
}

// ensure makes sure the page holding a is mapped (for controller-side access).
func (as *AS) ensure(a uint64) *pg {
	pa := pageDown(a)
	if p := as.pages[pa]; p != nil && p.state == 2 {
		return p
	}
	v := as.findVMA(a)
	if v == nil {
		return nil
	}
	pr := v.prot
	if pr == 0 {
		pr = 1
	}
	if err := as.mapPage(pa, pr); err != nil {
		return nil
	}
	return as.pages[pa]
}

func (as *AS) read(a uint64, n int) ([]byte, bool) {
	out := make([]byte, 0, n)
	for n > 0 {
		p := as.ensure(a)
		if p == nil {
			return nil, false
		}
		o := int(a & (pageSize - 1))
		c := min(n, pageSize-o)
		out = append(out, as.mf.page(p.off)[o:o+c]...)
		a += uint64(c)
		n -= c
	}
	return out, true
}

func (as *AS) write(a uint64, b []byte) bool {
	for len(b) > 0 {
		p := as.ensure(a)
		if p == nil {
			return false
		}
		o := int(a & (pageSize - 1))
		c := min(len(b), pageSize-o)
		copy(as.mf.page(p.off)[o:], b[:c])
		a += uint64(c)
		b = b[c:]
	}
	return true
}

func (as *AS) readU64(a uint64) (uint64, bool) {
	b, ok := as.read(a, 8)
	if !ok {
		return 0, false
	}
	return le64(b), true
}

func (as *AS) readU32(a uint64) (uint32, bool) {
	b, ok := as.read(a, 4)
	if !ok {
		return 0, false
	}
	return uint32(le64(append(b, 0, 0, 0, 0))), true
}

func (as *AS) readCString(a uint64, max int) (string, bool) {
	var s []byte
	for len(s) < max {
		b, ok := as.read(a+uint64(len(s)), 1)
		if !ok {
			return "", false
		}
		if b[0] == 0 {
			return string(s), true
		}
		s = append(s, b[0])
	}
	return string(s), true
}

func le64(b []byte) uint64 {
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}

func putLE(v uint64, n int) []byte {
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		b[i] = byte(v >> (8 * i))
	}
	return b
}

// findFree picks a free range of size bytes inside the gaps, top-down below top.
func (as *AS) findFree(size, top uint64) (uint64, bool) {
	for gi := len(as.gaps) - 1; gi >= 0; gi-- {
		g := as.gaps[gi]
		hi := min(g.hi, top)
		for hi > g.lo && hi-g.lo >= size {
			cand := (hi - size) &^ (pageSize - 1)
			if cand < g.lo {
				break
			}
			// the highest VMA overlapping [cand, hi)
			i := sort.Search(len(as.vmas), func(i int) bool { return as.vmas[i].hi > cand })
			if i < len(as.vmas) && as.vmas[i].lo < hi {
				// blocked: retry below the highest blocker
				j := i
				for j+1 < len(as.vmas) && as.vmas[j+1].lo < hi {
					j++
				}
				hi = as.vmas[j].lo
				continue
			}
			return cand, true
		}
	}
	return 0, false
}

// cloneInto copies every mapped page into dst (a fresh stub with the same layout).
func (as *AS) cloneInto(dst *AS) error {
	for _, v := range as.vmas {
		if !dst.inGap(v.lo, v.hi) {
			return fmt.Errorf("layout conflict at %#x-%#x (%s)", v.lo, v.hi, v.kind)
		}
	}
	dst.vmas = append([]vma(nil), as.vmas...)
	for a, p := range as.pages {
		if p.state != 2 {
			continue
		}
		if err := dst.mapPage(a, p.prot); err != nil {
			return err
		}
		copy(dst.mf.page(dst.pages[a].off), as.mf.page(p.off))
	}
	return nil
}
