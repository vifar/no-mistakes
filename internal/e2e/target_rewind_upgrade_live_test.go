//go:build e2e && !windows

package e2e

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Upgrade input is an on-disk database without retarget provenance. The
// independent oracle is the approved eligibility rule: unknown provenance
// and confirmed attempts must both refuse drift without charging a rewind.
// The real daemon performs migration, recovery, and comparison validation.
func TestTargetAdvanceUpgradeProvenanceJourney(t *testing.T) {
	for _, provenance := range []string{"legacy unknown", "confirmed attempt"} {
		t.Run(provenance, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
			commitTrustedRepoConfig(t, h, "commands:\n  test: 'false'\nauto_fix:\n  test: 0\n")
			initFromOwnWorktree(t, h, "init-upgrade-provenance")
			const branch = "feature/upgrade-provenance"
			h.CommitChange(branch, "feature.txt", "unpublished work\n", "submit feature")
			h.PushToGate(branch)
			parked := waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 90*time.Second)
			gateJourneyDaemon(t, h, "stop")
			path := filepath.Join(h.NMHome, "state.sqlite")
			if provenance == "legacy unknown" {
				legacy, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = legacy.Exec("ALTER TABLE runs DROP COLUMN retarget_attempted; PRAGMA user_version=0")
				closeErr := legacy.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("prepare pre-upgrade database: %v %v", err, closeErr)
				}
			} else {
				database, err := db.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				err = database.MarkRunRetargetAttempted(parked.ID)
				closeErr := database.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("record confirmed attempt: %v %v", err, closeErr)
				}
			}
			target := advanceMain(t, h, "landed.txt", "landed target\n", "advance target during upgrade")
			gateJourneyDaemon(t, h, "start")
			// Recovery itself must reject the drift, before an old gate can
			// authorize work against a comparison lacking eligibility proof.
			terminal := h.WaitForRun(branch, 90*time.Second)
			if terminal.Status != types.RunFailed || terminal.Error == nil || !strings.Contains(*terminal.Error, "PR comparison changed after rebase") {
				out, _ := h.Run("axi", "status", "--run", parked.ID)
				t.Fatalf("unproven retarget absence proceeded: %s %s\n%s", terminal.Status, deref(terminal.Error), out)
			}
			charge, pending, history := targetRewindHistory(t, h.NMHome, parked.ID)
			if charge != 0 || pending || history != 0 || h.UpstreamBranchSHA("main") != target {
				t.Fatalf("refusal mutated rewind history or target: %d %t %d", charge, pending, history)
			}
			out, err := h.Run("axi", "status", "--run", parked.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: real daemon refused target drift after recovery without charging a rewind:\n%s", provenance, out)
		})
	}
}
