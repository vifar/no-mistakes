//go:build e2e && !windows

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #123 and its selected rollback fixes are the independent oracle.
// These journeys execute the real CLI/daemon; only the agent and forge are
// simulated. Recovery seeds are made with the owned daemon stopped.
func capJourneyCheck(action map[string]any) map[string]any {
	action["match"] = "Commit budget exhausted (read-only gap check)"
	return action
}

func capJourneyCalls(h *Harness) int {
	n := 0
	for _, call := range h.AgentInvocations() {
		if strings.Contains(call.Prompt, "Commit budget exhausted (read-only gap check)") {
			n++
		}
	}
	return n
}

func capJourneyRestored(t *testing.T, h *Harness, run *ipc.RunInfo, head string) {
	t.Helper()
	worktree := paths.WithRoot(h.NMHome).WorktreeDir(run.RepoID, run.ID)
	if got := gitIn(t, h, worktree, "rev-parse", "HEAD"); got != head {
		t.Fatalf("over-budget commit survived: got=%s want=%s", got, head)
	}
	if got := gitIn(t, h, worktree, "status", "--porcelain"); got != "" {
		t.Fatalf("over-budget edits survived: %s", got)
	}
	t.Logf("actual Git state: HEAD=%s; clean worktree; gap-check calls=%d", head, capJourneyCalls(h))
	if passes := documentJourneyPasses(t, h, run.ID); len(passes) != 0 {
		t.Fatalf("zero-budget check spent a pass: %+v", passes)
	}
}

func TestDocumentCapAcceptanceCleanAfterHeadAdvance(t *testing.T) {
	for _, kind := range []string{"rebase", "reviewed-code-fix-strict"} {
		t.Run(kind, func(t *testing.T) {
			scenario := documentJourneyScenario(t, "", documentJourneyAction("README.md", "accurate guide\n", "initial gap"))
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
			t.Cleanup(h.dumpDebugState)
			setupStatefulGitHub(t, h)
			commitTrustedRepoConfig(t, h, "document:\n  max_commits_per_run: 1\ncommands:\n  lint: 'true'\nno_ci: true\n")
			initFromOwnWorktree(t, h, "init-cap-acceptance")
			const branch = "feature/cap-acceptance"
			h.CommitChange(branch, "feature.txt", "feature\n", "feature")
			if kind == "reviewed-code-fix-strict" {
				out, err := h.RunInDir(h.AddWorktree(branch), "axi", "run", "--shipping-mode", "full", "--intent", "validate Document commit cap", "--wait", "10s")
				if err != nil && !(strings.Contains(out, "launch_receipt:") && (strings.Contains(out, "elapsed") || strings.Contains(out, "context deadline exceeded"))) {
					t.Fatalf("strict launch: %v\n%s", err, out)
				}
				t.Logf("actual strict launch:\n%s", out)
			} else {
				h.PushToGate(branch)
			}
			parked := waitForStepStatus(t, h, branch, types.StepDocument, types.StepStatusAwaitingApproval, 90*time.Second)
			passes := documentJourneyPasses(t, h, parked.ID)
			if len(passes) != 1 || passes[0].CommitCount != 1 {
				t.Fatalf("initial pass did not exhaust budget: %+v", passes)
			}
			gateJourneyDaemon(t, h, "stop")
			worktree := paths.WithRoot(h.NMHome).WorktreeDir(parked.RepoID, parked.ID)
			if kind == "rebase" {
				// Real Git replays the accepted commits onto an empty new base.
				base := gitIn(t, h, worktree, "merge-base", "HEAD", "origin/main")
				tree := gitIn(t, h, worktree, "rev-parse", base+"^{tree}")
				newBase := gitIn(t, h, worktree, "commit-tree", tree, "-p", base, "-m", "new target base")
				gitIn(t, h, worktree, "rebase", "--onto", newBase, base)
				if gitIn(t, h, worktree, "rev-parse", "HEAD^{tree}") != gitIn(t, h, worktree, "rev-parse", parked.HeadSHA+"^{tree}") {
					t.Fatal("rebase changed content")
				}
			} else {
				if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("reviewed repair\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitIn(t, h, worktree, "add", "feature.txt")
				gitIn(t, h, worktree, "commit", "-m", "repair feature")
			}
			advanced := gitIn(t, h, worktree, "rev-parse", "HEAD")
			if advanced == parked.HeadSHA {
				t.Fatal("head did not advance")
			}
			documentJourneyScenarioWith(t, scenario, nil, capJourneyCheck(documentJourneyAction("", "")))
			// Seed the recovery boundary at Review, so the real reviewer and
			// Test certify the changed head before Document sees it again.
			documentJourneyDB(t, h, func(database *db.DB) {
				rows, err := database.GetStepsByRun(parked.ID)
				if err != nil {
					t.Fatal(err)
				}
				var reviewID string
				var reviewFindings *string
				for _, row := range rows {
					if row.StepName == types.StepReview {
						reviewID = row.ID
						reviewFindings = row.FindingsJSON
					}
				}
				if err := database.UpdateRunHeadSHA(parked.ID, advanced); err != nil {
					t.Fatal(err)
				}
				if err := database.ResetStepsFrom(parked.ID, types.StepReview.Order()); err != nil {
					t.Fatal(err)
				}
				if err := database.StartStep(reviewID); err != nil {
					t.Fatal(err)
				}
				if err := database.ParkStepForApproval(parked.ID, reviewID, types.StepStatusAwaitingApproval, 0, 0, reviewFindings); err != nil {
					t.Fatal(err)
				}
			})
			gateJourneyDaemon(t, h, "start")
			documentJourneyNextGate(t, h, parked.ID, types.StepReview, "")
			h.Respond(parked.ID, types.StepReview, types.ActionFix)
			published := waitForStepStatus(t, h, branch, types.StepPR, types.StepStatusCompleted, 90*time.Second)
			if kind == "reviewed-code-fix-strict" && (published.Shipping == nil || published.Shipping.Mode != types.ShippingFull) {
				t.Fatal("live run did not use strict shipping")
			}
			if published.HeadSHA != advanced || h.UpstreamBranchSHA(branch) != advanced || capJourneyCalls(h) != 1 {
				t.Fatalf("cap failed to publish unchanged head: %+v calls=%d", published, capJourneyCalls(h))
			}
			if passes := documentJourneyPasses(t, h, parked.ID); len(passes) != 1 {
				t.Fatalf("gap check spent budget: %+v", passes)
			}
			documentJourneyStatus(t, h, published.ID)
		})
	}
}

func TestDocumentCapAcceptanceZeroBudgetAndRealGap(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: documentJourneyScenarioWith(t, "", nil, documentJourneyCapGapAction())})
	t.Cleanup(h.dumpDebugState)
	branch, head := documentJourneyStart(t, h, "document:\n  max_commits_per_run: 0\ncommands:\n  lint: 'true'\nno_ci: true\n")
	parked := waitForStepStatus(t, h, branch, types.StepDocument, types.StepStatusAwaitingApproval, 90*time.Second)
	step, _ := findStep(parked.Steps, types.StepDocument)
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 2 || findings.Items[0].Action != types.ActionAskUser || findings.Items[1].Action != types.ActionNoOp || findings.Items[1].Severity != "info" {
		t.Fatalf("gap/cap actions violate #123: %s", *step.FindingsJSON)
	}
	capJourneyRestored(t, h, parked, head)
	documentJourneyStatus(t, h, parked.ID)
	// Ordinary approval is allowed for the real gap; the cap is no blocker.
	h.Respond(parked.ID, types.StepDocument, types.ActionApprove)
	run := waitForStepStatus(t, h, branch, types.StepLint, types.StepStatusCompleted, 90*time.Second)
	documentJourneyStatus(t, h, run.ID)
}

func TestDocumentCapAcceptanceRollback(t *testing.T) {
	for _, kind := range []string{"reassess", "repeat-edit", "failed", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			scenario := documentJourneyScenarioWith(t, "", nil, documentJourneyCapGapAction())
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
			t.Cleanup(h.dumpDebugState)
			branch, head := documentJourneyStart(t, h, "document:\n  max_commits_per_run: 0\ncommands:\n  lint: 'true'\n")
			parked := waitForStepStatus(t, h, branch, types.StepDocument, types.StepStatusAwaitingApproval, 90*time.Second)
			action := capJourneyCheck(documentJourneyAction("README.md", "forbidden repair\n"))
			action["git"] = [][]string{{"add", "README.md"}, {"commit", "-m", "forbidden over-budget commit"}}
			commands := action["git"].([][]string)
			switch kind {
			case "reassess":
				// The first agent claims clean after editing. Its successor
				// refuses to answer unless the original Git bytes are restored.
				next := documentJourneyCapGapAction()
				next["git"] = [][]string{{"diff", "--exit-code", head, "--", "README.md"}}
				nextScenario := documentJourneyScenarioWith(t, "", nil, next)
				commands = append(commands, []string{"-c", "alias.nm-next-check=!cp " + shellQuote(nextScenario) + " " + shellQuote(scenario), "nm-next-check"})
			case "failed":
				commands = append(commands, []string{"-c", "alias.fail=!exit 19", "fail"})
			case "cancelled":
				commands = append(commands, []string{"-c", "alias.hold=!sleep 20", "hold"})
			}
			action["git"] = commands
			documentJourneyScenarioWith(t, scenario, nil, action)
			previous := documentJourneyFix(t, h, parked)
			switch kind {
			case "reassess":
				parked = documentJourneyNextGate(t, h, parked.ID, types.StepDocument, previous)
				if capJourneyCalls(h) != 3 {
					t.Fatalf("expected initial check plus one reassessment pair, got %d", capJourneyCalls(h))
				}
			case "repeat-edit":
				parked = documentJourneyFailed(t, h, branch, "edited the worktree twice")
				if capJourneyCalls(h) != 3 {
					t.Fatal("reassessment was not bounded")
				}
			case "failed":
				parked = documentJourneyFailed(t, h, branch, "agent document gap check")
			case "cancelled":
				worktree := paths.WithRoot(h.NMHome).WorktreeDir(parked.RepoID, parked.ID)
				deadline := time.Now().Add(30 * time.Second)
				for gitIn(t, h, worktree, "rev-parse", "HEAD") == head {
					if time.Now().After(deadline) {
						t.Fatal("agent never created the cancellation probe commit")
					}
					time.Sleep(20 * time.Millisecond)
				}
				t.Log("canceling the owned daemon while its gap check holds an over-budget commit")
				gateJourneyDaemon(t, h, "stop")
			}
			capJourneyRestored(t, h, parked, head)
			if kind != "cancelled" {
				documentJourneyStatus(t, h, parked.ID)
			}
			t.Logf("%s: restored original head and bytes without a Document pass", kind)
		})
	}
}
