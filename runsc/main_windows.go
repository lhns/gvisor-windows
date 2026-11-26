// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

// Binary runsc on Windows: see package winrunsc.
package main

import "gvisor.dev/gvisor/runsc/winrunsc"

func main() {
	winrunsc.Main()
}
