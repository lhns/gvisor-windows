package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	k32          = syscall.NewLazyDLL("kernel32.dll")
	kb           = syscall.NewLazyDLL("kernelbase.dll")
	pVirtualAlloc2  = kb.NewProc("VirtualAlloc2")
	pMapViewOfFile3 = kb.NewProc("MapViewOfFile3")
	pUnmapViewOfFile2 = kb.NewProc("UnmapViewOfFile2")
	pVirtualFreeEx  = k32.NewProc("VirtualFreeEx")
	pVirtualProtectEx = k32.NewProc("VirtualProtectEx")
	pVirtualQueryEx = k32.NewProc("VirtualQueryEx")
	pCreateMapping  = k32.NewProc("CreateFileMappingW")
	pTerminate      = k32.NewProc("TerminateProcess")
)

const (
	MEM_COMMIT = 0x1000
	MEM_RESERVE = 0x2000
	MEM_RELEASE = 0x8000
	MEM_RESERVE_PLACEHOLDER = 0x40000
	MEM_REPLACE_PLACEHOLDER = 0x4000
	MEM_PRESERVE_PLACEHOLDER = 0x2
	PAGE_NOACCESS = 1
	PAGE_READONLY = 2
	PAGE_READWRITE = 4
)

func lasterr(r uintptr, e error) string {
	if e == nil {
		return "ok"
	}
	if en, ok := e.(syscall.Errno); ok {
		return fmt.Sprintf("error %d (%#x)", uint32(en), uint32(en))
	}
	return e.Error()
}

// returns true on success
func line(name string, ok bool, detail string) {
	s := "PASS"
	if !ok {
		s = "FAIL"
	}
	fmt.Printf("%s %s: %s\n", s, name, detail)
}

func valloc2(h syscall.Handle, base, size, typ, prot uintptr) (uintptr, string) {
	r, _, e := pVirtualAlloc2.Call(uintptr(h), base, size, typ, prot, 0, 0)
	if r == 0 {
		return 0, lasterr(r, e)
	}
	return r, "ok"
}

func split(h syscall.Handle, addr, size uintptr) (bool, string) {
	r, _, e := pVirtualFreeEx.Call(uintptr(h), addr, size, MEM_RELEASE|MEM_PRESERVE_PLACEHOLDER)
	if r == 0 {
		return false, lasterr(r, e)
	}
	return true, "ok"
}

func mapview(h syscall.Handle, sec syscall.Handle, addr uintptr, off uint64, size uintptr, atype, prot uintptr) (uintptr, string) {
	r, _, e := pMapViewOfFile3.Call(uintptr(sec), uintptr(h), addr, uintptr(off), size, atype, prot, 0, 0)
	if r == 0 {
		return 0, lasterr(r, e)
	}
	return r, "ok"
}

func unmap(h syscall.Handle, addr uintptr, preserve bool) string {
	fl := uintptr(0)
	if preserve {
		fl = MEM_PRESERVE_PLACEHOLDER
	}
	r, _, e := pUnmapViewOfFile2.Call(uintptr(h), addr, fl)
	if r == 0 {
		return lasterr(r, e)
	}
	return "ok"
}

type mbi struct {
	Base, AllocBase uintptr
	AllocProt, _    uint32
	Size            uintptr
	State, Protect, Type uint32
	_               uint32
}

func query(h syscall.Handle, addr uintptr) string {
	var m mbi
	r, _, _ := pVirtualQueryEx.Call(uintptr(h), addr, uintptr(unsafe.Pointer(&m)), unsafe.Sizeof(m))
	if r == 0 {
		return "query failed"
	}
	return fmt.Sprintf("[base=%#x size=%#x state=%#x prot=%#x type=%#x]", m.Base, m.Size, m.State, m.Protect, m.Type)
}

func main() {
	// harmless suspended child
	argv, _ := syscall.UTF16PtrFromString(`C:\Windows\System32\cmd.exe /c pause`)
	var si syscall.StartupInfo
	var pi syscall.ProcessInformation
	si.Cb = uint32(unsafe.Sizeof(si))
	err := syscall.CreateProcess(nil, argv, nil, nil, false, 0x4 /*CREATE_SUSPENDED*/|0x08000000, nil, nil, &si, &pi)
	if err != nil {
		fmt.Println("FAIL CreateProcess:", err)
		return
	}
	defer pTerminate.Call(uintptr(pi.Process), 0)
	h := pi.Process
	fmt.Printf("INFO child pid=%d suspended\n", pi.ProcessId)

	// section: 1 MB pagefile-backed, filled with a pattern is not needed here
	sec, _, e := pCreateMapping.Call(^uintptr(0), 0, PAGE_READWRITE, 0, 0x100000, 0)
	if sec == 0 {
		fmt.Println("FAIL CreateFileMapping", e)
		return
	}
	S := syscall.Handle(sec)

	// A: reserve placeholder 1 MB anywhere in child
	base, msg := valloc2(h, 0, 0x100000, MEM_RESERVE|MEM_RESERVE_PLACEHOLDER, PAGE_NOACCESS)
	line("S2.reserve_placeholder_1MB", base != 0, fmt.Sprintf("%s base=%#x", msg, base))
	if base == 0 {
		return
	}
	fmt.Println("INFO base alignment:", fmt.Sprintf("%#x mod 64K = %#x", base, base%0x10000))

	// B: control: split first 64 KB and map a 64K view of the section at its 64K-aligned address
	ok, msg := split(h, base, 0x10000)
	line("S2.split_64K", ok, msg)
	a, msg := mapview(h, S, base, 0, 0x10000, MEM_REPLACE_PLACEHOLDER, PAGE_READWRITE)
	line("S2.map_64K_at_64K_aligned (control)", a != 0, msg)

	// C: VirtualProtectEx on single 4K page inside the 64K view
	var old uint32
	r, _, e := pVirtualProtectEx.Call(uintptr(h), base+0x1000, 0x1000, PAGE_READONLY, uintptr(unsafe.Pointer(&old)))
	line("S2.VirtualProtectEx_single_4K_in_64K_view", r != 0, fmt.Sprintf("%s oldprot=%#x", lasterr(r, e), old))
	r, _, e = pVirtualProtectEx.Call(uintptr(h), base+0x2000, 0x1000, PAGE_NOACCESS, uintptr(unsafe.Pointer(&old)))
	line("S2.VirtualProtectEx_4K_to_NOACCESS", r != 0, lasterr(r, e))
	fmt.Println("INFO query 0x0000:", query(h, base))
	fmt.Println("INFO query 0x1000:", query(h, base+0x1000))
	fmt.Println("INFO query 0x2000:", query(h, base+0x2000))
	fmt.Println("INFO query 0x3000:", query(h, base+0x3000))

	// D: map with 4K file offset at 64K-aligned placeholder
	pl := base + 0x10000
	ok, msg = split(h, pl, 0x10000)
	line("S2.split_next_64K", ok, msg)
	a, msg = mapview(h, S, pl, 0x1000, 0x10000, MEM_REPLACE_PLACEHOLDER, PAGE_READWRITE)
	line("S2.map_view_at_4K_file_offset (64K-aligned addr)", a != 0, msg)
	a, msg = mapview(h, S, pl, 0x10000, 0x10000, MEM_REPLACE_PLACEHOLDER, PAGE_READWRITE)
	line("S2.map_view_at_64K_file_offset (control)", a != 0, msg)

	// E: 4K-aligned (not 64K) placeholder: split at +0x1000 inside the next region
	pl2 := base + 0x20000
	ok, msg = split(h, pl2, 0x1000)
	line("S2.split_placeholder_4K_size", ok, msg)
	if ok {
		a, msg = mapview(h, S, pl2, 0, 0x1000, MEM_REPLACE_PLACEHOLDER, PAGE_READWRITE)
		line("S2.map_4K_view_into_4K_placeholder", a != 0, msg)
	}
	ok, msg = split(h, pl2+0x1000, 0x1000)
	line("S2.split_at_4K_offset_after_first_4K", ok, msg)
	pl3 := pl2 + 0x1000
	a, msg = mapview(h, S, pl3, 0, 0x1000, MEM_REPLACE_PLACEHOLDER, PAGE_READWRITE)
	line("S2.map_4K_view_at_4K_aligned_addr (not 64K aligned)", a != 0, msg)
	a, msg = mapview(h, S, pl3+0x1000, 0, 0x10000, MEM_REPLACE_PLACEHOLDER, PAGE_READWRITE)
	line("S2.map_64K_view_at_4K_aligned_placeholder_addr", a != 0, msg)

	// F: no placeholder, explicit non-64K-aligned address
	a, msg = mapview(h, S, base+0x40000+0x1000, 0, 0x1000, 0, PAGE_READWRITE)
	line("S2.MapViewOfFile3_explicit_addr_4K_aligned_no_placeholder", a != 0, msg)
	a, msg = mapview(h, S, base+0x40000, 0x1000, 0x1000, 0, PAGE_READWRITE)
	line("S2.MapViewOfFile3_4K_file_offset_no_placeholder", a != 0, msg)
	// G: VirtualAlloc2 4K placeholder at odd 4K address
	b2, msg := valloc2(h, base+0x80000+0x1000, 0x1000, MEM_RESERVE|MEM_RESERVE_PLACEHOLDER, PAGE_NOACCESS)
	line("S2.VirtualAlloc2_placeholder_at_4K_aligned_addr", b2 != 0, msg)
	extra(h, S, base)
	// H: file-offset granularity with 4K view size (size 4K, 64K aligned addr)
	ok, msg = split(h, base+0x30000, 0x1000)
	if ok {
		a, msg = mapview(h, S, base+0x30000, 0x1000, 0x1000, MEM_REPLACE_PLACEHOLDER, PAGE_READWRITE)
		line("S2.map_4K_view_at_4K_file_offset_into_4K_placeholder", a != 0, msg)
	} else {
		line("S2.split_for_H", false, msg)
	}
}

func init() { extra = extraTests }

var extra func(h syscall.Handle, S syscall.Handle, base uintptr)

func extraTests(h syscall.Handle, S syscall.Handle, base uintptr) {
	pMapViewOfFile := k32.NewProc("MapViewOfFile")
	pRPM := k32.NewProc("ReadProcessMemory")
	pWPM := k32.NewProc("WriteProcessMemory")
	// parent view of the whole section; write markers at file offsets 0x1000, 0x5000, 0x10000
	pv, _, _ := pMapViewOfFile.Call(uintptr(S), 0xF001F, 0, 0, 0x100000)
	for _, off := range []uintptr{0x1000, 0x5000, 0x10000} {
		*(*uint64)(unsafe.Pointer(pv + off)) = 0xC0DE0000 + uint64(off)
	}
	// fresh region: 4K-aligned (not 64K-aligned) start, 64K view at 4K file offset
	reg, _ := valloc2(h, 0, 0x200000, MEM_RESERVE|MEM_RESERVE_PLACEHOLDER, PAGE_NOACCESS)
	addr := reg + 0x5000 // 4K-aligned, not 64K-aligned
	ok, msg := split(h, reg, 0x5000)
	line("S2b.split_leading_20K", ok, msg)
	ok, msg = split(h, addr, 0x10000)
	line("S2b.split_64K_at_4K_aligned_addr", ok, msg)
	a, msg := mapview(h, S, addr, 0x4000, 0x10000, MEM_REPLACE_PLACEHOLDER, PAGE_READWRITE)
	line("S2b.map_64K_view_at_4K_aligned_addr_and_16K_file_offset", a != 0, msg)
	// section offset 0x5000 should appear at addr + 0x1000
	var got uint64
	var n uintptr
	r, _, e := pRPM.Call(uintptr(h), addr+0x1000, uintptr(unsafe.Pointer(&got)), 8, uintptr(unsafe.Pointer(&n)))
	line("S2b.content_check_file_off_0x5000_at_addr+0x1000", r != 0 && got == 0xC0DE5000, fmt.Sprintf("read=%#x (%s)", got, lasterr(r, e)))
	// write from child side via WriteProcessMemory, observe in parent view
	v := uint64(0xBEEF)
	r, _, e = pWPM.Call(uintptr(h), addr+0x2000, uintptr(unsafe.Pointer(&v)), 8, uintptr(unsafe.Pointer(&n)))
	line("S2b.WriteProcessMemory_into_view_visible_in_parent", r != 0 && *(*uint64)(unsafe.Pointer(pv + 0x6000)) == 0xBEEF, lasterr(r, e))
	// protection per 4K on a view at unaligned addr
	var old uint32
	r, _, e = pVirtualProtectEx.Call(uintptr(h), addr+0x3000, 0x1000, PAGE_NOACCESS, uintptr(unsafe.Pointer(&old)))
	line("S2b.VirtualProtectEx_4K_in_view_at_4K_aligned_addr", r != 0, lasterr(r, e))
	// adjacent 4K views with different file offsets, contiguous, 4K-aligned
	b := reg + 0x100000 + 0x3000
	split(h, reg+0x100000, 0x3000)
	split(h, b, 0x1000)
	split(h, b+0x1000, 0x1000)
	a1, m1 := mapview(h, S, b, 0x10000, 0x1000, MEM_REPLACE_PLACEHOLDER, PAGE_READWRITE)
	a2, m2 := mapview(h, S, b+0x1000, 0x1000, 0x1000, MEM_REPLACE_PLACEHOLDER, PAGE_READWRITE)
	var g1, g2 uint64
	pRPM.Call(uintptr(h), b, uintptr(unsafe.Pointer(&g1)), 8, uintptr(unsafe.Pointer(&n)))
	pRPM.Call(uintptr(h), b+0x1000, uintptr(unsafe.Pointer(&g2)), 8, uintptr(unsafe.Pointer(&n)))
	line("S2b.two_adjacent_4K_views_different_file_offsets", a1 != 0 && a2 != 0 && g1 == 0xC0DF0000 && g2 == 0xC0DE1000, fmt.Sprintf("%s/%s g1=%#x g2=%#x", m1, m2, g1, g2))
	// unmap a single 4K view back to placeholder, then reuse
	s := unmap(h, b, true)
	line("S2b.UnmapViewOfFile2_preserve_placeholder_4K", s == "ok", s)
}
