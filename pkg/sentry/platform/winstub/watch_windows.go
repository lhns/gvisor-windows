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

//go:build windows && amd64

package winstub

import (
	"encoding/binary"
	"log"
	"os"
)

// Hardware write watchpoints for guest diagnostics (WINSTUB_WATCH=1): a guest
// syscall with number watchSyscall and the address in rdi sets DR0 of the
// calling thread to an 8-byte write watch there; every hit is logged with the
// writing instruction and the registers, and the thread continues.
var watchOn = os.Getenv("WINSTUB_WATCH") != ""

const (
	watchSyscall  = 0x7ffe
	ctxDebugRegs  = 0x100010
	ctxDr0Off     = 0x48
	ctxDr6Off     = 0x68
	ctxDr7Off     = 0x70
	watchMaxLines = 400
)

// watchSet arms DR0 of the parked thread t on [addr, addr+8).
func (as *addressSpace) watchSet(t *thread, addr uint64) {
	c := newCtxBuf()
	if r, _, _ := pSuspendThread.Call(t.hThread); int32(r) == -1 {
		return
	}
	defer pResumeThread.Call(t.hThread)
	if !c.get(t.hThread, ctxDebugRegs) {
		log.Printf("winstub: WATCH: cannot read the debug registers of thread %d", t.idx)
		return
	}
	*c.u64(ctxDr0Off) = addr &^ 7
	*c.u64(ctxDr6Off) = 0
	*c.u64(ctxDr7Off) = 1 | 1<<16 | 2<<18
	ok := c.set(t.hThread)
	v, _ := as.readStub(addr&^7, 8)
	log.Printf("winstub: WATCH armed at %#x (stub pid %d thread %d, ok %v): % x", addr&^7, as.wpid, t.idx, ok, v)
}

// watchHit handles a single-step exception of t: a hit of the watchpoint is
// logged and the thread continues. It reports false for any other single step.
func (as *addressSpace) watchHit(t *thread, ctx *ctxBuf, excAddr uint64) bool {
	c := newCtxBuf()
	if !c.get(t.hThread, ctxDebugRegs) {
		return false
	}
	dr6 := *c.u64(ctxDr6Off)
	if dr6&1 == 0 {
		return false
	}
	*c.u64(ctxDr6Off) = 0
	c.set(t.hThread)
	if n := as.nWatch.Add(1); n <= watchMaxLines {
		a := *c.u64(ctxDr0Off)
		v, _ := as.readStub(a, 8)
		code, _ := as.readStub(excAddr-16, 16)
		var regs [16]uint64
		for i := range regs {
			regs[i] = *ctx.u64(ctxRegsOff + uintptr(8*i))
		}
		log.Printf("winstub: WATCH hit %d at %#x now %#x (stub pid %d thread %d) after rip %#x, code[-16:] % x, regs %x",
			n, a, binary.LittleEndian.Uint64(append(v, make([]byte, 8)...)), as.wpid, t.idx, excAddr, code, regs)
	}
	return true
}
