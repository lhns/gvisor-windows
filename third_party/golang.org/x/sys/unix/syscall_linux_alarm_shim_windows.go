// gvisor-windows: Linux ABI shim, see docs/adr/0006. Generated from syscall_linux_alarm.go by windowsshim_gen.

//go:build windows && amd64

// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.


package unix

// SYS_ALARM is not defined on arm or riscv, but is available for other GOARCH
// values.

//sys	Alarm(seconds uint) (remaining uint, err error)
