package daemon

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestImmutableShippingRerunUsesPublishedSourceWithoutUpdatingGateBranch(t *testing.T) {
	for _, gateState := range []string{"absent", "stale", "moved"} {
		t.Run(gateState, func(t *testing.T) {
			p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
				steps := make([]pipeline.Step, 0, len(types.AllSteps()))
				for _, name := range types.AllSteps() {
					steps = append(steps, &mockPassStep{name: name})
				}
				return steps
			})
			repo, baseSHA := setupTestGitRepo(t, p, d, "immutable-rerun-"+gateState)
			gitCmd(t, repo.WorkingPath, "checkout", "-B", "main")

			published := filepath.Join(t.TempDir(), "published.git")
			gitCmd(t, "", "init", "--bare", published)
			gitCmd(t, repo.WorkingPath, "remote", "set-url", "gate", published)
			gitCmd(t, p.RepoDir(repo.ID), "remote", "set-url", "origin", published)
			// Finalize the intended fixture gate configuration before admitting runs.
			if err := git.RecordSharedConfigBaseline(context.Background(), p.RepoDir(repo.ID)); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, repo.WorkingPath, "push", "gate", "main:refs/heads/main")
			repo, err := d.ReplaceRepoURLs(repo.ID, published, "")
			if err != nil {
				t.Fatal(err)
			}

			gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
			gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "immutable source")
			submitted := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
			gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/feature")
			gitCmd(t, repo.WorkingPath, "checkout", "main")
			if gateState == "moved" {
				gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "advance target")
				gitCmd(t, repo.WorkingPath, "push", "gate", "main:refs/heads/main")
				gitCmd(t, repo.WorkingPath, "checkout", "feature")
			}
			// Keep the selected commit out of the gate so the rerun must fetch
			// the published object before WorktreeAdd can use it.
			missingSourceObject := exec.Command("git", "--git-dir", p.RepoDir(repo.ID), "cat-file", "-e", submitted+"^{commit}")
			if err := missingSourceObject.Run(); err == nil {
				t.Fatalf("test fixture already has published source object %s in the local gate", submitted)
			}

			shippingPin := &types.ShippingOptions{Mode: types.ShippingImmutableSource, TargetBranch: "main", TargetSHA: baseSHA, Draft: rerunTestBoolPtr(true)}
			selected, err := d.InsertRunWithShipping(repo.ID, "feature", submitted, baseSHA, nil, "", "", "", "main", false, nil, shippingPin)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(selected.ID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			newer, err := d.InsertRun(repo.ID, "feature", baseSHA, baseSHA)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(newer.ID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			if gateState == "stale" {
				gitCmd(t, p.RepoDir(repo.ID), "update-ref", "refs/heads/feature", baseSHA)
			}
			gateBranchesBefore := gitOutput(t, p.RepoDir(repo.ID), "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads")
			publishedBranchesBefore := gitOutput(t, published, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads")

			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var result ipc.RerunResult
			if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{
				RepoID: repo.ID, Branch: "feature", PreviousRunID: selected.ID,
			}, &result); err != nil {
				if gateState != "moved" {
					t.Fatal(err)
				}
				if !strings.Contains(err.Error(), "immutable target moved") {
					t.Fatalf("moved target refusal = %v, want immutable target movement", err)
				}
				runs, runErr := d.GetRunsByRepo(repo.ID)
				if runErr != nil || len(runs) != 2 {
					t.Fatalf("moved target refusal created a run: runs=%d err=%v", len(runs), runErr)
				}
				return
			}
			if gateState == "moved" {
				t.Fatalf("rerun succeeded after target movement: %s", result.RunID)
			}
			got := waitForRunTerminalStateWithTimeout(t, d, result.RunID, time.Minute)
			if got.Status != types.RunCompleted || got.SubmittedHeadSHA == nil || *got.SubmittedHeadSHA != submitted {
				t.Fatalf("rerun = %+v, want completed at immutable source %s", got, submitted)
			}
			if got.Shipping == nil || got.Shipping.Mode != types.ShippingImmutableSource || got.Shipping.TargetBranch != "main" || got.Shipping.TargetSHA != baseSHA || got.Shipping.Draft == nil || !*got.Shipping.Draft {
				t.Fatalf("rerun lost strict shipping mode, target, or draft: %+v", got.Shipping)
			}
			if got.PRBaseBranch == nil || *got.PRBaseBranch != "main" {
				t.Fatalf("rerun PR base = %#v, want pinned main target", got.PRBaseBranch)
			}
			fetchedSourceObject := exec.Command("git", "--git-dir", p.RepoDir(repo.ID), "cat-file", "-e", submitted+"^{commit}")
			if err := fetchedSourceObject.Run(); err != nil {
				t.Fatalf("rerun did not fetch published source object %s into the gate: %v", submitted, err)
			}
			if gateBranchesAfter := gitOutput(t, p.RepoDir(repo.ID), "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads"); gateBranchesAfter != gateBranchesBefore {
				t.Fatalf("rerun changed gate branches:\nbefore:\n%s\nafter:\n%s", gateBranchesBefore, gateBranchesAfter)
			}
			if publishedBranchesAfter := gitOutput(t, published, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads"); publishedBranchesAfter != publishedBranchesBefore {
				t.Fatalf("rerun changed published source branches:\nbefore:\n%s\nafter:\n%s", publishedBranchesBefore, publishedBranchesAfter)
			}
		})
	}
}

func rerunTestBoolPtr(value bool) *bool { return &value }

func TestRerunChecksCallerHeadAgainstSelectedHead(t *testing.T) {
	for _, preserved := range []bool{false, true} {
		name := "gate"
		if preserved {
			name = "preserved"
		}
		for _, relation := range []string{"differs", "matches", "omitted"} {
			t.Run(name+"/"+relation, func(t *testing.T) {
				step := &mockPassStep{name: types.StepReview}
				p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{step} })
				repo, submitted := setupTestGitRepo(t, p, d, "rerun-head-repo")
				prior, err := d.InsertRun(repo.ID, "main", submitted, submitted)
				if err != nil {
					t.Fatal(err)
				}
				intent := "  preserve these exact requirements\n"
				if err := d.UpdateRunIntent(prior.ID, db.RunIntent{Summary: intent, Source: db.RunIntentSourceAgent, Score: 1}); err != nil {
					t.Fatal(err)
				}
				selected := submitted
				if preserved {
					gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "pipeline rewrite")
					selected = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
					gitCmd(t, repo.WorkingPath, "push", "gate", selected+":"+custody.RecoveryRef(prior.ID))
				}
				if err := d.UpdateRunStatusWithVerifiedHead(prior.ID, types.RunFailed, selected); err != nil {
					t.Fatal(err)
				}
				if relation != "matches" {
					gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "--amend", "-m", "corrected local head")
				}
				callerHead := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
				if dirty, err := git.HasUncommittedChanges(context.Background(), repo.WorkingPath); err != nil || dirty {
					t.Fatalf("caller worktree is dirty: %v, err=%v", dirty, err)
				}
				client, err := ipc.Dial(p.Socket())
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				var result ipc.RerunResult
				// Use the wire shape so the regression runs on the pre-fix protocol.
				params := map[string]string{
					"repo_id": repo.ID, "branch": "main", "caller_head_sha": callerHead,
				}
				if relation == "omitted" {
					delete(params, "caller_head_sha")
				}
				err = client.Call(ipc.MethodRerun, params, &result)
				if relation != "differs" {
					if err != nil {
						t.Fatal(err)
					}
					run := waitForRunTerminalState(t, d, result.RunID)
					if run.SubmittedHeadSHA == nil || *run.SubmittedHeadSHA != selected || run.Status != types.RunCompleted {
						t.Fatalf("matching rerun did not complete at selected head %s: %+v", selected, run)
					}
					if run.Intent == nil || *run.Intent != intent || run.IntentSource == nil || *run.IntentSource != db.RunIntentSourceRerun {
						t.Fatalf("rerun lost exact intent or provenance: %+v", run)
					}
					t.Logf("rerun response: run_id=%s; persisted status=%s submitted_head_sha=%s intent=%q intent_source=%s", result.RunID, run.Status, *run.SubmittedHeadSHA, *run.Intent, *run.IntentSource)
				} else {
					if err == nil {
						t.Fatalf("rerun started %s at selected head %s despite clean caller head %s", result.RunID, selected, callerHead)
					}
					for _, want := range []string{selected, callerHead, "no-mistakes axi status", "no-mistakes axi run"} {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("refusal %q missing %q", err, want)
						}
					}
					t.Logf("rerun response: %v", err)
					runs, err := d.GetRunsByRepo(repo.ID)
					if err != nil || len(runs) != 1 || step.execCnt.Load() != 0 {
						t.Fatalf("refused rerun performed work: runs=%d executions=%d err=%v", len(runs), step.execCnt.Load(), err)
					}
					t.Logf("persisted runs=%d; step executions=%d", len(runs), step.execCnt.Load())
				}
				if got := gitOutput(t, p.RepoDir(repo.ID), "rev-parse", "refs/heads/main"); got != submitted {
					t.Fatalf("gate branch moved to %s, want %s", got, submitted)
				}
				if got := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD"); got != callerHead {
					t.Fatalf("caller head moved to %s, want %s", got, callerHead)
				}
				t.Logf("unchanged gate branch=%s; unchanged caller HEAD=%s", submitted, callerHead)
			})
		}
	}
}

func TestRerunRefusalDoesNotSupersedeActiveRun(t *testing.T) {
	started := make(chan struct{})
	step := &mockSlowStep{name: types.StepReview, started: started}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{step} })
	repo, selected := setupTestGitRepo(t, p, d, "active-rerun-head-repo")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var first ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", New: selected, Old: selected,
		Intent: "keep the active run alive",
	}, &first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("original run did not start")
	}
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "--amend", "-m", "corrected local head")
	callerHead := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gateRefs := gitOutput(t, p.RepoDir(repo.ID), "show-ref")
	callerRefs := gitOutput(t, repo.WorkingPath, "show-ref")
	var result ipc.RerunResult
	err = client.Call(ipc.MethodRerun, map[string]string{
		"repo_id": repo.ID, "branch": "main", "caller_head_sha": callerHead,
	}, &result)
	if err == nil || !strings.Contains(err.Error(), "refusing rerun") || !strings.Contains(err.Error(), selected) || !strings.Contains(err.Error(), callerHead) {
		t.Fatalf("expected head mismatch refusal, got result=%+v err=%v", result, err)
	}
	t.Logf("rerun response: %v", err)
	active, err := d.GetActiveRun(repo.ID, "main")
	if err != nil || active == nil || active.ID != first.RunID || active.Status != types.RunRunning {
		t.Fatalf("original run was superseded: active=%+v err=%v", active, err)
	}
	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("refusal created a run: runs=%d err=%v", len(runs), err)
	}
	if got := gitOutput(t, p.RepoDir(repo.ID), "show-ref"); got != gateRefs {
		t.Fatalf("gate refs changed: before=%s after=%s", gateRefs, got)
	}
	if got := gitOutput(t, repo.WorkingPath, "show-ref"); got != callerRefs {
		t.Fatalf("caller refs changed: before=%s after=%s", callerRefs, got)
	}
	t.Logf("original run_id=%s remains %s; persisted runs=%d; caller and gate refs unchanged", active.ID, active.Status, len(runs))
}
