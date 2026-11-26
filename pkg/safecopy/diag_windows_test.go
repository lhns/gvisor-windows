// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows && amd64

package safecopy

import (
	"fmt"
	"testing"
	"unsafe"
)

// TestWindowsMemclrDiag prints what memclr reports for faults at various
// sizes and offsets, with and without REP STOSB, so that a misbehaving case
// can be told apart from the others in CI logs. It never fails.
func TestWindowsMemclrDiag(t *testing.T) {
	saved := hasERMS
	defer func() { hasERMS = saved }()
	t.Logf("hasERMS initially %v", saved)
	for _, erms := range []bool{false, true} {
		hasERMS = erms
		for _, size := range []int{1, 15, 16, 17, 255, 256, 257, 2047, 2048, 4096, 8192} {
			for _, before := range []int{0, 1, 16, 100} {
				m := mapNoAccessTail(t, 3*pageSize, 2*pageSize)
				start := 2*pageSize - before
				fmt.Printf("DIAG erms=%v size=%d before=%d: start\n", erms, size, before)
				fault, sig := memclr(uintptr(unsafe.Pointer(&m[start])), uintptr(size), true)
				want := uintptr(unsafe.Pointer(&m[2*pageSize]))
				ok := sig == 11 && fault == want
				if size <= before {
					ok = sig == 0
				}
				fmt.Printf("DIAG erms=%v size=%d before=%d: fault-want=%d sig=%d ok=%v\n", erms, size, before, int64(fault)-int64(want), sig, ok)
			}
		}
	}
}
