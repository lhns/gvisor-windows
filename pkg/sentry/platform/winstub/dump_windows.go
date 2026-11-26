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
	for as := range live {
		fmt.Fprintf(w, "winstub: address space pid %d dead=%v\n", as.wpid, as.dead.Load())
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
			fmt.Fprintf(w, "  thread %d tid %d: state %d ctlSleep %d stubSleep %d mode %d parked %v lastKind %d req %+v spins %d rip %#x exit %d suspendcount %d replies %d setevents %d stubwaits %d\n",
				i, t.tid, t.state(), *t.u32(stubasm.PgCtlSlp), *t.u32(stubasm.PgStubSlp), *t.u32(stubasm.PgMode), t.parked, t.lastKind, t.req, *t.u64(stubasm.PgSpins), rip, code, prevSuspend, t.nReply.Load(), t.nSet.Load(), *t.u32(stubasm.PgWaits))
			t.mu.Unlock()
		}
		as.tmu.Unlock()
	}
}
