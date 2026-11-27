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

// Command disasm prints the instructions of an ELF file in [lo,hi) with the
// platform's own decoder (for reading a guest crash on a host without objdump).
package main

import (
	"debug/elf"
	"fmt"
	"os"
	"strconv"

	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/x86asm"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: disasm file lo hi")
		os.Exit(2)
	}
	f, err := elf.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	lo, _ := strconv.ParseUint(os.Args[2], 0, 64)
	hi, _ := strconv.ParseUint(os.Args[3], 0, 64)
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || lo < p.Vaddr || lo >= p.Vaddr+p.Filesz {
			continue
		}
		b := make([]byte, p.Filesz)
		p.ReadAt(b, 0)
		for a := lo; a < hi && a < p.Vaddr+p.Filesz; {
			in, err := x86asm.Decode(b[a-p.Vaddr:], 64)
			if err != nil {
				fmt.Printf("%#x: ?? %02x\n", a, b[a-p.Vaddr])
				a++
				continue
			}
			fmt.Printf("%#x: % x\t%s\n", a, b[a-p.Vaddr:a-p.Vaddr+uint64(in.Len)], x86asm.GNUSyntax(in, a, nil))
			a += uint64(in.Len)
		}
	}
}
