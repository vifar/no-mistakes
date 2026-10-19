//go:build e2e && !windows

package e2e

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// An empty retained commit has the original target's tree but is still a
// mandatory ancestor. Native Git ancestry and tree equality are the oracles;
// a tree-only terminal shortcut or an original-target continuity check fails
// these journeys through the real binary and daemon.
func TestRetainedTargetContinuityLiveJourney(t *testing.T) {
	for _, name := range []string{"open sibling", "open rollback", "open consecutive", "merge rollback", "squash rollback", "rebase rollback", "merge included", "squash included", "rebase included"} {
		t.Run(name, func(t *testing.T) {
			j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
			j.setTrusted("commands:\n  test: 'true'\n  lint: 'true'\n")
			j.setForge(forgeState{Protection: &forgeEndpoint{Body: []byte(`{"strict":false,"contexts":[],"checks":[]}`)}, Checks: []forgeCheck{greenCheck("build")}})
			j.init()
			const branch = "feature/retained-continuity"
			j.push(branch)
			run := j.expectReady(branch, 90*time.Second)
			database, err := db.OpenReadOnly(filepath.Join(j.h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			receipt, err := database.GetRunPRContext(run.ID)
			if err != nil || receipt == nil || !receipt.CIQualified {
				t.Fatalf("missing qualification: %+v %v", receipt, err)
			}
			head := j.h.UpstreamBranchSHA(branch)
			body := livePRBody(t, j.statePath, branch)
			gitIn(t, j.h, j.h.WorkDir, "checkout", "main")
			gitIn(t, j.h, j.h.WorkDir, "commit", "--allow-empty", "-m", "retained empty T1")
			first := gitIn(t, j.h, j.h.WorkDir, "rev-parse", "HEAD")
			gitIn(t, j.h, j.h.WorkDir, "push", "origin", "main")
			wait := func(label string, predicate func(context.Context) bool) {
				t.Helper()
				deadline := pollDeadline(t, 90*time.Second)
				ctx, cancel := context.WithDeadline(t.Context(), deadline)
				defer cancel()
				for time.Now().Before(deadline) {
					if predicate(ctx) {
						return
					}
					time.Sleep(200 * time.Millisecond)
				}
				t.Fatalf("timed out waiting for %s (run %s):\n%s", label, run.ID, j.ciLog(run.ID))
			}
			wait("empty T1 retained with readiness", func(ctx context.Context) bool {
				retained, err := database.GetRetainedPRTarget(run.ID, receipt)
				info, readErr := j.h.runInfoWithContext(ctx, run.ID)
				return err == nil && retained == first && readErr == nil && info.CIReady
			})
			parts := strings.Fields(name)
			open := parts[0] == "open"
			losesTarget := parts[1] == "rollback" || parts[1] == "sibling"
			if losesTarget {
				gitIn(t, j.h, j.h.WorkDir, "reset", "--hard", receipt.TargetSHA)
			}
			if open {
				if parts[1] != "rollback" {
					gitIn(t, j.h, j.h.WorkDir, "commit", "--allow-empty", "-m", "observed T2")
				}
			} else {
				switch parts[0] {
				case "merge":
					gitIn(t, j.h, j.h.WorkDir, "merge", "--no-ff", branch, "-m", "forge merge")
				case "squash":
					gitIn(t, j.h, j.h.WorkDir, "merge", "--squash", branch)
					gitIn(t, j.h, j.h.WorkDir, "commit", "-m", "forge squash")
				case "rebase":
					for _, commit := range strings.Fields(gitIn(t, j.h, j.h.WorkDir, "rev-list", "--reverse", receipt.TargetSHA+".."+head)) {
						gitIn(t, j.h, j.h.WorkDir, "-c", "user.name=Disposable forge", "cherry-pick", commit)
					}
				}
			}
			observed := gitIn(t, j.h, j.h.WorkDir, "rev-parse", "HEAD")
			included := isAncestorIn(t, j.h, j.h.WorkDir, first, observed)
			if included == losesTarget {
				t.Fatalf("invalid ancestry oracle: T1=%s observed=%s included=%t", first, observed, included)
			}
			if !open && gitIn(t, j.h, j.h.WorkDir, "rev-parse", observed+"^{tree}") != gitIn(t, j.h, j.h.WorkDir, "rev-parse", head+"^{tree}") {
				t.Fatal("terminal fixture must have the original reviewed result tree")
			}
			gitIn(t, j.h, j.h.WorkDir, "push", "--force", "origin", "main")
			if !open {
				state := readStatefulGH(t, j.statePath)
				state.PRs[branch].State = "MERGED"
				state.PRs[branch].MergeCommitSHA = observed
				writeStatefulGH(t, j.statePath, state)
			}
			if open {
				wait("continuity verdict", func(ctx context.Context) bool {
					info, err := j.h.runInfoWithContext(ctx, run.ID)
					retained, readErr := database.GetRetainedPRTarget(run.ID, receipt)
					if losesTarget {
						return err == nil && !info.CIReady && readErr == nil && retained == first && strings.Contains(j.ciLog(run.ID), "published target did not fast-forward")
					}
					return err == nil && info.CIReady && readErr == nil && retained == observed
				})
			} else {
				terminal := j.h.WaitForRun(branch, 90*time.Second)
				want := types.RunCompleted
				if losesTarget {
					want = types.RunFailed
				}
				if terminal.Status != want || losesTarget && !strings.Contains(deref(terminal.Error), "merged result does not contain the verified advanced target") {
					t.Fatalf("terminal status=%s want=%s error=%s\n%s", terminal.Status, want, deref(terminal.Error), j.ciLog(run.ID))
				}
			}
			current, err := database.GetRunPRContext(run.ID)
			if err != nil || !reflect.DeepEqual(current, receipt) || j.h.UpstreamBranchSHA(branch) != head || livePRBody(t, j.statePath, branch) != body {
				t.Fatalf("continuity observation changed qualification, head, or attestation: %+v -> %+v %v", receipt, current, err)
			}
			out, err := j.h.Run("axi", "status", "--run", run.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: T0=%s retained_empty_T1=%s observed=%s T1_ancestor=%t receipt_unchanged=true\n%s\n%s", name, receipt.TargetSHA, first, observed, included, out, j.ciLog(run.ID))
			if open {
				j.finish(branch)
			}
		})
	}
}
