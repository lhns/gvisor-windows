// gvisor-windows: Linux ABI shim, see docs/adr/0006. Generated from pagesize_unix.go by windowsshim_gen.

//go:build windows && amd64

// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.


// For Unix, get the pagesize from the runtime.

package unix

import "syscall"

func Getpagesize() int {
	return syscall.Getpagesize()
}
