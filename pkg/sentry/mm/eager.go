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

// gvisor-windows: mapping memory up front on platforms whose page faults are
// expensive (docs/results/1r-platform-speed.md).

package mm

import (
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// EagerForkCopyLimit, if not zero, makes Fork give the child copies of the
// parent's private memory, mapped into the child's address space at once, when
// that memory is at most this many bytes, instead of sharing it copy-on-write.
// Neither process then faults on that memory after the fork. A platform whose
// page faults cost much more than copying a page (winstub: a debugger round trip
// of 0.1 to 0.4 ms) sets it.
var EagerForkCopyLimit uint64

// EagerExecMap makes the ELF loader map the new image (segments, vDSO, the top of
// the stack) into the address space before the first instruction runs, the
// writable segments already copied (PrefaultImage). Set by the same platforms.
var EagerExecMap bool

// prefaultWritableMax bounds the writable mappings PrefaultImage populates: a
// large bss would be allocated whole.
const prefaultWritableMax = 2 << 20

// PrefaultImage maps what a new process will touch first: every mapping of the
// image as the access it allows (so a writable private segment is copied now),
// and the stack from sp up. Errors are ignored: the pages then fault as usual.
//
// Preconditions: mm is the new memory manager of an execve and not yet in use.
func (mm *MemoryManager) PrefaultImage(ctx context.Context, sp hostarch.Addr) {
	type pf struct {
		ar hostarch.AddrRange
		at hostarch.AccessType
	}
	var todo []pf
	mm.mappingMu.RLock()
	for vseg := mm.vmas.FirstSegment(); vseg.Ok(); vseg = vseg.NextSegment() {
		vma := vseg.ValuePtr()
		ar := vseg.Range()
		at := vma.effectivePerms
		if !at.Any() {
			continue
		}
		switch {
		case vma.growsDown || vma.isStack:
			if !ar.Contains(sp) {
				continue
			}
			ar.Start = sp.RoundDown()
		case at.Write && ar.Length() > prefaultWritableMax:
			continue
		}
		todo = append(todo, pf{ar, at})
	}
	mm.mappingMu.RUnlock()
	for _, p := range todo {
		mm.prefault(ctx, p.ar, p.at)
	}
}

// prefault gets pmas for ar as an access at would, and maps them into the
// address space.
func (mm *MemoryManager) prefault(ctx context.Context, ar hostarch.AddrRange, at hostarch.AccessType) {
	mm.mappingMu.RLock()
	vseg, _, err := mm.getVMAsLocked(ctx, ar, at, false)
	if err != nil {
		mm.mappingMu.RUnlock()
		return
	}
	mm.activeMu.Lock()
	pseg, _, err := mm.getPMAsLocked(ctx, vseg, ar, at, false /* callerIndirectCommit */, false /* forPin */)
	mm.mappingMu.RUnlock()
	if err != nil || mm.as == nil {
		mm.activeMu.Unlock()
		return
	}
	mm.activeMu.DowngradeLock()
	mm.mapASLocked(ctx, pseg, ar, memmap.PlatformEffectDefault)
	mm.activeMu.RUnlock()
}
