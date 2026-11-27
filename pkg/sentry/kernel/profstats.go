// gvisor-windows: cheap per-syscall counters for profiling (GVISOR_PROFSTATS=1).

package kernel

import (
	"gvisor.dev/gvisor/pkg/sentry/profkeys"
	"fmt"
	"io"
	"os"
	"sort"
	"sync/atomic"
)

// ProfStats enables the counters below; set from the environment at start.
var ProfStats = os.Getenv("GVISOR_PROFSTATS") != ""

type profStat struct{ n, ns atomic.Int64 }

var (
	profSys    [1024]profStat // by syscall number: calls and time in the Sentry's implementation
	profSwitch [5]profStat    // by outcome of Platform.Switch: time in the guest and the platform
)

var profSwitchNames = [...]string{"syscall", "interrupt", "fault", "preempted", "other"}

// profRecordSwitch counts one return from Platform.Switch.
func profRecordSwitch(outcome int, ns int64) {
	profSwitch[outcome].n.Add(1)
	profSwitch[outcome].ns.Add(ns)
}

// ProfNow is the clock of the counters, in ns.
func ProfNow() int64 { return profNow() }

// DumpProfStats writes the counters, the syscalls sorted by total time.
func DumpProfStats(w io.Writer, name func(sysno uintptr) string) {
	type row struct {
		no     int
		n, ns  int64
	}
	var rows []row
	var tn, tns int64
	for i := range profSys {
		if n := profSys[i].n.Load(); n > 0 {
			rows = append(rows, row{i, n, profSys[i].ns.Load()})
			tn += n
			tns += profSys[i].ns.Load()
		}
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].ns > rows[b].ns })
	fmt.Fprintf(w, "PROFSTATS syscalls: %d calls, %.1f ms in the Sentry's implementations\n", tn, float64(tns)/1e6)
	fmt.Fprintf(w, "PROFSTATS %-24s %9s %10s %9s\n", "syscall", "calls", "total ms", "us/call")
	for _, r := range rows {
		fmt.Fprintf(w, "PROFSTATS %-24s %9d %10.1f %9.1f\n", name(uintptr(r.no)), r.n, float64(r.ns)/1e6, float64(r.ns)/float64(r.n)/1e3)
	}
	profkeys.Dump(w)
	for i, nm := range profSwitchNames {
		if n := profSwitch[i].n.Load(); n > 0 {
			fmt.Fprintf(w, "PROFSTATS switch %-10s %9d returns, %10.1f ms between the Sentry's resume and the return (guest + platform)\n", nm, n, float64(profSwitch[i].ns.Load())/1e6)
		}
	}
}
