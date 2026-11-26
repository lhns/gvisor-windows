// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package pgalloc

func mincore(s []byte, buf []byte) error {
	return hostResident(s, buf)
}
