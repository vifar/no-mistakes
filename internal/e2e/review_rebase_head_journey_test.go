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
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The issue #99 oracle is an actual two-parent merge: the analyzer must see
// that head exactly once when durable state proves it, and never run when it
// does not. The external Git shim returns the submitted head for exactly one
// post-merge observation to reconstruct the measured stale execution snapshot.
// It does not reproduce the unknown original caller. All other Git operations,
// the CLI, daemon, database and Review are real; agents and forge are simulated.
func TestReviewPostMergeStaleSnapshotJourney(t *testing.T) {
	for _, durableProof := range []bool{true, false} {
		name := "unproven_head_refuses_without_analyzer"
		if durableProof {
			name = "proven_head_reviews_merge_once"
		}
		t.Run(name, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude"})
			commitTrustedRepoConfig(t, h, "rebase:\n  strategy: merge\n")
			initFromOwnWorktree(t, h, "init-review-head")
			const branch = "feature/review-head"
			submitted := h.CommitChange(branch, "feature.txt", "feature work\n", "add feature work")
			mainSHA := advanceMain(t, h, "main-advance.txt", "main advanced\n", "advance main")
			h.Checkout(branch)
			realGit, err := testgit.RealGit()
			if err != nil {
				t.Fatal(err)
			}
			armed := filepath.Join(h.HomeDir, "merge-done")
			request := filepath.Join(h.HomeDir, "snapshot-request")
			release := filepath.Join(h.HomeDir, "snapshot-release")
			injected := filepath.Join(h.HomeDir, "snapshot-injected")
			shim := fmt.Sprintf(`#!/bin/sh
case "$*" in
  'merge --no-ff --no-edit origin/main')
    %s "$@" || exit "$?"
    : > %s
    exit 0 ;;
  'rev-parse HEAD')
    if [ -f %s ] && [ ! -f %s ]; then
      : > %s
      %s rev-parse HEAD > %s
      n=0
      while [ ! -f %s ]; do
        n=$((n+1))
        if [ "$n" -gt 400 ]; then exit 91; fi
        sleep 0.05
      done
      printf '%%s\n' %s
      exit 0
    fi ;;
esac
exec %s "$@"
`, shellQuote(realGit), shellQuote(armed), shellQuote(armed), shellQuote(injected), shellQuote(injected), shellQuote(realGit), shellQuote(request), shellQuote(release), shellQuote(submitted), shellQuote(realGit))
			if err := os.WriteFile(filepath.Join(h.BinDir, "git"), []byte(shim), 0o700); err != nil {
				t.Fatal(err)
			}
			h.PushToGate(branch)
			var merged string
			deadline := time.Now().Add(60 * time.Second)
			for time.Now().Before(deadline) {
				data, readErr := os.ReadFile(request)
				if readErr == nil && strings.TrimSpace(string(data)) != "" {
					merged = strings.TrimSpace(string(data))
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if merged == "" {
				t.Fatal("real merge did not reach the snapshot injection")
			}
			database, err := db.Open(filepath.Join(h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			run, err := database.GetActiveRun(h.repoID(), branch)
			if err != nil || run == nil {
				t.Fatalf("active fixture run: %+v %v", run, err)
			}
			if durableProof {
				if err := database.UpdateRunHeadSHA(run.ID, merged); err != nil {
					t.Fatal(err)
				}
			}
			before, err := database.GetRun(run.ID)
			if err != nil || before.ReviewApprovedHeadSHA != nil {
				t.Fatalf("fixture acquired approval before Review: %+v %v", before, err)
			}
			// Inspect the merge while the shim holds the run open; terminal
			// cleanup may remove the worktree as soon as we release it.
			ps := parentsOf(t, h, *run.WorktreeDir, merged)
			if len(ps) != 2 || ps[0] != submitted || ps[1] != mainSHA {
				t.Fatalf("real merge parents=%v, want [%s %s]", ps, submitted, mainSHA)
			}
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			finished := h.WaitForRun(branch, 90*time.Second)
			stored, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			pin, err := database.GetRunPRContext(run.ID)
			if err != nil || pin == nil || pin.Generation != 2 || pin.LocalHeadSHA != merged {
				t.Fatalf("generation-two merge pin: %+v %v", pin, err)
			}
			reviews := 0
			for _, invocation := range h.AgentInvocations() {
				if strings.Contains(invocation.Prompt, "Review the code changes and return structured findings") {
					reviews++
					if !strings.Contains(invocation.Prompt, "- target commit: "+merged+"\n") {
						t.Fatalf("analyzer did not receive actual merge %s", merged)
					}
				}
			}
			if durableProof {
				if finished.Status != types.RunCompleted || stored.HeadSHA != merged || stored.ReviewApprovedHeadSHA == nil || *stored.ReviewApprovedHeadSHA != merged || reviews != 1 {
					t.Fatalf("proven snapshot: status=%s error=%s stored=%+v analyzer_calls=%d", finished.Status, deref(finished.Error), stored, reviews)
				}
			} else {
				if finished.Status != types.RunFailed || finished.Error == nil || !strings.Contains(*finished.Error, "review_head_precondition:") || !strings.Contains(*finished.Error, submitted) || !strings.Contains(*finished.Error, merged) || reviews != 0 || stored.ReviewApprovedHeadSHA != nil {
					t.Fatalf("unproven snapshot: status=%s error=%s approval=%v analyzer_calls=%d", finished.Status, deref(finished.Error), stored.ReviewApprovedHeadSHA, reviews)
				}
			}
			t.Logf("real daemon result: status=%s submitted=%s merge=%s parents=%v pin=%s generation=%d durable_proof=%t analyzer_calls=%d approved=%s error=%s", finished.Status, submitted, merged, ps, pin.LocalHeadSHA, pin.Generation, durableProof, reviews, deref(stored.ReviewApprovedHeadSHA), deref(finished.Error))
			out, statusErr := h.Run("axi", "status")
			t.Logf("operator axi status (error=%v):\n%s", statusErr, out)
		})
	}
}

// An answer belongs to the source the reviewer asked about. If the comparison
// guard detects a changed source before Review enters, it must start a fresh
// review rather than resume the old asking pass against different code.
func TestReviewAnswerRestartsForChangedSourceHeadJourney(t *testing.T) {
	h, branch := answerFollowHarness(t, reviewConversationScenario(t))
	h.PushToGate(branch)
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	database, err := db.Open(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stored, err := database.GetRun(parked.ID)
	if err != nil || stored == nil || stored.WorktreeDir == nil {
		t.Fatalf("parked run: %+v %v", stored, err)
	}
	dir := *stored.WorktreeDir
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("flag = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, h, dir, "add", "feature.txt")
	gitIn(t, h, dir, "commit", "-m", "change the flag while its question is pending")
	changed := gitIn(t, h, dir, "rev-parse", "HEAD")
	if err := database.UpdateRunHeadSHA(stored.ID, changed); err != nil {
		t.Fatal(err)
	}
	out, answerErr := h.Run("axi", "answer", "--question", "q1", "--answer", conversationAnswerText)
	if answerErr != nil || !strings.Contains(out, "question-q1") {
		t.Fatalf("changed-source answer did not return a new question gate: %v\n%s", answerErr, out)
	}
	waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 30*time.Second)
	reviews := 0
	for _, invocation := range h.AgentInvocations() {
		if strings.Contains(invocation.Prompt, "Review the code changes and return structured findings") {
			reviews++
			if reviews == 2 {
				if !strings.Contains(invocation.Prompt, "- target commit: "+changed+"\n") || strings.Contains(invocation.Prompt, "Answers to the questions you asked in this pass") {
					t.Fatal("changed source received a stale finalize prompt instead of a fresh review")
				}
				for _, arg := range invocation.Args {
					if arg == "--resume" {
						t.Fatal("changed source resumed the old reviewer session")
					}
				}
			}
		}
	}
	after, err := database.GetRun(stored.ID)
	if err != nil || after.ReviewApprovedHeadSHA != nil || reviews != 2 {
		t.Fatalf("changed source did not receive one fresh review without approval: calls=%d stored=%+v error=%v", reviews, after, err)
	}
	pin, err := database.GetRunPRContext(stored.ID)
	if err != nil || pin == nil || pin.LocalHeadSHA != changed || pin.Generation != 2 {
		t.Fatalf("fresh review comparison: %+v %v", pin, err)
	}
	t.Logf("asking_head=%s changed_head=%s generation=%d analyzer_calls=%d (old asking turn plus fresh cold review); no stale finalize, approval absent, new question parked", stored.HeadSHA, changed, pin.Generation, reviews)
	t.Logf("operator axi answer (error=%v):\n%s", answerErr, out)
}
