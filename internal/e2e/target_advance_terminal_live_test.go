//go:build e2e && !windows

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The acceptance oracle is a terminal completed run at the original qualified
// comparison after the forge reports an exact, independently constructed Git
// merge. A corrupt result or an unobserved advanced target must fail closed.
// Only the external forge and agent are simulated; the binary, daemon,
// readiness transitions, comparison persistence, and terminal guard are real.
func TestTargetAdvanceTerminalMergeLiveJourney(t *testing.T) {
	for _, name := range []string{"exact target", "merge recovery", "squash", "rebase", "later target", "tampered result", "corrupt rebase", "missing observation", "stacked merge recovery", "stacked squash", "stacked rebase", "stacked later target", "stacked tampered result", "stacked corrupt rebase", "stacked missing observation"} {
		t.Run(name, func(t *testing.T) {
			mode := strings.TrimPrefix(name, "stacked ")
			stacked := mode != name
			j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
			j.setTrusted("commands:\n  test: 'true'\n  lint: 'true'\n")
			healthy := forgeState{Protection: &forgeEndpoint{Body: []byte(`{"strict":false,"contexts":[],"checks":[]}`)}, Checks: []forgeCheck{greenCheck("build")}}
			j.setForge(healthy)
			j.init()
			const branch = "feature/terminal-advance"
			first := j.h.CommitChange(branch, "first.txt", "first reviewed change\n", "first feature commit")
			j.h.CommitChange(branch, "second.txt", "second reviewed change\n", "second feature commit")
			if stacked && (mode == "rebase" || mode == "corrupt rebase") {
				// Keep two unapplied commits so a corrupt first rebased commit
				// cannot hide behind an otherwise correct final commit.
				j.h.CommitChange(branch, "third.txt", "third reviewed change\n", "third feature commit")
			}
			j.h.PushToGate(branch)
			run := j.expectReady(branch, 90*time.Second)
			database, err := db.OpenReadOnly(filepath.Join(j.h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			original, err := database.GetRunPRContext(run.ID)
			if err != nil || original == nil || !original.CIQualified {
				t.Fatalf("missing qualification: %+v %v", original, err)
			}
			head := j.h.UpstreamBranchSHA(branch)
			body := livePRBody(t, j.statePath, branch)
			statusWrites := j.ghCalls("/statuses/")
			if mode == "missing observation" {
				// Even a monitor poll during fixture construction cannot record
				// a verified target while live mergeability is unresolved.
				blocked := healthy
				blocked.Mergeable = "UNKNOWN"
				j.setForge(blocked)
			}
			target := original.TargetSHA
			if mode != "exact target" {
				if stacked {
					gitIn(t, j.h, j.h.WorkDir, "checkout", "main")
					gitIn(t, j.h, j.h.WorkDir, "merge", "--ff-only", first)
				}
				target = advanceMain(t, j.h, "target.txt", "advanced target\n", "advance target before merge")
			}
			waitUntil := func(label string, predicate func(context.Context) bool) {
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
				j.h.dumpDebugState()
				t.Fatalf("timed out waiting for %s (run %s):\n%s", label, run.ID, j.ciLog(run.ID))
			}
			if mode != "missing observation" && mode != "exact target" {
				waitUntil("verified advanced target", func(ctx context.Context) bool {
					got, err := database.GetRetainedPRTarget(run.ID, original)
					if err != nil || got != target {
						return false
					}
					info, err := j.h.runInfoWithContext(ctx, run.ID)
					return err == nil && info.CIReady
				})
			}
			if mode == "merge recovery" {
				broken := healthy
				broken.Protection = &forgeEndpoint{Status: 503, Message: "disposable protection outage"}
				j.setForge(broken)
				waitUntil("readiness cleared", func(ctx context.Context) bool {
					info, err := j.h.runInfoWithContext(ctx, run.ID)
					return err == nil && !info.CIReady
				})
				j.setForge(healthy)
				waitUntil("readiness recovered", func(ctx context.Context) bool {
					info, err := j.h.runInfoWithContext(ctx, run.ID)
					return err == nil && info.CIReady
				})
			}
			gitIn(t, j.h, j.h.WorkDir, "checkout", "main")
			switch mode {
			case "squash":
				gitIn(t, j.h, j.h.WorkDir, "merge", "--squash", branch)
				gitIn(t, j.h, j.h.WorkDir, "commit", "-m", "forge squash")
			case "rebase", "corrupt rebase":
				rebaseBase := original.MergeBaseSHA
				if stacked {
					rebaseBase = first
				}
				commits := strings.Fields(gitIn(t, j.h, j.h.WorkDir, "rev-list", "--reverse", rebaseBase+".."+head))
				for i, commit := range commits {
					gitIn(t, j.h, j.h.WorkDir, "-c", "user.name=Disposable forge", "cherry-pick", commit)
					if mode == "corrupt rebase" && i == 0 {
						if err := os.WriteFile(filepath.Join(j.h.WorkDir, "unexpected.txt"), []byte("corrupt rebased prefix\n"), 0600); err != nil {
							t.Fatal(err)
						}
						gitIn(t, j.h, j.h.WorkDir, "add", "unexpected.txt")
						gitIn(t, j.h, j.h.WorkDir, "commit", "--amend", "--no-edit")
					}
				}
			default:
				gitIn(t, j.h, j.h.WorkDir, "merge", "--no-ff", branch, "-m", "forge merge")
			}
			if mode == "tampered result" {
				gitIn(t, j.h, j.h.WorkDir, "rm", "first.txt")
				gitIn(t, j.h, j.h.WorkDir, "commit", "--amend", "--no-edit")
			}
			merged := gitIn(t, j.h, j.h.WorkDir, "rev-parse", "HEAD")
			if mode == "later target" {
				j.h.CommitChange("main", "after-merge.txt", "later target only\n", "later target movement")
			}
			gitIn(t, j.h, j.h.WorkDir, "push", "origin", "main")
			state := readStatefulGH(t, j.statePath)
			state.PRs[branch].State = "MERGED"
			state.PRs[branch].MergeCommitSHA = merged
			writeStatefulGH(t, j.statePath, state)
			terminal := j.h.WaitForRun(branch, 90*time.Second)
			want := types.RunCompleted
			if mode == "tampered result" || mode == "corrupt rebase" || mode == "missing observation" {
				want = types.RunFailed
			}
			if terminal.Status != want {
				t.Fatalf("terminal status=%s want=%s error=%s\n%s", terminal.Status, want, deref(terminal.Error), j.ciLog(run.ID))
			}
			if want == types.RunFailed && !strings.Contains(deref(terminal.Error), "merged PR tree differs from the reviewed comparison result") {
				t.Fatalf("failed for an unrelated reason: %s", deref(terminal.Error))
			}
			current, err := database.GetRunPRContext(run.ID)
			if err != nil || !reflect.DeepEqual(current, original) || terminal.HeadSHA != head || j.h.UpstreamBranchSHA(branch) != head {
				t.Fatalf("terminal observation moved head or qualification: %+v -> %+v (%v)", original, current, err)
			}
			if livePRBody(t, j.statePath, branch) != body {
				t.Fatal("terminal observation restamped the old PR attestation")
			}
			// Terminal failure may publish its own failure status, never a new
			// qualification status or fabricated successful attestation.
			out, err := j.h.Run("axi", "status", "--run", run.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: head=%s original_target=%s verified_target=%s merge=%s status_writes_before=%d\n%s", name, head, original.TargetSHA, target, merged, statusWrites, out)
			t.Logf("receipt unchanged at generation %d; terminal outcome=%s error=%s", current.Generation, terminal.Status, deref(terminal.Error))
		})
	}
}
