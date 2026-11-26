// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package boot

import "os"

// createMemoryFileBacking returns nil: on Windows pgalloc backs the
// MemoryFile with sections it creates itself.
func createMemoryFileBacking(name string) (*os.File, error) { return nil, nil }
