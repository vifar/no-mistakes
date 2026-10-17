//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTargetAdvanceRewindSafetyOrderingJourney(t *testing.T) {
	for _, strategy := range []string{"rebase", "merge"} {
		for _, kind := range []string{"force-rewritten main", "approved bundled commits"} {
			for _, eligible := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/eligible=%t", strategy, kind, eligible), func(t *testing.T) {
					h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
					t.Cleanup(func() {
						if !t.Failed() {
							return
						}
						h.dumpDebugState()
						for _, run := range h.Runs() {
							if state, err := json.MarshalIndent(h.RunInfo(run.ID), "", "  "); err == nil {
								t.Logf("run and gate diagnostics:\n%s", state)
							}
						}
					})
					script := filepath.Join(t.TempDir(), "advance.sh")
					limit := 3
					if !eligible {
						limit = 0
					}
					commitTrustedRepoConfig(t, h, fmt.Sprintf("commands:\n  test: %q\nrebase:\n  strategy: %s\n  max_target_advance_rewinds: %d\nauto_fix:\n  rebase: 0\n", "sh "+shellQuote(script), strategy, limit))
					base := h.UpstreamBranchSHA("main")
					branch := "main"
					if kind == "force-rewritten main" {
						base = h.CommitChange("main", "old-main.txt", "old main\n", "old main history")
						gitIn(t, h, h.WorkDir, "push", "origin", "main")
					} else {
						h.CommitChange("main", "bundled.txt", "approved bundle\n", "unpublished local main work")
						branch = "feature/approved-bundle"
					}
					initFromOwnWorktree(t, h, "init-safety-order")
					if kind == "force-rewritten main" {
						h.Checkout("main")
						h.PushToGate("main")
						if seed := h.WaitForRun("main", 60*time.Second); seed.Status != types.RunCompleted || seed.HeadSHA != base {
							t.Fatalf("could not seed prior gate main: %s %s", seed.Status, deref(seed.Error))
						}
					}
					gitIn(t, h, h.WorkDir, "checkout", "-b", "prepared-target", base)
					target := h.CommitChange("prepared-target", "landed.txt", "landed target\n", "prepare target advance")
					gitIn(t, h, h.WorkDir, "push", "origin", "prepared-target")
					h.Checkout("main")
					if kind == "force-rewritten main" {
						gitIn(t, h, h.WorkDir, "reset", "--hard", base+"^")
					}
					head := h.CommitChange(branch, "feature.txt", "submitted feature\n", "submit intended work")
					if kind == "force-rewritten main" {
						if got := gitIn(t, h, h.WorkDir, "ls-remote", "no-mistakes", "refs/heads/main"); !strings.HasPrefix(got, base+"\t") {
							t.Fatalf("force-rewrite fixture requires gate main=%s, got %q", base, got)
						}
					}
					command := "#!/bin/sh\nset -eu\ngit --git-dir=" + shellQuote(h.UpstreamDir) + " update-ref refs/heads/main " + target + "\n"
					if err := os.WriteFile(script, []byte(command), 0700); err != nil {
						t.Fatal(err)
					}
					gitIn(t, h, h.WorkDir, "push", "--force", "no-mistakes", branch)
					if kind == "approved bundled commits" {
						parked := waitForStepStatus(t, h, branch, types.StepRebase, types.StepStatusAwaitingApproval, 90*time.Second)
						if charge, pending, history := targetRewindHistory(t, h.NMHome, parked.ID); charge != 0 || pending || history != 0 {
							t.Fatalf("initial safety decision unexpectedly rewound: %d %t %d", charge, pending, history)
						}
						found := false
						for _, step := range h.RunInfo(parked.ID).Steps {
							if step.StepName == types.StepRebase && step.FindingsJSON != nil && strings.Contains(*step.FindingsJSON, "unintended bundled work") {
								found = true
							}
						}
						if !found {
							t.Fatal("initial park did not preserve the bundled-commit safety gate")
						}
						h.Respond(parked.ID, types.StepRebase, types.ActionApprove)
					}
					deadline := time.Now().Add(120 * time.Second)
					for {
						active := h.ActiveRun(branch)
						if active != nil {
							if active.Status.Terminal() {
								break
							}
							charge, pending, _ := targetRewindHistory(t, h.NMHome, active.ID)
							if pending && charge > 0 {
								for _, step := range h.RunInfo(active.ID).Steps {
									if step.StepName == types.StepRebase && (step.Status == types.StepStatusAwaitingApproval || step.Status == types.StepStatusFixReview) {
										for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionSkip} {
											if err := h.RespondError(active.ID, types.StepRebase, action); err == nil {
												t.Fatalf("pending rewind admitted %s", action)
											}
										}
										t.Fatalf("eligible rewind permanently parked before checked integration: %s", deref(step.FindingsJSON))
									}
								}
							}
						} else {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("run did not finish after target advancement")
						}
						time.Sleep(100 * time.Millisecond)
					}
					run := h.WaitForRun(branch, 10*time.Second)
					if run.SubmittedHeadSHA == nil || *run.SubmittedHeadSHA != head {
						t.Fatalf("observed a different submission: %+v", run.SubmittedHeadSHA)
					}
					if kind == "force-rewritten main" && run.BaseSHA != base {
						t.Fatalf("force-rewrite fixture recorded BaseSHA=%s, want remote main=%s", run.BaseSHA, base)
					}
					charge, pending, history := targetRewindHistory(t, h.NMHome, run.ID)
					if !eligible {
						if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "PR comparison changed after rebase") || charge != 0 || pending || history != 0 {
							t.Fatalf("ineligible advance proceeded: status=%s error=%s history=%d/%t/%d", run.Status, deref(run.Error), charge, pending, history)
						}
						if h.UpstreamBranchSHA("main") != target {
							t.Fatal("ineligible run published over the target")
						}
						return
					}
					if kind == "force-rewritten main" {
						if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "check PR context after push: PR comparison changed after rebase") {
							t.Fatalf("direct-main post-publication behavior changed: %s %s", run.Status, deref(run.Error))
						}
					} else if run.Status != types.RunCompleted {
						t.Fatalf("approved bundle failed after rewind: %s %s", run.Status, deref(run.Error))
					}
					published := h.UpstreamBranchSHA(branch)
					if charge != 1 || pending || history != 1 || published != run.HeadSHA || published == head || !isAncestorIn(t, h, h.UpstreamDir, target, published) {
						t.Fatalf("checked integration missing: history=%d/%t/%d published=%s head=%s target=%s", charge, pending, history, published, run.HeadSHA, target)
					}
					files := map[string]string{"landed.txt": "landed target", "feature.txt": "submitted feature"}
					if kind == "approved bundled commits" {
						files["bundled.txt"] = "approved bundle"
					} else {
						files["old-main.txt"] = "old main"
					}
					for path, want := range files {
						if got := gitIn(t, h, h.UpstreamDir, "show", published+":"+path); got != want {
							t.Fatalf("published %s=%q, want %q", path, got, want)
						}
					}
				})
			}
		}
	}
}
