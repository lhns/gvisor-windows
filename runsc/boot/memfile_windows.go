// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package boot

import (
	"os"

	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// Memory file options and host rlimits that differ per OS. The Windows values
// are those of tools/windows/sentryguest, the proven configuration.
const (
	memoryFileDelayedEviction = pgalloc.DelayedEvictionManual
	memoryFileNoAccounting    = true
)

var hostLimitResources []int

// createMemoryFileBacking returns nil: on Windows pgalloc backs the
// MemoryFile with sections it creates itself.
func createMemoryFileBacking(name string) (*os.File, error) { return nil, nil }
