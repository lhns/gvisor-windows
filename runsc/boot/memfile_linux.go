// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build linux

package boot

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// Memory file options and host rlimits that differ per OS.
const (
	memoryFileDelayedEviction = pgalloc.DelayedEvictionDefault
	memoryFileNoAccounting    = false
)

var hostLimitResources = []int{unix.RLIMIT_FSIZE, unix.RLIMIT_NOFILE}

// createMemoryFileBacking returns the memfd that backs the MemoryFile.
func createMemoryFileBacking(name string) (*os.File, error) {
	memfd, err := memutil.CreateMemFD(name, 0)
	if err != nil {
		return nil, fmt.Errorf("error creating memfd: %w", err)
	}
	return os.NewFile(uintptr(memfd), name), nil
}
