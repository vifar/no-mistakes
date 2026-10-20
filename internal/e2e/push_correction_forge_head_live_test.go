//go:build e2e && !windows

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Local correction lineage must never authorize a different publisher's head.
// The oracle is the recorded human decision: refuse without publication or a
// rewind and retain the original forge-head binding, even if the target moves.
func TestPushCorrectionIndependentForgeHeadLiveJourney(t *testing.T) {
	for _, targetMoves := range []bool{false, true} {
		t.Run(fmt.Sprintf("target_moves_%t", targetMoves), func(t *testing.T) {
			j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
			h := j.h
			j.setForge(forgeState{Checks: []forgeCheck{greenCheck("build")}})
			script := filepath.Join(filepath.Dir(h.AgentLog), "formatter-forge-race.sh")
			commitTrustedRepoConfig(t, h, fmt.Sprintf("commands:\n  test: 'true'\n  lint: 'true'\n  format: %q\n", "sh "+shellQuote(script)))
			base := h.UpstreamBranchSHA("main")
			target := h.CommitChange("main", "landed.txt", "advanced target\n", "prepare target")
			gitIn(t, h, h.WorkDir, "push", "origin", target+":refs/heads/prepared-target")
			gitIn(t, h, h.WorkDir, "reset", "--hard", base)
			const branch = "feature/push-independent-forge-head"
			source := h.CommitChange(branch, "feature.txt", "submitted feature\n", "published source")
			gitIn(t, h, h.WorkDir, "push", "origin", branch)
			body, _ := steps.BuildPipelineSummary([]*db.StepResult{{StepName: types.StepReview, Status: types.StepStatusCompleted}, {StepName: types.StepTest, Status: types.StepStatusCompleted}, {StepName: types.StepDocument, Status: types.StepStatusCompleted}}, nil, source)
			writeStatefulGH(t, j.statePath, statefulGHState{PRs: map[string]*statefulGHPR{branch: {Number: 42, URL: "https://github.com/example/closes/pull/42", Title: "Independent forge head", Body: body, Base: "main", Head: branch, State: "OPEN"}}})
			command := fmt.Sprintf("#!/bin/sh\nset -eu\nprintf 'corrected by Push\\n' > feature.txt\ngit --git-dir=%s update-ref refs/heads/%s %s\n", shellQuote(h.UpstreamDir), branch, target)
			if targetMoves {
				command += fmt.Sprintf("git --git-dir=%s update-ref refs/heads/main %s\n", shellQuote(h.UpstreamDir), target)
			}
			if err := os.WriteFile(script, []byte(command), 0700); err != nil {
				t.Fatal(err)
			}
			initFromOwnWorktree(t, h, "init-independent-forge-head")
			h.Checkout(branch)
			h.PushToGate(branch)
			run := h.WaitForRun(branch, 90*time.Second)
			if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "PR comparison changed") {
				t.Fatalf("independent forge head was not refused: %s %s", run.Status, deref(run.Error))
			}
			database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			stored, runErr := database.GetRun(run.ID)
			receipt, receiptErr := database.GetRunPRContext(run.ID)
			closeErr := database.Close()
			if runErr != nil || receiptErr != nil || closeErr != nil {
				t.Fatalf("read durable refusal: %v %v %v", runErr, receiptErr, closeErr)
			}
			if stored == nil || stored.LastPushedSHA != nil || stored.TargetAdvanceRewinds != 0 || receipt == nil || receipt.ForgeHeadSHA != source {
				t.Fatalf("local correction authorized external movement: run=%+v receipt=%+v", stored, receipt)
			}
			if got := h.UpstreamBranchSHA(branch); got != target {
				t.Fatalf("pipeline overwrote independent publisher: %s, want %s", got, target)
			}
			// Terminal cleanup may already have removed the managed worktree.
			// The recovery ref is the durable custody contract for this commit.
			gate := paths.WithRoot(h.NMHome).RepoDir(run.RepoID)
			if stored.HeadSHA == source || gitIn(t, h, h.WorkDir, "--git-dir="+gate, "rev-parse", "refs/no-mistakes/recover/"+run.ID) != stored.HeadSHA ||
				gitIn(t, h, h.WorkDir, "--git-dir="+gate, "show", stored.HeadSHA+":feature.txt") != "corrected by Push" {
				t.Fatal("the refusal did not exercise a retained pipeline correction")
			}
			out, err := h.Run("axi", "status", "--run", run.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("product refusal:\n%s", out)
			t.Logf("independent remote head %s retained; receipt forge head remains %s; no publication or rewind; local correction head %s", target, receipt.ForgeHeadSHA, stored.HeadSHA)
		})
	}
}
