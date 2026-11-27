// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command fsscan counts the %fs-prefixed instructions of ELF binaries by how the
// winstub platform's rewrite mode (ADR 0020) can patch them: Direct (the
// instruction is long enough for a jump), Window (a window of neighbouring
// instructions is), None (it falls back to the debugger, ~100 us per execution).
// "near" assumes the veneers are within rel32 reach (5-byte jump), "far" needs a
// 7-byte jump (code above 2 GiB, e.g. shared libraries).
//
//	fsscan file...
package main

import (
	"debug/elf"
	"fmt"
	"os"

	"gvisor.dev/gvisor/pkg/sentry/platform/winstub"
)

func main() {
	var tot winstub.FSReport
	var textBytes int
	fmt.Printf("%-44s %9s %7s | %6s %7s %6s | %6s %7s %6s | %6s %5s\n", "file", "text", "%fs", "near-D", "near-W", "near-N", "far-D", "far-W", "far-N", "unsupp", "rip")
	for _, f := range os.Args[1:] {
		r, n, err := scanFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", f, err)
			continue
		}
		line(f, r, n)
		tot.Total += r.Total
		tot.Unsupp += r.Unsupp
		tot.Rip += r.Rip
		tot.Near.Direct += r.Near.Direct
		tot.Near.Window += r.Near.Window
		tot.Near.None += r.Near.None
		tot.Far.Direct += r.Far.Direct
		tot.Far.Window += r.Far.Window
		tot.Far.None += r.Far.None
		textBytes += n
	}
	line("TOTAL", tot, textBytes)
}

func line(name string, r winstub.FSReport, text int) {
	if len(name) > 44 {
		name = "..." + name[len(name)-41:]
	}
	fmt.Printf("%-44s %9d %7d | %6d %7d %6d | %6d %7d %6d | %6d %5d\n", name, text, r.Total,
		r.Near.Direct, r.Near.Window, r.Near.None, r.Far.Direct, r.Far.Window, r.Far.None, r.Unsupp, r.Rip)
}

// scanFile scans the executable segments of an ELF file.
func scanFile(path string) (winstub.FSReport, int, error) {
	var rep winstub.FSReport
	f, err := elf.Open(path)
	if err != nil {
		return rep, 0, err
	}
	defer f.Close()
	n := 0
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || p.Flags&elf.PF_X == 0 {
			continue
		}
		b := make([]byte, p.Filesz)
		if _, err := p.ReadAt(b, 0); err != nil {
			return rep, n, err
		}
		r := winstub.ScanFS(b)
		rep.Total += r.Total
		rep.Unsupp += r.Unsupp
		rep.Rip += r.Rip
		rep.Near.Direct += r.Near.Direct
		rep.Near.Window += r.Near.Window
		rep.Near.None += r.Near.None
		rep.Far.Direct += r.Far.Direct
		rep.Far.Window += r.Far.Window
		rep.Far.None += r.Far.None
		n += len(b)
	}
	return rep, n, nil
}
