//go:build e2e && !windows

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These journeys exercise the two data-preservation regressions through the
// real daemon. The independent oracle is Git ancestry and the published tree,
// rather than the pipeline's own claim that integration completed.
func TestTargetAdvanceAdditionalPreservationJourney(t *testing.T) {
	for _, strategy := range []string{"rebase", "merge"} {
		for _, kind := range []string{"direct main", "source then conflict"} {
			t.Run(strategy+"/"+kind, func(t *testing.T) {
				h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
				script := filepath.Join(t.TempDir(), "advance.sh")
				commitTrustedRepoConfig(t, h, fmt.Sprintf("commands:\n  test: %q\nrebase:\n  strategy: %s\nauto_fix:\n  rebase: 0\n", "sh "+shellQuote(script), strategy))
				base := h.UpstreamBranchSHA("main")
				targetPath := "landed.txt"
				if kind == "source then conflict" {
					targetPath = "feature.txt"
				}
				target := h.CommitChange("main", targetPath, "landed target\n", "prepare target advance")
				gitIn(t, h, h.WorkDir, "push", "origin", target+":refs/heads/prepared-target")
				gitIn(t, h, h.WorkDir, "reset", "--hard", base)
				branch := "main"
				if kind == "source then conflict" {
					branch = "feature/partial-integration"
				}
				head := h.CommitChange(branch, "feature.txt", "submitted feature\n", "submit feature")
				commands := "#!/bin/sh\nset -eu\ngit --git-dir=" + shellQuote(h.UpstreamDir) + " update-ref refs/heads/main " + target + "\n"
				if kind == "source then conflict" {
					source := h.CommitChange(branch, "source.txt", "new source\n", "prepare source advance")
					gitIn(t, h, h.WorkDir, "push", "origin", source+":refs/heads/prepared-source")
					gitIn(t, h, h.WorkDir, "reset", "--hard", head)
					commands += "git --git-dir=" + shellQuote(h.UpstreamDir) + " update-ref refs/heads/" + branch + " " + source + "\n"
				}
				if err := os.WriteFile(script, []byte(commands), 0700); err != nil {
					t.Fatal(err)
				}
				initFromOwnWorktree(t, h, "init-preservation")
				h.Checkout(branch)
				h.PushToGate(branch)
				if kind == "source then conflict" {
					run := waitForStepStatus(t, h, branch, types.StepRebase, types.StepStatusAwaitingApproval, 90*time.Second)
					worktree := paths.WithRoot(h.NMHome).WorktreeDir(run.RepoID, run.ID)
					actual := gitIn(t, h, worktree, "rev-parse", "HEAD")
					if actual != head || run.HeadSHA != head {
						t.Fatalf("partial integration was not restored: actual=%s recorded=%s entry=%s", actual, run.HeadSHA, head)
					}
					charge, pending, history := targetRewindHistory(t, h.NMHome, run.ID)
					if charge != 1 || !pending || history != 1 {
						t.Fatalf("conflict lost pending rewind: %d %t %d", charge, pending, history)
					}
					t.Logf("%s: conflict parked after source integration; managed and recorded heads restored to %s; target=%s", strategy, head, target)
					logTargetRewindStatus(t, h, run.ID)
					h.Respond(run.ID, types.StepRebase, types.ActionAbort)
					terminal := h.WaitForRun(branch, 30*time.Second)
					if terminal.Status != types.RunFailed {
						t.Fatalf("abort did not terminate: %s", terminal.Status)
					}
					return
				}
				run := h.WaitForRun(branch, 120*time.Second)
				// Publishing main advances the comparison target itself. The
				// existing post-publication refusal remains in force; the rewind
				// contract requires preservation before that publication.
				if run.Status != types.RunFailed || run.Error == nil || (!strings.Contains(*run.Error, "check PR context after push:") || !strings.Contains(*run.Error, "head already published")) {
					t.Fatalf("unexpected direct-main outcome: %s %s", run.Status, deref(run.Error))
				}
				published := h.UpstreamBranchSHA("main")
				if published != run.HeadSHA || published == head {
					t.Fatalf("integrated head was not published: remote=%s recorded=%s submitted=%s", published, run.HeadSHA, head)
				}
				if !isAncestorIn(t, h, h.UpstreamDir, target, published) {
					t.Fatal("published main discarded the newly landed target")
				}
				if got := gitIn(t, h, h.UpstreamDir, "show", published+":landed.txt"); got != "landed target" {
					t.Fatalf("target content lost: %q", got)
				}
				if got := gitIn(t, h, h.UpstreamDir, "show", published+":feature.txt"); got != "submitted feature" {
					t.Fatalf("submitted content lost: %q", got)
				}
				charge, pending, history := targetRewindHistory(t, h.NMHome, run.ID)
				if charge != 1 || pending || history != 1 {
					t.Fatalf("direct-main rewind lifecycle: %d %t %d", charge, pending, history)
				}
				t.Logf("%s: published main=%s contains target=%s and both landed.txt and feature.txt; one rewind, no pending integration; existing post-publication comparison refusal retained", strategy, published, target)
				logTargetRewindStatus(t, h, run.ID)
			})
		}
	}
}

// Skipping a gate is a decision about the old comparison. After a target
// advance during Document, the gate must execute again in live and recovered
// execution, while old evidence remains only in the superseded generation.
func TestTargetAdvanceRecoveredCustomGateJourney(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(fmt.Sprintf("recover=%t", recover), func(t *testing.T) {
			targetAdvanceCustomGateJourney(t, recover)
		})
	}
}

func targetAdvanceCustomGateJourney(t *testing.T, recover bool) {
	t.Helper()
	scenario := cleanReviewScenario(t)
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	count := filepath.Join(t.TempDir(), "gate-invocations")
	extra := fmt.Sprintf("commands:\n  test: 'false'\n  lint: 'true'\ngates:\n  - name: audit\n    after: review\n    command: %q\nauto_fix:\n  test: 0\n  gates:\n    audit: 0\n", "printf 'invoked\\n' >> "+shellQuote(count)+"; exit 1")
	commitTrustedRepoConfig(t, h, extra)
	base := h.UpstreamBranchSHA("main")
	target := h.CommitChange("main", "landed.txt", "landed target\n", "prepare target advance")
	gitIn(t, h, h.WorkDir, "push", "origin", target+":refs/heads/prepared-target")
	gitIn(t, h, h.WorkDir, "reset", "--hard", base)
	const branch = "feature/recovered-audit"
	h.CommitChange(branch, "feature.txt", "submitted feature\n", "submit feature")
	oldScenario, err := os.ReadFile(scenario)
	if err != nil {
		t.Fatal(err)
	}
	documentAction := fmt.Sprintf("actions:\n  - match: 'Keep the project documentation accurate for this change.'\n    git:\n      - [\"--git-dir=%s\", update-ref, refs/heads/main, %s]\n    structured:\n      findings: []\n      summary: 'target advanced during Document'\n", h.UpstreamDir, target)
	if err := os.WriteFile(scenario, []byte(strings.Replace(string(oldScenario), "actions:\n", documentAction, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	initFromOwnWorktree(t, h, "init-recovered-audit")
	h.Checkout(branch)
	h.PushToGate(branch)
	gate := types.CustomGateStepName(types.StepReview, "audit")
	first := waitForStepStatus(t, h, branch, gate, types.StepStatusAwaitingApproval, 90*time.Second)
	h.Respond(first.ID, gate, types.ActionSkip)
	parked := waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 90*time.Second)
	// Seed the legacy generation-zero evidence layout a pre-upgrade parked
	// run uses. The real executor must archive it when comparison changes.
	evidenceRoot := paths.WithRoot(h.NMHome).RunEvidenceDir("", parked.ID)
	if err := os.MkdirAll(evidenceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidenceRoot, "legacy.log"), []byte("old comparison evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if recover {
		gateJourneyDaemon(t, h, "stop")
		gateJourneyDaemon(t, h, "start")
	}
	waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 90*time.Second)
	deadline := time.Now().Add(15 * time.Second)
	for {
		err = h.RespondError(parked.ID, types.StepTest, types.ActionSkip)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered Test gate never admitted skip: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	second := waitForStepStatus(t, h, branch, gate, types.StepStatusAwaitingApproval, 90*time.Second)
	charge, pending, history := targetRewindHistory(t, h.NMHome, parked.ID)
	if charge != 1 || pending || history != 1 || second.ID != parked.ID {
		t.Fatalf("custom gate did not belong to the rewound run: %d %t %d %s", charge, pending, history, second.ID)
	}
	invocations, err := os.ReadFile(count)
	if err != nil || strings.Count(string(invocations), "invoked") != 2 {
		t.Fatalf("skipped gate was not executed again: %q %v", invocations, err)
	}
	archived := filepath.Join(evidenceRoot, "superseded", "g0", "legacy.log")
	if got, err := os.ReadFile(archived); err != nil || string(got) != "old comparison evidence\n" {
		t.Fatalf("generation-zero evidence lost: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(evidenceRoot, "g1", "legacy.log")); !os.IsNotExist(err) {
		t.Fatalf("old evidence leaked into current generation: %v", err)
	}
	t.Logf("run=%s recover=%t, advanced target during Document, reran skipped audit gate; legacy evidence archived under superseded/g0 and absent from g1", second.ID, recover)
	logTargetRewindStatus(t, h, second.ID)
	h.Respond(second.ID, gate, types.ActionSkip)
	// Test's configured command always fails. Completion therefore proves
	// execution honored the operator's durable skip after the rewind, rather
	// than merely preserving a skipped database row until the next gate.
	if terminal := h.WaitForRun(branch, 120*time.Second); terminal.Status != types.RunCompleted {
		t.Fatalf("rewound run did not preserve the Test skip: %s %s", terminal.Status, deref(terminal.Error))
	}
}

func logTargetRewindStatus(t *testing.T, h *Harness, runID string) {
	t.Helper()
	out, err := h.Run("axi", "status", "--run", runID)
	if err != nil {
		t.Fatalf("read run status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "target_advance_rewinds: 1") {
		t.Fatalf("status omitted the committed rewind: %s", out)
	}
	t.Logf("axi status --run %s:\n%s", runID, out)
}
