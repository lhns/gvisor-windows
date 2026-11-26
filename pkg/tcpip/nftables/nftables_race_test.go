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

package nftables

import (
	"fmt"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/rand"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// raceFixture is a published ruleset with a base chain that counts, looks the
// source address up in a set and jumps to a regular chain that counts too.
type raceFixture struct {
	stk *stack.Stack
}

func newRaceFixture(t *testing.T) *raceFixture {
	t.Helper()
	stk := stack.New(stack.Options{})
	nf := NewNFTables(stk, tcpip.NewStdClock(), rand.RNGFrom(&fixedReader{}))
	tab, err := nf.AddTable(arbitraryFamily, "t", false)
	if err != nil {
		t.Fatalf("AddTable: %v", err)
	}
	base, err := nf.AddChainToTable(tab, "base", arbitraryInfoPolicyAccept, "", false, 0, nil, linux.NF_ACCEPT)
	if err != nil {
		t.Fatalf("AddChainToTable base: %v", err)
	}
	sub, err := nf.AddChainToTable(tab, "sub", nil, "", false, 0, nil, linux.NF_ACCEPT)
	if err != nil {
		t.Fatalf("AddChainToTable sub: %v", err)
	}

	subRule := &Rule{}
	subRule.addOperation(newCounter(0, 0))
	subRule.addOperation(&last{})
	if err := sub.RegisterRule(subRule, -1); err != nil {
		t.Fatalf("RegisterRule sub: %v", err)
	}

	jump := mustCreateJump(t, "sub")
	jump.verdict.Chain = sub
	baseRule := &Rule{}
	baseRule.addOperation(newCounter(0, 0))
	baseRule.addOperation(jump)
	if err := base.RegisterRule(baseRule, -1); err != nil {
		t.Fatalf("RegisterRule base: %v", err)
	}

	set := &nftSet{name: "s", handle: 1, keyLen: 4, backend: nf.newSetMapBackend(4, 0)}
	tab.sets[set.name] = set
	tab.setHandles[set.handle] = set

	stk.SetNFTables(nf)
	return &raceFixture{stk: stk}
}

// update runs one writer transaction as processBatchMessage does: copy the
// published ruleset, change the copy, publish it or discard it.
func (f *raceFixture) update(t *testing.T, n int, publish bool, change func(cp *NFTables, tab *Table)) {
	t.Helper()
	f.stk.LockNFTablesUpdate()
	defer f.stk.UnlockNFTablesUpdate()
	cur := f.stk.NFTables().(*NFTables)
	cp := cur.DeepCopy()
	tab, err := cp.GetTable(arbitraryFamily, "t", 0)
	if err != nil {
		t.Errorf("update %d: GetTable: %v", n, err)
		return
	}
	change(cp, tab)
	if !publish {
		cp.Discard(cur)
		return
	}
	cp.SetGenID(cp.GetGenID() + 1)
	f.stk.SetNFTables(cp)
}

// TestRulesetUpdatesRaceWithPacketEvaluation changes the ruleset (rules, set
// elements, tables, a NAT chain that creates a conntrack, discarded copies)
// while goroutines evaluate packets against the published ruleset. It is only
// meaningful under the race detector.
func TestRulesetUpdatesRaceWithPacketEvaluation(t *testing.T) {
	f := newRaceFixture(t)
	const (
		writers    = 3
		readers    = 6
		writerLoop = 60
		readerLoop = 400
		maxRules   = 12
	)

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < writerLoop; i++ {
				n := w*writerLoop + i
				switch i % 4 {
				case 0, 1:
					// Rules that read the set, count and jump; old ones are deleted.
					f.update(t, n, true, func(cp *NFTables, tab *Table) {
						base, _ := tab.GetChain("base")
						set := tab.sets["s"]
						key := []byte{10, byte(w), byte(i >> 8), byte(i)}
						set.elements = append(set.elements, nftSetElem{startKey: key})
						idx := len(set.elements) - 1
						if _, err := set.backend.Add(&set.elements[idx], idx); err != nil {
							t.Errorf("set add: %v", err)
						}
						lk := &lookupOp{set: set, sregIdx: 0}
						set.bindings = append(set.bindings, lk)
						r := &Rule{}
						r.addOperation(mustCreatePayloadLoad(t, linux.NFT_PAYLOAD_NETWORK_HEADER, ipv4SrcAddrOffset, ipv4SrcAddrLen, linux.NFT_REG_1))
						r.addOperation(lk)
						r.addOperation(newCounter(0, 0))
						if err := base.RegisterRule(r, -1); err != nil {
							t.Errorf("RegisterRule: %v", err)
						}
						for base.RuleCount() > maxRules {
							old, _ := base.GetRule(1)
							if err := base.DeleteRule(old); err != nil {
								t.Errorf("DeleteRule: %v", err)
								return
							}
						}
					})
				case 2:
					// A table is created and removed again.
					name := fmt.Sprintf("tmp%d", n)
					f.update(t, n, true, func(cp *NFTables, _ *Table) {
						tt, err := cp.AddTable(arbitraryFamily, name, false)
						if err != nil {
							t.Errorf("AddTable: %v", err)
							return
						}
						if _, err := cp.AddChainToTable(tt, "c", nil, "", false, 0, nil, linux.NF_ACCEPT); err != nil {
							t.Errorf("AddChainToTable: %v", err)
						}
					})
					f.update(t, n, true, func(cp *NFTables, _ *Table) {
						if _, err := cp.DeleteTable(arbitraryFamily, name); err != nil {
							t.Errorf("DeleteTable: %v", err)
						}
					})
				case 3:
					// A failed batch: a NAT chain makes the copy create a conntrack,
					// and the copy is dropped.
					f.update(t, n, false, func(cp *NFTables, tab *Table) {
						info := &BaseChainInfo{BcType: BaseChainTypeNat, Hook: stack.NFPrerouting, Priority: arbitraryPriority}
						if _, err := cp.AddChainToTable(tab, fmt.Sprintf("nat%d", n), info, "", false, 0, nil, linux.NF_ACCEPT); err != nil {
							t.Errorf("AddChainToTable nat: %v", err)
						}
					})
				}
			}
		}(w)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < readerLoop; i++ {
				// A snapshot must not change under the reader.
				nf := f.stk.NFTables().(*NFTables)
				tab, err := nf.GetTable(arbitraryFamily, "t", 0)
				if err != nil {
					t.Errorf("GetTable: %v", err)
					return
				}
				base, _ := tab.GetChain("base")
				rules := base.RuleCount()
				for j := 0; j < 10; j++ {
					pkt := makeArbitraryIPv4Packet()
					v, err := nf.EvaluateHook(stack.IP, arbitraryHook, pkt, nil /* route */, 0 /* outNICID */)
					if err != nil || v.Code != VC(linux.NF_ACCEPT) {
						t.Errorf("EvaluateHook = %v, %v; want accept", v, err)
						return
					}
				}
				if got := base.RuleCount(); got != rules {
					t.Errorf("published snapshot changed: %d rules, then %d", rules, got)
					return
				}
			}
		}()
	}
	wg.Wait()

	final := f.stk.NFTables().(*NFTables)
	tab, err := final.GetTable(arbitraryFamily, "t", 0)
	if err != nil {
		t.Fatalf("GetTable: %v", err)
	}
	if _, err := tab.GetChain("nat0"); err == nil {
		t.Errorf("a discarded copy leaked its NAT chain into the published ruleset")
	}
	if final.connTrack != nil {
		t.Errorf("a discarded copy leaked its conntrack into the published ruleset")
	}
	// Writers are serialized, so no published update was lost: four updates per
	// four iterations are published, one is discarded.
	if got, want := final.GetGenID(), uint32(1+writers*writerLoop); got != want {
		t.Errorf("genid = %d, want %d", got, want)
	}
}

// TestDiscardStopsConntrackReaper checks that dropping a copy that created its
// own conntrack stops the reaper, and that a copy sharing the published
// conntrack leaves it running.
func TestDiscardStopsConntrackReaper(t *testing.T) {
	f := newRaceFixture(t)
	cur := f.stk.NFTables().(*NFTables)

	shared := cur.DeepCopy()
	shared.Discard(cur) // No conntrack: nothing to stop.

	cp := cur.DeepCopy()
	tab, err := cp.GetTable(arbitraryFamily, "t", 0)
	if err != nil {
		t.Fatalf("GetTable: %v", err)
	}
	info := &BaseChainInfo{BcType: BaseChainTypeNat, Hook: stack.NFPrerouting, Priority: arbitraryPriority}
	if _, err := cp.AddChainToTable(tab, "nat", info, "", false, 0, nil, linux.NF_ACCEPT); err != nil {
		t.Fatalf("AddChainToTable: %v", err)
	}
	if cp.connTrackReaper == nil {
		t.Fatalf("NAT chain did not create a conntrack")
	}
	cp.Discard(cur)
	if cp.connTrackReaper.Stop() {
		t.Errorf("reaper was still running after Discard")
	}
}
