// gvisor-windows: Linux ABI shim, see docs/adr/0006. Generated from fdset.go by windowsshim_gen.

//go:build windows && amd64

// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.


package unix

// Set adds fd to the set fds.
func (fds *FdSet) Set(fd int) {
	fds.Bits[fd/NFDBITS] |= (1 << (uintptr(fd) % NFDBITS))
}

// Clear removes fd from the set fds.
func (fds *FdSet) Clear(fd int) {
	fds.Bits[fd/NFDBITS] &^= (1 << (uintptr(fd) % NFDBITS))
}

// IsSet returns whether fd is in the set fds.
func (fds *FdSet) IsSet(fd int) bool {
	return fds.Bits[fd/NFDBITS]&(1<<(uintptr(fd)%NFDBITS)) != 0
}

// Zero clears the set fds.
func (fds *FdSet) Zero() {
	clear(fds.Bits[:])
}
