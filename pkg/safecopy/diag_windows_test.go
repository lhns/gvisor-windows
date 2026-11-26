// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows && amd64

package safecopy

import (
	"fmt"
	"testing"
	"unsafe"
)

// TestWindowsMemclrDiag prints what the fault paths report, from the most
// basic to the most special, so that a misbehaving case can be told apart in
// CI logs. It never fails; it may crash, in which case the last line tells
// which case did.
func TestWindowsMemclrDiag(t *testing.T) {
	saved := hasERMS
	defer func() { hasERMS = saved }()
	fmt.Printf("DIAG hasERMS initially %v\n", saved)
	m := mapNoAccessTail(t, 3*pageSize, 2*pageSize)
	bad := uintptr(unsafe.Pointer(&m[2*pageSize]))
	src := make([]byte, 64)

	fmt.Println("DIAG A: memcpy write fault, size 1, via CopyOut")
	n, err := CopyOut(unsafe.Pointer(bad), src[:1])
	fmt.Printf("DIAG A: n=%d err=%v\n", n, err)

	fmt.Println("DIAG B: ZeroOut size 1 at the fault")
	zn, err := ZeroOut(unsafe.Pointer(bad), 1)
	fmt.Printf("DIAG B: n=%d err=%v\n", zn, err)

	fmt.Println("DIAG C: memclr size 1 directly, after ensureFaultStack")
	ensureFaultStack()
	fault, sig := memclr(bad, 1, true)
	fmt.Printf("DIAG C: fault-bad=%d sig=%d\n", int64(fault)-int64(bad), sig)

	for _, erms := range []bool{false, true} {
		hasERMS = erms
		for _, size := range []int{1, 15, 16, 17, 255, 256, 257, 2047, 2048, 4096, 8192} {
			for _, before := range []int{0, 1, 16, 100} {
				start := 2*pageSize - before
				fmt.Printf("DIAG D erms=%v size=%d before=%d: start\n", erms, size, before)
				zn, err := ZeroOut(unsafe.Pointer(&m[start]), uintptr(size))
				wantN := uintptr(min(size, before))
				ok := zn == wantN && (size <= before && err == nil || size > before && err == (SegvError{bad}))
				fmt.Printf("DIAG D erms=%v size=%d before=%d: n=%d err=%v ok=%v\n", erms, size, before, zn, err, ok)
			}
		}
	}
}
