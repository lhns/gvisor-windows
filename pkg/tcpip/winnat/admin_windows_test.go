package winnat_test

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestAdminProbe tries something only an administrator can do (creating a
// file in the Windows directory) and checks it against the account the
// workflow expects, so CI proves that the network tests above ran without
// elevation. GVISOR_ADMIN_PROBE is "denied" (a standard user: the probe must
// fail), "allowed" (an administrator: it must succeed) or unset (skipped).
func TestAdminProbe(t *testing.T) {
	want := os.Getenv("GVISOR_ADMIN_PROBE")
	if want == "" {
		t.Skip("GVISOR_ADMIN_PROBE not set")
	}
	elevated := windows.GetCurrentProcessToken().IsElevated()
	probe := filepath.Join(os.Getenv("SystemRoot"), "gvisor-admin-probe.txt")
	err := os.WriteFile(probe, []byte("x"), 0o600)
	if err == nil {
		os.Remove(probe)
	}
	t.Logf("elevated token: %v; writing %s: %v", elevated, probe, err)
	switch want {
	case "denied":
		if err == nil || elevated {
			t.Fatalf("expected a standard user, but the probe succeeded (err=%v, elevated=%v)", err, elevated)
		}
	case "allowed":
		if err != nil || !elevated {
			t.Fatalf("expected an administrator, but the probe failed (err=%v, elevated=%v)", err, elevated)
		}
	default:
		t.Fatalf("GVISOR_ADMIN_PROBE=%q, want denied or allowed", want)
	}
}
