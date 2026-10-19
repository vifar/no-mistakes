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

// Exercise Document's real daemon path during a competing closing-issue
// write. Agent output and the forge are simulated; SQLite and Git are real.
// The oracle is one completed pass, preserved closing references, and carried
// validation on the published documentation head, with no repeated agent turn.
func TestSQLiteDocumentContentionOnRealDaemon(t *testing.T) {
	action := documentJourneyAction("README.md", "documentation after writer contention\n")
	action["delay_ms"] = 3000
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: documentJourneyScenario(t, "", action)})
	setupStatefulGitHub(t, h)
	branch, submitted := documentJourneyStart(t, h, "commands:\n  lint: 'true'\n")
	waitSQLiteJourney(t, h, "active Document agent", func() bool {
		for _, call := range h.AgentInvocations() {
			if strings.Contains(call.Prompt, "Find what this change made stale") {
				return true
			}
		}
		return false
	})
	run := h.ActiveRun(branch)
	if run == nil {
		t.Fatal("no active Document run")
	}
	raw, err := sql.Open("sqlite", filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE runs SET closing_issue_refs='example/repo#92' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(6 * time.Second)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	completed := waitForStepStatus(t, h, branch, types.StepPR, types.StepStatusCompleted, 90*time.Second)
	passes := documentJourneyPasses(t, h, run.ID)
	if completed.HeadSHA == submitted || h.UpstreamBranchSHA(branch) != completed.HeadSHA || len(passes) != 1 || passes[0].CommitCount != 1 || passes[0].CarriedReviewFrom == nil || *passes[0].CarriedReviewFrom != submitted || documentJourneyCalls(h) != 1 {
		t.Fatalf("Document lost credit or repeated work: run=%+v passes=%+v calls=%d", completed, passes, documentJourneyCalls(h))
	}
	var refs, approved string
	if err := raw.QueryRow(`SELECT closing_issue_refs, review_approved_head_sha FROM runs WHERE id=?`, run.ID).Scan(&refs, &approved); err != nil || refs != "example/repo#92" || approved != completed.HeadSHA {
		t.Fatalf("lost concurrent writer or validation: refs=%q approved=%q err=%v", refs, approved, err)
	}
	t.Logf("real daemon published documentation head %s after a six-second closing-issue writer; refs=%s, one Document invocation, one completed pass, carried Review head=%s", completed.HeadSHA, refs, approved)
	documentJourneyStatus(t, h, run.ID)
}
