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
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The author's contract requires simultaneous pipeline-owned head and target
// advances to rewind before publication. Real Git content/ancestry and the
// daemon's durable review approval are the oracle; failure after the correction
// or publication of the old target must not count as success.
func TestPushCorrectionTargetAdvanceLiveJourney(t *testing.T) {
	for _, attached := range []bool{false, true} {
		t.Run(fmt.Sprintf("attached_PR_%t", attached), func(t *testing.T) {
			opts := SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)}
			var h *Harness
			var journey *scopeJourney
			if attached {
				journey = newScopeJourney(t, opts)
				journey.setForge(forgeState{Checks: []forgeCheck{greenCheck("build")}})
				h = journey.h
			} else {
				h = NewHarness(t, opts)
			}
			script := filepath.Join(filepath.Dir(h.AgentLog), "format-race.sh")
			commitTrustedRepoConfig(t, h, fmt.Sprintf("commands:\n  test: 'true'\n  lint: 'true'\n  format: %q\n", "sh "+shellQuote(script)))
			base := h.UpstreamBranchSHA("main")
			target := h.CommitChange("main", "landed.txt", "advanced target\n", "prepare target")
			gitIn(t, h, h.WorkDir, "push", "origin", target+":refs/heads/prepared-target")
			gitIn(t, h, h.WorkDir, "reset", "--hard", base)
			const branch = "feature/push-correction-window"
			source := h.CommitChange(branch, "feature.txt", "submitted feature\n", "submit feature")
			gitIn(t, h, h.WorkDir, "push", "origin", branch)
			h.CommitChange(branch, "unpublished.txt", "submitted addition\n", "unpublished addition")
			if attached {
				body, _ := steps.BuildPipelineSummary([]*db.StepResult{{StepName: types.StepReview, Status: types.StepStatusCompleted}, {StepName: types.StepTest, Status: types.StepStatusCompleted}, {StepName: types.StepDocument, Status: types.StepStatusCompleted}}, nil, source)
				writeStatefulGH(t, journey.statePath, statefulGHState{PRs: map[string]*statefulGHPR{branch: {Number: 42, URL: "https://github.com/example/closes/pull/42", Title: "Push correction", Body: body, Base: "main", Head: branch, State: "OPEN"}}})
			}
			// This deterministic stand-in for a repository formatter changes a
			// tracked file and advances the actual target remote during Push.
			command := fmt.Sprintf("#!/bin/sh\nset -eu\nprintf 'corrected by Push\\n' > feature.txt\ngit --git-dir=%s update-ref refs/heads/main %s\n", shellQuote(h.UpstreamDir), target)
			if err := os.WriteFile(script, []byte(command), 0700); err != nil {
				t.Fatal(err)
			}
			initFromOwnWorktree(t, h, "init-push-correction")
			h.Checkout(branch)
			h.PushToGate(branch)
			var run *ipc.RunInfo
			if attached {
				run = journey.expectReady(branch, 90*time.Second)
			} else {
				run = h.WaitForRun(branch, 90*time.Second)
				if run.Status != types.RunCompleted {
					t.Fatalf("correction and target advance failed: %s %s", run.Status, deref(run.Error))
				}
			}
			published := h.UpstreamBranchSHA(branch)
			if published != run.HeadSHA || !isAncestorIn(t, h, h.UpstreamDir, target, published) {
				t.Fatal("published head did not integrate the advanced target")
			}
			for path, want := range map[string]string{"feature.txt": "corrected by Push", "unpublished.txt": "submitted addition"} {
				if got := gitIn(t, h, h.UpstreamDir, "show", published+":"+path); got != want {
					t.Fatalf("published %s = %q, want %q", path, got, want)
				}
			}
			subjects := gitIn(t, h, h.UpstreamDir, "log", "--format=%s", base+".."+published)
			if strings.Count(subjects, "no-mistakes: apply agent fixes") != 1 {
				t.Fatalf("expected exactly one preserved Push correction: %s", subjects)
			}
			charge, pending, history := targetRewindHistory(t, h.NMHome, run.ID)
			if charge != 1 || pending || history != 1 {
				t.Fatalf("rewind lifecycle: %d %t %d", charge, pending, history)
			}
			database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			stored, readErr := database.GetRun(run.ID)
			closeErr := database.Close()
			if readErr != nil || closeErr != nil || stored == nil || stored.ReviewApprovedHeadSHA == nil || *stored.ReviewApprovedHeadSHA != published {
				t.Fatalf("published correction lacks exact-head review approval: %+v %v %v", stored, readErr, closeErr)
			}
			logTargetRewindStatus(t, h, run.ID)
			t.Logf("published=%s, target=%s; corrected feature and submitted addition retained; exact published head approved by Review; one Push correction:\n%s", published, target, subjects)
			if attached {
				t.Logf("live PR body:\n%s", livePRBody(t, journey.statePath, branch))
				journey.finish(branch)
			}
		})
	}
}

func TestPushTargetAdvanceBudgetLiveJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
	root := filepath.Dir(h.AgentLog)
	script, count := filepath.Join(root, "advance-at-push.sh"), filepath.Join(root, "push-count")
	commitTrustedRepoConfig(t, h, fmt.Sprintf("commands:\n  test: 'true'\n  lint: 'true'\n  format: %q\nrebase:\n  max_target_advance_rewinds: 1\n", "sh "+shellQuote(script)))
	base := h.UpstreamBranchSHA("main")
	first := h.CommitChange("main", "landed.txt", "first target\n", "prepare first advance")
	second := h.CommitChange("main", "landed.txt", "second target\n", "prepare second advance")
	gitIn(t, h, h.WorkDir, "push", "origin", second+":refs/heads/prepared-target")
	gitIn(t, h, h.WorkDir, "reset", "--hard", base)
	const branch = "feature/push-budget"
	submitted := h.CommitChange(branch, "feature.txt", "submitted feature\n", "submit feature")
	gitIn(t, h, h.WorkDir, "push", "origin", branch)
	command := fmt.Sprintf("#!/bin/sh\nset -eu\nif [ -f %s ]; then target=%s; else target=%s; fi\nprintf 'push\\n' >> %s\ngit --git-dir=%s update-ref refs/heads/main \"$target\"\n", shellQuote(count), second, first, shellQuote(count), shellQuote(h.UpstreamDir))
	if err := os.WriteFile(script, []byte(command), 0700); err != nil {
		t.Fatal(err)
	}
	initFromOwnWorktree(t, h, "init-push-budget")
	h.Checkout(branch)
	h.PushToGate(branch)
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "target advance rewind cap reached") {
		t.Fatalf("repeated Push advances did not refuse at the budget: %s %s", run.Status, deref(run.Error))
	}
	charge, pending, history := targetRewindHistory(t, h.NMHome, run.ID)
	if charge != 1 || pending || history != 1 || h.UpstreamBranchSHA(branch) != submitted || h.UpstreamBranchSHA("main") != second {
		t.Fatalf("bounded refusal rewound twice or published: %d %t %d", charge, pending, history)
	}
	data, err := os.ReadFile(count)
	if err != nil || string(data) != "push\npush\n" {
		t.Fatalf("expected two distinct advances during Push: %q %v", data, err)
	}
	logTargetRewindStatus(t, h, run.ID)
	t.Logf("two Push advances (%s then %s), one committed rewind, explicit budget refusal, upstream feature branch unchanged at %s", first, second, submitted)
}
