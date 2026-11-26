// Package pe writes the minimal stub executable: one executable section, no imports,
// no CRT. Its entry point only jumps to the stub's thread-entry routine with RCX set
// to the main thread's shared page.
package pe

import "encoding/binary"

// ImageBase is above the guest range (stubasm.GuestMax).
const ImageBase uint64 = 0x7100_0000_0000

// Stub returns a PE32+ image whose entry code is `movabs rcx,page; movabs rax,entry; jmp rax`.
func Stub(page, entry uint64) []byte {
	code := []byte{0x48, 0xB9}
	code = binary.LittleEndian.AppendUint64(code, page)
	code = append(code, 0x48, 0xB8)
	code = binary.LittleEndian.AppendUint64(code, entry)
	code = append(code, 0xFF, 0xE0)

	const hdrSize = 0x200
	b := make([]byte, hdrSize+0x200)
	le := binary.LittleEndian
	copy(b, "MZ")
	le.PutUint32(b[0x3C:], 0x40)
	copy(b[0x40:], "PE\x00\x00")
	c := b[0x44:]
	le.PutUint16(c[0:], 0x8664)  // AMD64
	le.PutUint16(c[2:], 1)       // sections
	le.PutUint16(c[16:], 0xF0)   // optional header size
	le.PutUint16(c[18:], 0x0023) // executable, relocs stripped, large address aware
	o := b[0x58:]
	le.PutUint16(o[0:], 0x20B)
	le.PutUint32(o[4:], 0x200)   // SizeOfCode
	le.PutUint32(o[16:], 0x1000) // entry RVA
	le.PutUint32(o[20:], 0x1000) // BaseOfCode
	le.PutUint64(o[24:], ImageBase)
	le.PutUint32(o[32:], 0x1000) // section alignment
	le.PutUint32(o[36:], 0x200)  // file alignment
	le.PutUint16(o[40:], 6)      // OS version
	le.PutUint16(o[48:], 6)      // subsystem version
	le.PutUint32(o[56:], 0x2000) // SizeOfImage
	le.PutUint32(o[60:], hdrSize)
	le.PutUint16(o[68:], 2)       // GUI subsystem: no console host
	le.PutUint16(o[70:], 0x8100)  // NX compatible, terminal server aware
	le.PutUint64(o[72:], 0x40000) // stack reserve
	le.PutUint64(o[80:], 0x4000)  // stack commit
	le.PutUint64(o[88:], 0x100000)
	le.PutUint64(o[96:], 0x1000)
	le.PutUint32(o[108:], 16) // data directories (all empty)
	s := b[0x58+0xF0:]
	copy(s, ".text")
	le.PutUint32(s[8:], uint32(len(code)))
	le.PutUint32(s[12:], 0x1000)
	le.PutUint32(s[16:], 0x200)
	le.PutUint32(s[20:], hdrSize)
	le.PutUint32(s[36:], 0x60000020)
	copy(b[hdrSize:], code)
	return b
}
