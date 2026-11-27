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
	"fmt"
	"io"
	"sync"
	"time"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
)

var (
	liveMu sync.Mutex
	live   = map[*addressSpace]struct{}{}
)

func registerAS(as *addressSpace) {
	liveMu.Lock()
	live[as] = struct{}{}
	liveMu.Unlock()
}

func unregisterAS(as *addressSpace) {
	liveMu.Lock()
	delete(live, as)
	liveMu.Unlock()
}

// DumpState writes the state of every stub thread of every live address space:
// for diagnosing a hang (a thread waiting for a reply that never comes, or the
// other way round).
func DumpState(w io.Writer) {
	liveMu.Lock()
	defer liveMu.Unlock()
	// CPU cycles each thread used in 300 ms: which threads are running at all.
	cycles := func() map[*thread]uint64 {
		m := map[*thread]uint64{}
		for as := range live {
			as.tmu.Lock()
			for _, t := range as.slots {
				if t != nil && t.started && t.hThread != 0 {
					var c uint64
					pQueryThreadCycle.Call(t.hThread, uintptr(unsafe.Pointer(&c)))
					m[t] = c
				}
			}
			as.tmu.Unlock()
		}
		return m
	}
	c0 := cycles()
	time.Sleep(300 * time.Millisecond)
	c1 := cycles()
	for as := range live {
		fmt.Fprintf(w, "winstub: address space pid %d dead=%v last debug event: %v; TerminateThread fallbacks: %d\n", as.wpid, as.dead.Load(), as.lastEvent.Load(), as.nTermFallback.Load())
		fmt.Fprintf(w, "  counts: debug events %d, %%fs run by the debugger %d, stops moved out of veneers %d, replaced threads %d\n", as.nEvents.Load(), as.nFSInline.Load(), as.nFSFix.Load(), as.nThreadReplaced.Load())
		as.tmu.Lock()
		for i, t := range as.slots {
			if t == nil || !t.started {
				continue
			}
			t.mu.Lock()
			// Where is the thread? Suspend it for a moment (diagnostics only).
			var rip uint64
			var code uint32
			prevSuspend := int64(-1)
			pGetExitCode.Call(t.hThread, uintptr(unsafe.Pointer(&code)))
			if r, _, _ := pSuspendThread.Call(t.hThread); int32(r) != -1 {
				prevSuspend = int64(r)
				var ctx ctxBuf
				if ctx.get(t.hThread, ctxControl) {
					rip = *ctx.u64(ctxRipOff)
				}
				pResumeThread.Call(t.hThread)
			}
			fmt.Fprintf(w, "  thread %d tid %d: state %d ctlSleep %d stubSleep %d mode %d parked %v lastKind %d req %+v spins %d rip %#x (%s) exit %d suspendcount %d cycles300ms %d replies %d setevents %d (failed %d, error %d) stubwaits %d\n",
				i, t.tid, t.state(), *t.u32(stubasm.PgCtlSlp), *t.u32(stubasm.PgStubSlp), *t.u32(stubasm.PgMode), t.parked, t.lastKind, t.req, *t.u64(stubasm.PgSpins), rip, ntdllSymbol(rip), code, prevSuspend, c1[t]-c0[t], t.nReply.Load(), t.nSet.Load(), t.nSetFail.Load(), t.lastSetErr.Load(), *t.u32(stubasm.PgWaits))
			t.mu.Unlock()
		}
		as.tmu.Unlock()
	}
}

// ntdllSymbol names the ntdll export at or below addr (ntdll is mapped at the
// same address in every process), for reading DumpState.
func ntdllSymbol(addr uint64) string {
	ntd.Load()
	base := ntd.Handle()
	if base == 0 || addr < uint64(base) {
		return "?"
	}
	u32 := func(off uintptr) uint32 { return *(*uint32)(ptr(base + off)) }
	nt := uintptr(u32(0x3C))
	exp := uintptr(u32(nt + 0x18 + 0x70)) // optional header data directory 0
	if exp == 0 {
		return "?"
	}
	n := int(u32(exp + 0x18))
	funcs, names, ords := uintptr(u32(exp+0x1C)), uintptr(u32(exp+0x20)), uintptr(u32(exp+0x24))
	rva := uint32(addr - uint64(base))
	best, bestName := uint32(0), "?"
	for i := 0; i < n; i++ {
		ord := uintptr(*(*uint16)(ptr(base + ords + 2*uintptr(i))))
		f := u32(funcs + 4*ord)
		if f <= rva && f > best {
			p := base + uintptr(u32(names+4*uintptr(i)))
			var b []byte
			for ; *(*byte)(ptr(p)) != 0; p++ {
				b = append(b, *(*byte)(ptr(p)))
			}
			best, bestName = f, string(b)
		}
	}
	return fmt.Sprintf("%s+%#x", bestName, rva-best)
}
