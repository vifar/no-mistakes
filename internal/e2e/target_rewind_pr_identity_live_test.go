//go:build e2e && !windows

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The approved design pins PR absence throughout a pending rewind. A newly
// opened PR must stop integration even when its source Git ref never moved.
func TestTargetAdvanceNewPRWhilePendingJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
	state := setupStatefulGitHub(t, h)
	script := filepath.Join(t.TempDir(), "advance.sh")
	commitTrustedRepoConfig(t, h, fmt.Sprintf("commands:\n  test: %q\nauto_fix:\n  rebase: 0\n", "sh "+shellQuote(script)))
	base := h.UpstreamBranchSHA("main")
	target := h.CommitChange("main", "feature.txt", "conflicting target\n", "prepare target")
	gitIn(t, h, h.WorkDir, "push", "origin", target+":refs/heads/prepared-target")
	gitIn(t, h, h.WorkDir, "reset", "--hard", base)
	const branch = "feature/pending-pr-identity"
	h.CommitChange(branch, "feature.txt", "submitted feature\n", "submit feature")
	gitIn(t, h, h.WorkDir, "push", "origin", branch)
	source := h.UpstreamBranchSHA(branch)
	// Keep the submitted head unpublished so attach-time adoption cannot
	// make this fixture ineligible before it reaches the pending rewind.
	head := h.CommitChange(branch, "unpublished.txt", "local addition\n", "extend unpublished head")
	command := "#!/bin/sh\nset -eu\ngit --git-dir=" + shellQuote(h.UpstreamDir) + " update-ref refs/heads/main " + target + "\n"
	if err := os.WriteFile(script, []byte(command), 0700); err != nil {
		t.Fatal(err)
	}
	initFromOwnWorktree(t, h, "init-pending-pr")
	h.Checkout(branch)
	h.PushToGate(branch)
	parked := waitForStepStatus(t, h, branch, types.StepRebase, types.StepStatusAwaitingApproval, 90*time.Second)
	if parked.PRURL != nil {
		t.Fatal("run unexpectedly attached a PR before the rewind")
	}
	writeStatefulGH(t, state, statefulGHState{PRs: map[string]*statefulGHPR{branch: {Number: 42, URL: "https://github.com/example/closes/pull/42", Title: "New PR", Body: "opened during rewind", Base: "main", Head: branch, State: "OPEN"}}})
	h.Respond(parked.ID, types.StepRebase, types.ActionFix)
	terminal := h.WaitForRun(branch, 60*time.Second)
	if terminal.Status != types.RunFailed || terminal.Error == nil || !strings.Contains(*terminal.Error, "PR") || !strings.Contains(*terminal.Error, "changed") {
		t.Fatalf("new PR did not fail closed: %s %s", terminal.Status, deref(terminal.Error))
	}
	// Terminalization reconciles the managed head before persisting failure;
	// the worktree itself may already have been removed by daemon cleanup.
	if actual := terminal.HeadSHA; actual != head || h.UpstreamBranchSHA(branch) != source {
		t.Fatalf("PR identity refusal mutated a head: managed=%s expected=%s", actual, head)
	}
	charge, pending, history := targetRewindHistory(t, h.NMHome, terminal.ID)
	if charge != 1 || pending || history != 1 {
		t.Fatalf("terminal refusal lifecycle: charge=%d pending=%t history=%d", charge, pending, history)
	}
	t.Logf("new PR during pending rewind refused with unchanged managed head=%s and source=%s, one classification, cleared terminal pending state: %s", head, source, *terminal.Error)
}
