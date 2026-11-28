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
	"container/list"
	"crypto/sha256"
	"sync"

	"gvisor.dev/gvisor/pkg/sentry/platform/winstub/stubasm"
	"gvisor.dev/gvisor/pkg/sentry/profkeys"
)

// Shared veneers (ADR 0025).
//
// A veneer with displaced instructions after the syscall is where the syscall
// returns to, and a fork child with a new address space resumes there: every
// address space puts the veneer of a site (address and original bytes) at the same
// address, which the platform-wide registry hands out.
//
// The space is reclaimed. An address space holds every veneer it got until it is
// released or emptied, unmaps included: a thread stopped in a syscall, or a signal
// frame, may still return into the veneer of a site that is no longer mapped, and
// the platform sees neither. A cached patch state holds the veneers it lists. A
// veneer nobody holds keeps its address (a fork child that maps its text after its
// parent's release finds it there) until the space is needed: only when the area
// has no room left are the idle veneers given up, the longest idle first.

// veneerTop is the end of the shared veneer area (a variable for tests).
var veneerTop = stubasm.Veneers + stubasm.VeneersSize

// veneerRegistry hands out the addresses of the shared veneers.
type veneerRegistry struct {
	mu   sync.Mutex
	next uint64 // never handed out from here up
	m    map[veneerKey]*sharedVen
	free []rng      // given back, sorted, never adjacent
	idle *list.List // of *sharedVen without holders, the longest idle first
}

type veneerKey struct {
	addr uint64
	hash [32]byte
}

// sharedVen is one veneer of the registry. refs and idle are guarded by the
// registry's mu.
type sharedVen struct {
	key      veneerKey
	va, vlen uint64
	refs     int
	idle     *list.Element
}

// sharedVeneer returns the veneer for the window at addr with the given original
// bytes, allocating vlen bytes for it if it has no place; nil if the area is full.
// as holds it from now on. The caller holds as.mu.
func (as *addressSpace) sharedVeneer(addr uint64, window []byte, vlen uint64) *sharedVen {
	k := veneerKey{addr: addr, hash: sha256.Sum256(window)}
	r := &as.p.veneers
	r.mu.Lock()
	v, ok := r.m[k]
	if !ok {
		va := r.alloc(vlen)
		if va == 0 {
			r.mu.Unlock()
			if profkeys.On {
				profkeys.Count("winstub syscall site: shared veneer area full", 0)
			}
			return nil
		}
		if r.m == nil {
			r.m = map[veneerKey]*sharedVen{}
		}
		v = &sharedVen{key: k, va: va, vlen: vlen}
		r.m[k] = v
	}
	if as.vheld[v] {
		r.mu.Unlock()
		return v
	}
	r.refLocked(v)
	r.mu.Unlock()
	if as.vheld == nil {
		as.vheld = map[*sharedVen]bool{}
	}
	as.vheld[v] = true
	return v
}

// holdVeneers makes as hold the veneers of l (which their owner holds). The
// caller holds as.mu.
func (as *addressSpace) holdVeneers(l []sharedRec) {
	r := &as.p.veneers
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, sh := range l {
		if as.vheld[sh.v] {
			continue
		}
		r.refLocked(sh.v)
		if as.vheld == nil {
			as.vheld = map[*sharedVen]bool{}
		}
		as.vheld[sh.v] = true
	}
}

// dropVeneers gives up every veneer as holds: no thread of as runs or will run
// guest code of its owner (Release, or the stub is emptied or terminated).
func (as *addressSpace) dropVeneers() {
	as.mu.Lock()
	held := as.vheld
	as.vheld = nil
	as.mu.Unlock()
	r := &as.p.veneers
	r.mu.Lock()
	defer r.mu.Unlock()
	for v := range held {
		r.unrefLocked(v)
	}
}

// holdSnap and releaseSnap count a cached patch state's hold on its veneers.
func (r *veneerRegistry) holdSnap(l []sharedRec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, sh := range l {
		r.refLocked(sh.v)
	}
}

func (r *veneerRegistry) releaseSnap(l []sharedRec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, sh := range l {
		r.unrefLocked(sh.v)
	}
}

func (r *veneerRegistry) refLocked(v *sharedVen) {
	if v.refs++; v.idle != nil {
		r.idle.Remove(v.idle)
		v.idle = nil
	}
}

func (r *veneerRegistry) unrefLocked(v *sharedVen) {
	if v.refs--; v.refs == 0 {
		if r.idle == nil {
			r.idle = list.New()
		}
		v.idle = r.idle.PushBack(v)
	}
}

// alloc returns the address of vlen free bytes, or 0. The caller holds r.mu.
func (r *veneerRegistry) alloc(vlen uint64) uint64 {
	if r.next == 0 {
		r.next = stubasm.VeneersShared
	}
	for {
		for i, f := range r.free {
			if f.hi-f.lo >= vlen {
				if r.free[i].lo += vlen; r.free[i].lo == f.hi {
					r.free = append(r.free[:i], r.free[i+1:]...)
				}
				return f.lo
			}
		}
		if r.next+vlen <= veneerTop {
			va := r.next
			r.next += vlen
			return va
		}
		// Full: give up the veneer idle the longest.
		if r.idle == nil || r.idle.Len() == 0 {
			return 0
		}
		v := r.idle.Remove(r.idle.Front()).(*sharedVen)
		v.idle = nil
		delete(r.m, v.key)
		r.giveBack(rng{v.va, v.va + v.vlen})
		if profkeys.On {
			profkeys.Count("winstub syscall site: shared veneer space reused", 0)
		}
	}
}

// giveBack adds x to the free ranges, merging neighbours (and the top).
func (r *veneerRegistry) giveBack(x rng) {
	i := 0
	for i < len(r.free) && r.free[i].hi < x.lo {
		i++
	}
	if i < len(r.free) && r.free[i].hi == x.lo {
		x.lo = r.free[i].lo
		r.free = append(r.free[:i], r.free[i+1:]...)
	}
	if i < len(r.free) && r.free[i].lo == x.hi {
		x.hi = r.free[i].hi
		r.free = append(r.free[:i], r.free[i+1:]...)
	}
	if x.hi == r.next {
		r.next = x.lo
		return
	}
	r.free = append(r.free, rng{})
	copy(r.free[i+1:], r.free[i:])
	r.free[i] = x
}

// stats returns the bytes of the shared veneer area handed out and held.
func (r *veneerRegistry) stats() (used, held uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next != 0 {
		used = r.next - stubasm.VeneersShared
	}
	for _, f := range r.free {
		used -= f.hi - f.lo
	}
	for _, v := range r.m {
		if v.refs > 0 {
			held += v.vlen
		}
	}
	return
}
