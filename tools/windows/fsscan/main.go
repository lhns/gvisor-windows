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
//	fsscan -targets file...
//	fsscan -audit file objdump-starts.txt objdump-fs.txt
//
// -audit compares the decoder's instruction boundaries and %fs instructions with
// those of objdump (the files hold one hex address per line).
//
// With -targets it looks for jump tables in the data of each file (runs of 8-byte,
// absolute 4-byte or table-relative 4-byte entries that point at instruction starts)
// and counts the %fs windows whose inside is the target of one: a jump there lands in
// the middle of a patch window, which the scan of the text cannot see.
package main

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"gvisor.dev/gvisor/pkg/sentry/platform/winstub"
)

func main() {
	if len(os.Args) == 5 && os.Args[1] == "-audit" {
		if err := audit(os.Args[2], os.Args[3], os.Args[4]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "-targets" {
		for _, f := range os.Args[2:] {
			if err := targets(f); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", f, err)
			}
		}
		return
	}
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

// targets reports, for one ELF file, the %fs windows (near: 5-byte jump, far:
// 7-byte jump) whose interior instruction starts are jump table targets.
func targets(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	type seg struct {
		v uint64
		b []byte
	}
	var text, data []seg
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		b := make([]byte, p.Filesz)
		if _, err := p.ReadAt(b, 0); err != nil {
			return err
		}
		if p.Flags&elf.PF_X != 0 {
			text = append(text, seg{p.Vaddr, b})
		} else {
			data = append(data, seg{p.Vaddr, b})
		}
	}
	isStart := map[uint64]bool{}
	var lo, hi uint64 = ^uint64(0), 0
	type win struct {
		w    winstub.FSWindow
		base uint64
	}
	var wins [2][]win
	for _, t := range text {
		lo, hi = min(lo, t.v), max(hi, t.v+uint64(len(t.b)))
		for j, minLen := range []int{5, 7} {
			ws, starts := winstub.FSWindows(t.b, minLen)
			if j == 0 {
				for _, o := range starts {
					isStart[t.v+uint64(o)] = true
				}
			}
			for _, w := range ws {
				wins[j] = append(wins[j], win{w, t.v})
			}
		}
	}
	tgt := map[uint64]bool{}
	in := func(a uint64) bool { return a >= lo && a < hi && isStart[a] }
	for _, d := range data {
		b := d.b
		for q := 0; q+8 <= len(b); q += 8 {
			if v := binary.LittleEndian.Uint64(b[q:]); in(v) {
				tgt[v] = true
			}
		}
		for q := 0; q+12 <= len(b); q += 4 {
			// absolute 32-bit entries
			if in(uint64(binary.LittleEndian.Uint32(b[q:]))) && in(uint64(binary.LittleEndian.Uint32(b[q+4:]))) && in(uint64(binary.LittleEndian.Uint32(b[q+8:]))) {
				for k := q; k+4 <= len(b); k += 4 {
					v := uint64(binary.LittleEndian.Uint32(b[k:]))
					if !in(v) {
						break
					}
					tgt[v] = true
				}
			}
			// entries relative to the start of the table
			base := d.v + uint64(q)
			ok := true
			for k := 0; k < 3 && ok; k++ {
				ok = in(base + uint64(int64(int32(binary.LittleEndian.Uint32(b[q+4*k:])))))
			}
			if ok {
				for k := q; k+4 <= len(b); k += 4 {
					v := base + uint64(int64(int32(binary.LittleEndian.Uint32(b[k:]))))
					if !in(v) {
						break
					}
					tgt[v] = true
				}
			}
		}
	}
	for j, name := range []string{"near (5-byte jump)", "far (7-byte jump)"} {
		hits, inside := 0, 0
		for _, w := range wins[j] {
			hit := false
			for _, o := range w.w.Insn[1:] {
				a := w.base + uint64(o)
				if tgt[a] {
					hit = true
					if hits < 8 {
						fmt.Printf("  %s: window %#x-%#x entered at %#x\n", name, w.base+uint64(w.w.Lo), w.base+uint64(w.w.Hi), a)
					}
				}
			}
			if hit {
				hits++
			}
			if len(w.w.Insn) > 1 {
				inside++
			}
		}
		fmt.Printf("%s: %s: %d windows, %d with a jump table target in the middle (%d jump table targets in all)\n", path, name, len(wins[j]), hits, len(tgt))
	}
	return nil
}

// audit compares the decoder with objdump for the executable segments of one file.
func audit(path, startsFile, fsFile string) error {
	f, err := elf.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	read := func(name string) (map[uint64]bool, error) {
		b, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		m := map[uint64]bool{}
		for _, l := range strings.Fields(string(b)) {
			v, err := strconv.ParseUint(strings.TrimPrefix(l, "0x"), 16, 64)
			if err == nil {
				m[v] = true
			}
		}
		return m, nil
	}
	objStarts, err := read(startsFile)
	if err != nil {
		return err
	}
	objFS, err := read(fsFile)
	if err != nil {
		return err
	}
	ourStarts, ourFS := map[uint64]bool{}, map[uint64]bool{}
	var texts []struct {
		v uint64
		b []byte
	}
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || p.Flags&elf.PF_X == 0 {
			continue
		}
		b := make([]byte, p.Filesz)
		if _, err := p.ReadAt(b, 0); err != nil {
			return err
		}
		texts = append(texts, struct {
			v uint64
			b []byte
		}{p.Vaddr, b})
		st, fs := winstub.Decoded(b)
		for _, o := range st {
			ourStarts[p.Vaddr+uint64(o)] = true
		}
		for _, o := range fs {
			ourFS[p.Vaddr+uint64(o)] = true
		}
	}
	bytesAt := func(a uint64) string {
		for _, t := range texts {
			if a >= t.v && a < t.v+uint64(len(t.b)) {
				o := a - t.v
				return fmt.Sprintf("% x", t.b[o:min(uint64(len(t.b)), o+16)])
			}
		}
		return ""
	}
	diff := func(name string, ours, theirs map[uint64]bool) {
		var only []uint64
		for a := range theirs {
			if !ours[a] {
				only = append(only, a)
			}
		}
		var bogus []uint64
		for a := range ours {
			if !theirs[a] {
				bogus = append(bogus, a)
			}
		}
		sort.Slice(only, func(i, j int) bool { return only[i] < only[j] })
		sort.Slice(bogus, func(i, j int) bool { return bogus[i] < bogus[j] })
		fmt.Printf("%s: %s: decoder %d, objdump %d; only objdump (missed) %d, only decoder (bogus) %d\n", path, name, len(ours), len(theirs), len(only), len(bogus))
		for i, a := range only {
			if i >= 12 {
				break
			}
			fmt.Printf("  missed %#x: %s\n", a, bytesAt(a))
		}
		for i, a := range bogus {
			if i >= 12 {
				break
			}
			fmt.Printf("  bogus  %#x: %s\n", a, bytesAt(a))
		}
	}
	diff("instruction starts", ourStarts, objStarts)
	diff("%fs instructions", ourFS, objFS)
	return nil
}
