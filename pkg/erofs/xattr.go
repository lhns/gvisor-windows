// gvisor-windows: extended attributes of image inodes (security.capability,
// SELinux labels, user.*), read from the inode's xattr area and the shared
// xattr area of the image.

package erofs

import (
	"encoding/binary"
	"fmt"

	"gvisor.dev/gvisor/pkg/errors/linuxerr"
)

// Xattr is one extended attribute of an inode.
type Xattr struct {
	Name  string
	Value []byte
}

const (
	xattrIbodyHeaderSize = 12
	xattrEntryHeaderSize = 4
	xattrLongPrefixFlag  = 0x80
)

// xattrPrefixes maps the name index of an entry to the prefix of its name.
var xattrPrefixes = map[uint8]string{
	1: "user.",
	2: "system.posix_acl_access",
	3: "system.posix_acl_default",
	4: "trusted.",
	6: "security.",
}

// xattrIsize returns the size of the inline xattr area of an inode with the
// given xattr count (the on-disk i_xattr_icount).
func xattrIsize(count uint16) uint64 {
	if count == 0 {
		return 0
	}
	return xattrIbodyHeaderSize + uint64(count-1)*4
}

// Xattrs returns the extended attributes of the inode. Entries with a long
// name prefix (not produced by default) are skipped.
func (i *Inode) Xattrs() ([]Xattr, error) {
	if i.xattrSize == 0 {
		return nil, nil
	}
	img := i.image
	hdr, err := img.BytesAt(i.xattrOff, xattrIbodyHeaderSize)
	if err != nil {
		return nil, err
	}
	shared := int(hdr[4])
	if uint64(xattrIbodyHeaderSize+4*shared) > i.xattrSize {
		return nil, linuxerr.EUCLEAN
	}
	var out []Xattr
	for k := 0; k < shared; k++ {
		idb, err := img.BytesAt(i.xattrOff+xattrIbodyHeaderSize+uint64(4*k), 4)
		if err != nil {
			return nil, err
		}
		id := uint64(binary.LittleEndian.Uint32(idb))
		off := uint64(img.sb.XattrBlockAddr)<<img.sb.BlockSizeBits + id*4
		x, _, err := img.xattrEntryAt(off)
		if err != nil {
			return nil, err
		}
		if x != nil {
			out = append(out, *x)
		}
	}
	off := i.xattrOff + xattrIbodyHeaderSize + uint64(4*shared)
	end := i.xattrOff + i.xattrSize
	for off+xattrEntryHeaderSize <= end {
		x, n, err := img.xattrEntryAt(off)
		if err != nil {
			return nil, err
		}
		if x != nil {
			out = append(out, *x)
		}
		off += n
	}
	return out, nil
}

// GetXattr returns the value of one extended attribute.
func (i *Inode) GetXattr(name string) ([]byte, error) {
	xs, err := i.Xattrs()
	if err != nil {
		return nil, err
	}
	for _, x := range xs {
		if x.Name == name {
			return x.Value, nil
		}
	}
	return nil, linuxerr.ENODATA
}

// xattrEntryAt parses the entry at off. It returns the entry (nil if its name
// uses a prefix this implementation does not know) and the entry's size in
// the image, rounded to 4 bytes.
func (i *Image) xattrEntryAt(off uint64) (*Xattr, uint64, error) {
	h, err := i.BytesAt(off, xattrEntryHeaderSize)
	if err != nil {
		return nil, 0, err
	}
	nameLen := uint64(h[0])
	index := h[1]
	valLen := uint64(binary.LittleEndian.Uint16(h[2:4]))
	body, err := i.BytesAt(off+xattrEntryHeaderSize, nameLen+valLen)
	if err != nil {
		return nil, 0, err
	}
	size := (xattrEntryHeaderSize + nameLen + valLen + 3) &^ 3
	if index&xattrLongPrefixFlag != 0 {
		return nil, size, nil
	}
	prefix, ok := xattrPrefixes[index]
	if !ok {
		return nil, size, nil
	}
	return &Xattr{
		Name:  prefix + string(body[:nameLen]),
		Value: append([]byte(nil), body[nameLen:]...),
	}, size, nil
}

var _ = fmt.Sprint
