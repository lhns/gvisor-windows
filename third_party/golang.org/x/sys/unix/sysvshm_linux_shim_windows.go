// gvisor-windows: Linux ABI shim, see docs/adr/0006. Generated from sysvshm_linux.go by windowsshim_gen.

//go:build windows && amd64

// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.


package unix

import "runtime"

// SysvShmCtl performs control operations on the shared memory segment
// specified by id.
func SysvShmCtl(id, cmd int, desc *SysvShmDesc) (result int, err error) {
	if runtime.GOARCH == "arm" ||
		runtime.GOARCH == "mips64" || runtime.GOARCH == "mips64le" {
		cmd |= ipc_64
	}

	return shmctl(id, cmd, desc)
}
