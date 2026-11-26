// gvisor-windows: Linux ABI shim, see docs/adr/0006. Generated from race0.go by windowsshim_gen.

//go:build windows && amd64

// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.


package unix

import (
	"unsafe"
)

const raceenabled = false

func raceAcquire(addr unsafe.Pointer) {
}

func raceReleaseMerge(addr unsafe.Pointer) {
}

func raceReadRange(addr unsafe.Pointer, len int) {
}

func raceWriteRange(addr unsafe.Pointer, len int) {
}
