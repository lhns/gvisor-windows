// Package winhost serves Linux file semantics from a Windows (NTFS) folder.
//
// It is the host side of the Windows backend of the gofer filesystem (ADR
// 0005, 0007): files are opened by the Sentry itself, with no gofer process and
// no file protocol. Linux metadata that NTFS cannot hold (owner, mode, device
// nodes, emulated symlinks, extended attributes) lives in an NTFS alternate
// data stream of each file, encoded by this file. The package has no Sentry
// dependencies so it can be tested alone.
package winhost

import (
	"encoding/binary"
	"errors"
	"sort"
)

// MetaStream is the name of the alternate data stream holding Metadata.
const MetaStream = "gvisor.meta"

const metaMagic = "GVM1"

// Linux file type bits (S_IFMT and friends), repeated here to keep the package
// free of dependencies.
const (
	SIFMT   = 0o170000
	SIFSOCK = 0o140000
	SIFLNK  = 0o120000
	SIFREG  = 0o100000
	SIFBLK  = 0o060000
	SIFDIR  = 0o040000
	SIFCHR  = 0o020000
	SIFIFO  = 0o010000
)

// Metadata is the content of the metadata stream. A file without the stream
// has the mount's defaults.
type Metadata struct {
	// Mode is the full Linux st_mode, file type included.
	Mode uint32
	UID  uint32
	GID  uint32
	// RdevMajor and RdevMinor are set for character and block devices.
	RdevMajor uint32
	RdevMinor uint32
	// Target is the target of an emulated symlink (Mode is S_IFLNK).
	Target string
	// Xattrs are the extended attributes, by full name ("user.x").
	Xattrs map[string][]byte
}

// Format of the stream (little endian):
//
//	"GVM1"
//	u32 mode, u32 uid, u32 gid, u32 rdevMajor, u32 rdevMinor
//	u32 len, target bytes
//	u32 count, then count times: u16 nameLen, name, u32 valueLen, value
//
// Xattrs are written sorted by name so equal metadata encodes equally.

// Encode returns the stream content.
func (m *Metadata) Encode() []byte {
	b := make([]byte, 0, 64)
	b = append(b, metaMagic...)
	for _, v := range []uint32{m.Mode, m.UID, m.GID, m.RdevMajor, m.RdevMinor, uint32(len(m.Target))} {
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	b = append(b, m.Target...)
	names := make([]string, 0, len(m.Xattrs))
	for n := range m.Xattrs {
		names = append(names, n)
	}
	sort.Strings(names)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(names)))
	for _, n := range names {
		b = binary.LittleEndian.AppendUint16(b, uint16(len(n)))
		b = append(b, n...)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(m.Xattrs[n])))
		b = append(b, m.Xattrs[n]...)
	}
	return b
}

// ErrBadMetadata reports a metadata stream that cannot be decoded.
var ErrBadMetadata = errors.New("winhost: malformed metadata stream")

// DecodeMetadata parses the content of the stream.
func DecodeMetadata(b []byte) (*Metadata, error) {
	if len(b) < len(metaMagic) || string(b[:len(metaMagic)]) != metaMagic {
		return nil, ErrBadMetadata
	}
	b = b[len(metaMagic):]
	u32 := func() (uint32, bool) {
		if len(b) < 4 {
			return 0, false
		}
		v := binary.LittleEndian.Uint32(b)
		b = b[4:]
		return v, true
	}
	var hdr [6]uint32
	for i := range hdr {
		v, ok := u32()
		if !ok {
			return nil, ErrBadMetadata
		}
		hdr[i] = v
	}
	m := &Metadata{Mode: hdr[0], UID: hdr[1], GID: hdr[2], RdevMajor: hdr[3], RdevMinor: hdr[4]}
	if uint64(hdr[5]) > uint64(len(b)) {
		return nil, ErrBadMetadata
	}
	m.Target = string(b[:hdr[5]])
	b = b[hdr[5]:]
	n, ok := u32()
	if !ok {
		return nil, ErrBadMetadata
	}
	if n > 0 {
		m.Xattrs = make(map[string][]byte, n)
	}
	for ; n > 0; n-- {
		if len(b) < 2 {
			return nil, ErrBadMetadata
		}
		nl := int(binary.LittleEndian.Uint16(b))
		b = b[2:]
		if len(b) < nl {
			return nil, ErrBadMetadata
		}
		name := string(b[:nl])
		b = b[nl:]
		vl, ok := u32()
		if !ok || uint64(vl) > uint64(len(b)) {
			return nil, ErrBadMetadata
		}
		m.Xattrs[name] = append([]byte(nil), b[:vl]...)
		b = b[vl:]
	}
	return m, nil
}

// Name escaping. Linux names may hold characters that NTFS reserves: ':'
// would address an alternate stream (and so our metadata), '\' separates path
// components, and < > " | ? * and control characters are invalid. They are
// mapped to U+F000+c, the private-use range WSL and Cygwin use, in both
// directions. A guest name that itself holds a character of U+F000..U+F0FF
// is not distinguishable from an escaped one (documented limit).

func needsEscape(r rune) bool {
	switch r {
	case ':', 92, '<', '>', '"', '|', '?', '*':
		return true
	}
	return r < 0x20
}

// EscapeName maps a Linux file name to its NTFS form.
func EscapeName(s string) string {
	clean := true
	for _, r := range s {
		if needsEscape(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if needsEscape(r) {
			r += 0xF000
		}
		out = append(out, r)
	}
	return string(out)
}

// UnescapeName is the inverse of EscapeName.
func UnescapeName(s string) string {
	clean := true
	for _, r := range s {
		if r >= 0xF000 && r < 0xF100 && needsEscape(r-0xF000) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= 0xF000 && r < 0xF100 && needsEscape(r-0xF000) {
			r -= 0xF000
		}
		out = append(out, r)
	}
	return string(out)
}
