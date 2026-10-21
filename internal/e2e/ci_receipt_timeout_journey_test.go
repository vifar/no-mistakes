//go:build e2e && !windows

package e2e

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #87's repair path is a terminal failed run whose reason the real AXI
// status command exposes. Remove only this disposable run's receipt to model
// the missing write; never bypass the production comparison guard.
func TestCIReceiptTimeoutStatusJourney(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude", GlobalConfigExtra: "ci_readiness_receipt_timeout: 1s\n"})
	j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{runningCheck("build")}})
	j.init()
	const branch = "feature/receipt-timeout"
	j.push(branch)
	run := j.expectWaiting(branch, "CI checks running, waiting for results...")
	database, err := sql.Open("sqlite", filepath.Join(j.h.NMHome, "state.sqlite")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec("DELETE FROM run_pr_contexts WHERE run_id = ?", run.ID); err != nil {
		t.Fatal(err)
	}
	j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{greenCheck("build")}})
	deadline := time.Now().Add(90 * time.Second)
	info := j.h.RunInfo(run.ID)
	for !info.Status.Terminal() && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		info = j.h.RunInfo(run.ID)
	}
	if info.Status != types.RunFailed {
		t.Fatalf("run=%s err=%q log=%s", info.Status, deref(info.Error), j.ciLog(run.ID))
	}
	out, err := j.h.Run("axi", "status", "--run", run.ID)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	for _, want := range []string{"outcome: failed", "ci-readiness-receipt-missing", "comparison receipt", run.HeadSHA} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %q:\n%s", want, out)
		}
	}
	if info.CIReady {
		t.Fatal("failed receipt was reported ready")
	}
	t.Logf("Real CLI status after owned receipt fault injection:\n%s", out)
}
