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
	"os"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
)

// Stub recycling (ADR 0022). A released address space whose guest threads all end
// themselves is emptied and kept as a spare instead of terminating its process: the
// next NewAddressSpace takes it without creating a process (about 5 ms of process
// creation, loader events and anchor start, twice per fork+exec).
//
// Emptied means: no guest thread, no guest view (only the stub's own), the patch
// state and the history of a new address space, and the guest range holding only
// placeholders. A new generation begins (as.gen): a platform context that still holds
// a thread of the old one leaves it alone (winContext.tgen).

// recycleOn can be turned off (WINSTUB_RECYCLE=0).
var recycleOn = os.Getenv("WINSTUB_RECYCLE") != "0"

// maxRecycled bounds the emptied stubs kept; more are terminated.
const maxRecycled = 4

// release releases as: it is emptied and kept as a spare in the background, or
// terminated.
func (p *Platform) release(as *addressSpace) {
	if !recycleOn || as.dead.Load() || as.released.Load() {
		as.kill()
		return
	}
	p.spareMu.Lock()
	full := p.closed || len(p.recycled) >= maxRecycled
	p.spareMu.Unlock()
	if full {
		as.kill()
		return
	}
	// Emptying takes a few milliseconds (the thread's exit, unmapping, checking
	// the range); the task that released the address space does not wait for it.
	go func() {
		if !p.recycle(as) {
			as.kill()
		}
	}()
}

// recycle empties as and keeps it as a spare. It reports false if as cannot be
// reused; the caller then terminates it.
func (p *Platform) recycle(as *addressSpace) bool {
	t0 := qpc()
	if why := as.reset(); why != "" {
		p.nRecycleFail.Add(1)
		Trace("as: not recycled (%s)", why)
		return false
	}
	p.spareMu.Lock()
	defer p.spareMu.Unlock()
	if p.closed || len(p.recycled) >= maxRecycled {
		return false
	}
	p.recycled = append(p.recycled, as)
	p.nRecycled.Add(1)
	Trace("as: recycled in %.3fms", float64(qpc()-t0)/1e6)
	return true
}

// takeRecycled returns an emptied stub, or nil.
func (p *Platform) takeRecycled() *addressSpace {
	p.spareMu.Lock()
	defer p.spareMu.Unlock()
	for len(p.recycled) > 0 {
		as := p.recycled[len(p.recycled)-1]
		p.recycled = p.recycled[:len(p.recycled)-1]
		if !as.dead.Load() {
			return as
		}
		go as.kill()
	}
	return nil
}

// reset empties as for a new owner; it returns why it could not ("" if it did).
func (as *addressSpace) reset() string {
	as.relMu.Lock()
	defer as.relMu.Unlock()
	// Every guest thread must be waiting for a reply, in the stub's code: it then
	// waits on for the next owner (allocThread hands it out with the whole context).
	// A thread anywhere else would need TerminateThread, which can leave a lock of
	// ntdll held and the stub unable to start a thread again
	// (docs/results/1m-platform-repair.md).
	as.tmu.Lock()
	for _, t := range as.slots[1:] {
		if t != nil && t.started && (t.hThread == 0 || t.state() != 1) {
			as.tmu.Unlock()
			return "a thread is not waiting for a reply"
		}
	}
	for _, t := range as.slots[1:] {
		if t != nil && t.started {
			t.idle = true
		}
	}
	as.freeSlots = as.freeSlots[:0]
	for i := len(as.slots) - 1; i >= 1; i-- {
		if as.slots[i] != nil && !as.slots[i].lost {
			as.freeSlots = append(as.freeSlots, i)
		}
	}
	as.gen.Add(1)
	as.tmu.Unlock()

	as.mu.Lock()
	defer as.mu.Unlock()
	var sys []*view
	for _, v := range as.views {
		if v.sys {
			sys = append(sys, v)
			continue
		}
		if _, err := call(pUnmapViewOfFile2, as.h, uintptr(v.lo), memPreservePH); err != nil {
			return "a view could not be unmapped"
		}
		as.freeInsert(rng{v.lo, v.hi})
	}
	as.views = sys
	for _, p := range as.privs {
		pCloseHandle.Call(p.h)
	}
	as.privs = nil
	n := newAddressSpace(as.p)
	as.nsites, as.vcur, as.fcur, as.ftop, as.gcur = 0, n.vcur, n.fcur, n.ftop, stubasm.FSBig
	as.siteCache, as.restartAt, as.ud2Sites, as.fsCache, as.fsVen = n.siteCache, n.restartAt, n.ud2Sites, n.fsCache, nil
	as.fsUD2, as.fsSplit, as.fsWhy, as.fsHits = n.fsUD2, n.fsSplit, n.fsWhy, n.fsHits
	as.tails, as.tailExact, as.heads, as.edges = n.tails, n.tailExact, n.heads, n.edges
	as.hist, as.histOK = [32]byte{}, true
	as.holesDone.Store(false)
	// Nothing of the guest may remain: every committed page of the guest range is
	// in a view of the stub's own or in a hole Windows keeps for itself
	// (KUSER_SHARED_DATA, the loader's window for thread stacks and TEBs).
	own := []rng{{uint64(holeStart), uint64(holeEnd)}, {stubasm.WinWindow, stubasm.WinWindow + stubasm.WinWindowSize}}
	for _, v := range sys {
		own = append(own, rng{v.lo, v.hi})
	}
	for _, r := range queryCommitted(as.h, stubasm.GuestMin, stubasm.GuestMax) {
		ok := false
		for _, o := range own {
			if o.lo <= r.lo && r.hi <= o.hi {
				ok = true
				break
			}
		}
		if !ok {
			return "guest memory left at " + r.String()
		}
	}
	return ""
}

// queryCommitted returns the committed regions of [lo,hi) in process h.
func queryCommitted(h uintptr, lo, hi uint64) []rng {
	var out []rng
	for addr := lo; addr < hi; {
		var m mbi
		if r, _, _ := pVirtualQueryEx.Call(h, uintptr(addr), uintptr(unsafe.Pointer(&m)), unsafe.Sizeof(m)); r == 0 {
			break
		}
		end := uint64(m.Base) + uint64(m.Size)
		if m.State == memCommit {
			out = append(out, rng{max(lo, uint64(m.Base)), min(hi, end)})
		}
		if end <= addr {
			break
		}
		addr = end
	}
	return out
}
