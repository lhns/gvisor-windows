# 1n: nftables safe for concurrent use

Node: "Make nftables safe for concurrent use" (net-nftables-thread-safety). Starting point: `1n-bridge-networking.md` kept `--nftables` off because the package comment said "not yet thread-safe" (b/345684870).

## Hazards found

The gVisor commit this branch was merged from (release-20261123.0-87, `c3efd8c93`) already has a copy-on-write scheme; only the package comment and flag description still carried the old caveat. I audited every place state is read on the packet path and written by netlink.

How it works today:
- Readers: `ipv4.go`/`ipv6.go` load `Stack.NFTables()` (an `atomic.Pointer`) per hook and evaluate against that snapshot without a lock. Evaluation writes only to per-packet state (`registerSet`, `PacketBuffer`), atomics (`counter`, `last`, `ChainCounter`, `conn.mark`) and the conntrack, which has its own locking (it is what iptables uses too).
- Writers: `ProcessMessage` and `processBatchMessage` take `Stack.nftablesUpdateMu`; a batch works on `NFTables.DeepCopy()` and `commitNftables` publishes it with `SetNFTables` only at `NFNL_MSG_BATCH_END` without errors. The netlink GET paths read the live snapshot under the same mutex and never mutate it.
- This is the same idea as `pkg/tcpip/stack/iptables.go`, which keeps tables behind an RWMutex and replaces them wholesale on a `SetEntries`; here readers do not even take the read lock.

Checked and found safe: `deepCopyTable/Chain/Rule/Set` clone every map, slice and set backend; lookup and jump targets are re-pointed by `updateReferences`, so a copy never shares a mutable object with the published snapshot; `flagSet`, `chainUse` and `bindings` are only written on copies; `rng` is a value holding the stack's secure reader; `SetNFTablesConfigured` is atomic.

Defects fixed:
1. A copy that creates a conntrack (first NAT chain or `ct`/`nat`/`masq`/xt expression) also starts its reaper timer. When the batch failed or never reached `BATCH_END`, the copy was dropped but the timer kept running and kept the conntrack alive. `NFTables.Discard(published)` now stops the reaper of an uncommitted copy that owns its own conntrack, called from a `defer` in `processBatchMessage`.
2. The documentation claimed the opposite of the code (package comment, TODO, flag text). Replaced by a short description of the scheme.

Accepted limits (not safety problems):
- Counters are copied by value, so increments that hit the old snapshot between `DeepCopy` and the swap are lost (the architecture note in `protocol.go` already says so). Sharing the atomics between snapshots would fix it, but upstream's `TestDeepCopyIsolatesOperations` requires copies to be separate objects.
- A packet that passes several hooks may see a different snapshot at each hook; conntrack is shared, so this matches Linux, where a transaction commits between hooks.
- A conntrack reaper `Stop` racing with the reaper callback can re-arm it once; it only happens on a discarded copy and is bounded by one more tick.

## Tests

- `pkg/tcpip/nftables/nftables_test.go` and `nftinterp_test.go` copied unchanged from gVisor master at `c3efd8c9381c89b78c1d86b6c64a49d53ee938f8`. (Upstream's own `TestNFTablesConcurrentUpdateAndEvaluate` calls `CheckInput` with nftables not enabled, so it never evaluates a rule.)
- `pkg/tcpip/nftables/nftables_race_test.go` (new): three writers do copy, change, publish-or-discard (rules with set lookups, counters and a jump, set elements, table create and delete, a discarded copy with a NAT chain) while six readers evaluate packets against the published snapshot and check that it does not change under them; also checks that no update is lost (`genid`) and that a discarded copy leaks neither chain nor conntrack. `TestDiscardStopsConntrackReaper` covers defect 1.
- Workflow `.github/workflows/nftables.yml`: `go test -race -count=3` of the race tests, then the whole package under `-race`.
- Negative check: with `cp := cur` instead of `cur.DeepCopy()` the race test fails with `WARNING: DATA RACE` (run 45878419860), so it detects an in-place update.

Results (`ubuntu-latest`, Go from `go.mod`):
- race tests and full package with `-race`, green: https://github.com/lhns/gvisor-windows/actions/runs/45878197798
- negative check, red as intended: https://github.com/lhns/gvisor-windows/actions/runs/45878419860

## Default on

`--nftables` now defaults to true (`runsc/config/flags.go`; `--nftables=false` turns it off). `install.sh` no longer passes the flag, so the acceptance jobs run on the default. Overhead with no nftables rule configured is the `IsNFTablesConfigured` atomic load per hook.

`podman-acceptance-linux` on the branch with the default on (dispatch, all jobs green, including `bridge-v6.2.0-nft-priv` (Podman 6.2.0, nftables) and `bridge-v5.7.1-ipt-priv`): https://github.com/lhns/gvisor-windows/actions/runs/45878471726. `build` (Linux and Windows): https://github.com/lhns/gvisor-windows/actions/runs/45878477142.

Verdict: The nftables package was already safe for concurrent use by copy-on-write snapshots swapped atomically per batch (lock-free readers); one leak of a discarded copy's conntrack timer is fixed, a `-race` test that fails without the copy and passes with it runs in CI, and `--nftables` is now on by default after the strict Podman 6.2.0 and 5.7.1 jobs passed with it.
