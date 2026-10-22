package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The base branch is part of what the review is about. A PR against develop
// and a PR against main are different review objects with different scope,
// different comparison, and different merge destination, so a run that
// continues a lineage inherits the lineage's base branch instead of
// resolving the repository default again.

// lineageRepo creates refs/heads/epic/feature in the gate with one commit and
// returns the repository record plus that commit's SHA. The gate ref must
// stand at the SHA the push notification reports, so the fixture pushes once
// and the test drives the later pushes itself.
//
// The gate also carries refs/heads/develop, the base branch the lineage
// targets, because the run setup resolves the recorded base branch against
// the remote before it starts.
func lineageRepo(t *testing.T, p *paths.Paths, d *db.DB, repoID string) (*db.Repo, string) {
	t.Helper()
	repo, _ := setupTestGitRepo(t, p, d, repoID)
	workDir := repo.WorkingPath
	gitCmd(t, workDir, "push", "gate", "HEAD:refs/heads/develop")
	gitCmd(t, workDir, "checkout", "-b", "epic/feature")
	if err := os.WriteFile(filepath.Join(workDir, "epic.txt"), []byte("epic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", "epic.txt")
	gitCmd(t, workDir, "commit", "-m", "epic")
	firstSHA := gitOutput(t, workDir, "rev-parse", "HEAD")
	gitCmd(t, workDir, "push", "gate", "HEAD:refs/heads/epic/feature")
	return repo, firstSHA
}

// advanceLineage adds a commit to the lineage's branch and pushes it to the
// gate, returning the new head SHA.
func advanceLineage(t *testing.T, repo *db.Repo, name string) string {
	t.Helper()
	workDir := repo.WorkingPath
	if err := os.WriteFile(filepath.Join(workDir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, workDir, "add", name)
	gitCmd(t, workDir, "commit", "-m", name)
	headSHA := gitOutput(t, workDir, "rev-parse", "HEAD")
	gitCmd(t, workDir, "push", "gate", "HEAD:refs/heads/epic/feature")
	return headSHA
}

// A replacement run continues the lineage: the branch's established base
// branch is carried forward onto the successor's own record, with no
// --base-branch passed again.
func TestReplacementRunInheritsTheLineageBaseBranch(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	repo, firstSHA := lineageRepo(t, p, d, "pr-base-lineage-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// The first run establishes the lineage's base branch.
	var first ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:         p.RepoDir("pr-base-lineage-repo"),
		Ref:          "refs/heads/epic/feature",
		Old:          "0000000000000000000000000000000000000000",
		New:          firstSHA,
		PRBaseBranch: "develop",
	}, &first)
	if err != nil {
		t.Fatal(err)
	}
	firstRun := waitForRunTerminalState(t, d, first.RunID)
	if firstRun.PRBaseBranch == nil || *firstRun.PRBaseBranch != "develop" {
		t.Fatalf("first run PRBaseBranch = %#v, want develop", firstRun.PRBaseBranch)
	}
	if !firstRun.PRBaseBranchRequested {
		t.Fatal("the fresh --base-branch request was not persisted")
	}

	// A replacement run for the same branch, with no --base-branch passed
	// again, must carry the lineage's base branch forward.
	secondSHA := advanceLineage(t, repo, "epic2.txt")
	var second ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("pr-base-lineage-repo"),
		Ref:  "refs/heads/epic/feature",
		Old:  firstSHA,
		New:  secondSHA,
	}, &second)
	if err != nil {
		t.Fatal(err)
	}
	secondRun := waitForRunTerminalState(t, d, second.RunID)
	if secondRun.ID == firstRun.ID {
		t.Fatal("the replacement run reused the first run")
	}
	if secondRun.PRBaseBranch == nil {
		t.Fatal("the replacement run did not inherit the lineage's base branch")
	}
	if *secondRun.PRBaseBranch != "develop" {
		t.Fatalf("replacement run PRBaseBranch = %q, want the lineage's develop", *secondRun.PRBaseBranch)
	}
	if secondRun.PRBaseBranchRequested {
		t.Fatal("the inherited base branch became a fresh retarget request")
	}
}

// A caller that passes a new --base-branch still wins for that run, and the
// fresh request keeps its one-time retarget authority.
func TestFreshBaseBranchRequestBeatsTheInheritedLineage(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	repo, firstSHA := lineageRepo(t, p, d, "pr-base-fresh-wins-repo")
	// The caller's fresh request names another base branch, so it must exist
	// on the remote too.
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/release/1.x")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var first ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:         p.RepoDir("pr-base-fresh-wins-repo"),
		Ref:          "refs/heads/epic/feature",
		Old:          "0000000000000000000000000000000000000000",
		New:          firstSHA,
		PRBaseBranch: "develop",
	}, &first)
	if err != nil {
		t.Fatal(err)
	}
	waitForRunTerminalState(t, d, first.RunID)

	secondSHA := advanceLineage(t, repo, "epic2.txt")
	var second ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:         p.RepoDir("pr-base-fresh-wins-repo"),
		Ref:          "refs/heads/epic/feature",
		Old:          firstSHA,
		New:          secondSHA,
		PRBaseBranch: "release/1.x",
	}, &second)
	if err != nil {
		t.Fatal(err)
	}
	secondRun := waitForRunTerminalState(t, d, second.RunID)
	if secondRun.PRBaseBranch == nil || *secondRun.PRBaseBranch != "release/1.x" {
		t.Fatalf("successor PRBaseBranch = %#v, want the caller's fresh request release/1.x", secondRun.PRBaseBranch)
	}
	if !secondRun.PRBaseBranchRequested {
		t.Fatal("the fresh --base-branch request lost its retarget authority")
	}
}

// With no lineage record and no per-run override, the run records no base
// branch at all, so the repository's own default branch stays the target.
// Inheritance must never invent a base branch.
func TestNoLineageNoOverrideRecordsNoBaseBranch(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	_, firstSHA := lineageRepo(t, p, d, "pr-base-no-lineage-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var only ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("pr-base-no-lineage-repo"),
		Ref:  "refs/heads/epic/feature",
		Old:  "0000000000000000000000000000000000000000",
		New:  firstSHA,
	}, &only)
	if err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, only.RunID)
	if run.PRBaseBranch != nil {
		t.Fatalf("a run without a lineage or an override recorded PRBaseBranch = %#v, want none", run.PRBaseBranch)
	}
	if run.PRBaseBranchRequested {
		t.Fatal("a run without a base branch request recorded a retarget request")
	}
}

// A rerun of a run that recorded no base branch still inherits the lineage's
// base branch: the rerun continues the same lineage, so it targets the same
// base.
func TestRerunInheritsTheLineageBaseBranchWhenTheSelectedRunHasNone(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	repo, firstSHA := lineageRepo(t, p, d, "pr-base-rerun-lineage-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// The first run establishes the lineage's base branch.
	var first ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:         p.RepoDir("pr-base-rerun-lineage-repo"),
		Ref:          "refs/heads/epic/feature",
		Old:          "0000000000000000000000000000000000000000",
		New:          firstSHA,
		PRBaseBranch: "develop",
	}, &first)
	if err != nil {
		t.Fatal(err)
	}
	firstRun := waitForRunTerminalState(t, d, first.RunID)

	// A second run for the same branch records no base branch of its own, so
	// the selected run for the rerun below has none either.
	secondSHA := advanceLineage(t, repo, "epic2.txt")
	var second ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("pr-base-rerun-lineage-repo"),
		Ref:  "refs/heads/epic/feature",
		Old:  firstSHA,
		New:  secondSHA,
	}, &second)
	if err != nil {
		t.Fatal(err)
	}
	secondRun := waitForRunTerminalState(t, d, second.RunID)
	if secondRun.PRBaseBranch == nil || *secondRun.PRBaseBranch != "develop" {
		t.Fatalf("second run PRBaseBranch = %#v, want the inherited develop", secondRun.PRBaseBranch)
	}

	// A rerun of the first run inherits the lineage's base branch too.
	var rerun ipc.RerunResult
	err = client.Call(ipc.MethodRerun, &ipc.RerunParams{
		RepoID:        "pr-base-rerun-lineage-repo",
		Branch:        "epic/feature",
		PreviousRunID: firstRun.ID,
	}, &rerun)
	if err != nil {
		t.Fatal(err)
	}
	rerunRun := waitForRunTerminalState(t, d, rerun.RunID)
	if rerunRun.PRBaseBranch == nil || *rerunRun.PRBaseBranch != "develop" {
		t.Fatalf("rerun PRBaseBranch = %#v, want the lineage's develop", rerunRun.PRBaseBranch)
	}
	if rerunRun.PRBaseBranchRequested {
		t.Fatal("the inherited base branch became a fresh retarget request")
	}
}

// The existing HandleRerun inheritance must not regress, and a rerun whose
// caller passes a DIFFERENT base branch keeps the CALLER's request: the
// caller's explicit request wins for that run, and the selected run's own
// base branch and the lineage's base branch do not override it.
//
// This is the case the pre-existing rerun test cannot distinguish, because it
// reruns with the SAME value it started with. Here the rerun names another
// branch, so a run that silently preferred the selected run's value or the
// lineage's value would be caught.
func TestRerunWithADifferentBaseBranchKeepsTheCallersRequest(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	repo, firstSHA := lineageRepo(t, p, d, "pr-base-rerun-different-repo")
	// The caller's different base branch has to exist on the remote too,
	// because the run setup resolves the recorded base branch against the
	// remote before the run starts.
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/release/1.x")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// The first run establishes the lineage's base branch.
	var first ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:         p.RepoDir("pr-base-rerun-different-repo"),
		Ref:          "refs/heads/epic/feature",
		Old:          "0000000000000000000000000000000000000000",
		New:          firstSHA,
		PRBaseBranch: "develop",
	}, &first)
	if err != nil {
		t.Fatal(err)
	}
	firstRun := waitForRunTerminalState(t, d, first.RunID)
	if firstRun.PRBaseBranch == nil || *firstRun.PRBaseBranch != "develop" {
		t.Fatalf("first run PRBaseBranch = %#v, want develop", firstRun.PRBaseBranch)
	}

	// A rerun that names a DIFFERENT base branch keeps the caller's request.
	var rerun ipc.RerunResult
	err = client.Call(ipc.MethodRerun, &ipc.RerunParams{
		RepoID:        "pr-base-rerun-different-repo",
		Branch:        "epic/feature",
		PreviousRunID: firstRun.ID,
		PRBaseBranch:  "release/1.x",
	}, &rerun)
	if err != nil {
		t.Fatal(err)
	}
	rerunRun := waitForRunTerminalState(t, d, rerun.RunID)
	if rerunRun.PRBaseBranch == nil || *rerunRun.PRBaseBranch != "release/1.x" {
		t.Fatalf("rerun PRBaseBranch = %#v, want the caller's release/1.x", rerunRun.PRBaseBranch)
	}
	if !rerunRun.PRBaseBranchRequested {
		t.Fatal("the caller's fresh --base-branch request lost its retarget authority")
	}
}

// A base branch that cannot be carried is not silently replaced. A recorded
// lineage base branch that is not a usable branch name is refused visibly,
// naming the branch, instead of being dropped for the repository default.
func TestUninheritableLineageBaseBranchIsRefusedVisibly(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	_, firstSHA := lineageRepo(t, p, d, "pr-base-uninheritable-repo")

	// A lineage record whose base branch cannot be used as a branch name.
	// The run is inserted directly because no launch path can record one.
	corrupt, err := d.InsertRunWithIntent("pr-base-uninheritable-repo", "epic/feature", "0000000000000000000000000000000000000001", firstSHA, nil, "bad..branch")
	if err != nil {
		t.Fatal(err)
	}
	if corrupt.PRBaseBranch == nil {
		t.Fatal("the fixture did not record the unusable base branch")
	}
	if err := d.UpdateRunStatus(corrupt.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("pr-base-uninheritable-repo"),
		Ref:  "refs/heads/epic/feature",
		Old:  "0000000000000000000000000000000000000000",
		New:  firstSHA,
	}, &result)
	if err == nil {
		t.Fatal("a successor that could not carry the lineage base branch was admitted")
	}
	if !strings.Contains(err.Error(), "bad..branch") {
		t.Fatalf("error = %v, want it to name the unusable base branch", err)
	}
	if strings.Contains(err.Error(), "main") {
		t.Fatalf("the refusal silently retargeted to the default: %v", err)
	}

	// The refused successor did not record a different base branch either.
	runs, listErr := d.GetRunsByRepo("pr-base-uninheritable-repo")
	if listErr != nil {
		t.Fatal(listErr)
	}
	for _, run := range runs {
		if run.ID == corrupt.ID {
			continue
		}
		if run.PRBaseBranch != nil && *run.PRBaseBranch != "bad..branch" {
			t.Fatalf("the refused successor recorded base branch %q", *run.PRBaseBranch)
		}
	}
}

// A lineage base branch that no longer exists on the remote is still carried
// onto the successor. The successor targets the base the lineage was on, so
// the run is refused at the comparison boundary instead of quietly re-scoping
// the review to the repository default.
func TestALineageBaseBranchThatNoLongerExistsIsStillCarried(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	_, firstSHA := lineageRepo(t, p, d, "pr-base-removed-base-repo")

	// The lineage's base branch was recorded and then deleted upstream.
	recorded, err := d.InsertRunWithIntent("pr-base-removed-base-repo", "epic/feature", "0000000000000000000000000000000000000001", firstSHA, nil, "gone/base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(recorded.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("pr-base-removed-base-repo"),
		Ref:  "refs/heads/epic/feature",
		Old:  "0000000000000000000000000000000000000000",
		New:  firstSHA,
	}, &result)
	// The successor targets the base the lineage was on, so the missing base
	// branch is refused. The refusal is reported as the launch error.
	if err != nil && !strings.Contains(err.Error(), "gone/base") {
		t.Fatal(err)
	}
	runs, listErr := d.GetRunsByRepo("pr-base-removed-base-repo")
	if listErr != nil {
		t.Fatal(listErr)
	}
	var successor *db.Run
	for _, run := range runs {
		if run.ID != recorded.ID && run.Branch == "epic/feature" {
			successor = run
		}
	}
	if successor == nil {
		t.Fatal("the successor run was not recorded")
	}
	if successor.PRBaseBranch == nil || *successor.PRBaseBranch != "gone/base" {
		t.Fatalf("successor PRBaseBranch = %#v, want the lineage's gone/base", successor.PRBaseBranch)
	}
	if successor.PRBaseBranchRequested {
		t.Fatal("the inherited base branch became a fresh retarget request")
	}
	// The inherited base branch is only the PROSPECTIVE target. It must not
	// refuse the successor at setup: when an open PR exists, its live target
	// takes precedence and the inherited value is never fetched, so a stale
	// inherited branch must not stop a valid continuation. Where the inherited
	// value IS the target - no open PR - the comparison boundary (the Rebase
	// step's fetch of the base) fails closed there instead.
	if successor.Status == types.RunFailed && successor.Error != nil && strings.Contains(*successor.Error, "gone/base") {
		t.Fatalf("the successor was refused at setup for the missing inherited base: %s", *successor.Error)
	}
}

// A base branch the operator REQUESTED for this run must still exist on the
// remote before the run starts, so an explicit request for a branch that is
// not there is refused at setup instead of failing later. The exemption for
// an inherited base branch must not weaken this.
func TestRequestedBaseBranchThatNoLongerExistsIsRefusedAtSetup(t *testing.T) {
	step := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{step}
	})

	_, firstSHA := lineageRepo(t, p, d, "pr-base-requested-missing-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// The caller asks for a base branch that does not exist on the remote.
	var result ipc.PushReceivedResult
	err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate:         p.RepoDir("pr-base-requested-missing-repo"),
		Ref:          "refs/heads/epic/feature",
		Old:          "0000000000000000000000000000000000000000",
		New:          firstSHA,
		PRBaseBranch: "gone/base",
	}, &result)
	if err != nil && !strings.Contains(err.Error(), "gone/base") {
		t.Fatal(err)
	}
	runs, listErr := d.GetRunsByRepo("pr-base-requested-missing-repo")
	if listErr != nil {
		t.Fatal(listErr)
	}
	var requested *db.Run
	for _, run := range runs {
		if run.Branch == "epic/feature" {
			requested = run
		}
	}
	if requested == nil {
		t.Fatal("the requested run was not recorded")
	}
	if requested.PRBaseBranch == nil || *requested.PRBaseBranch != "gone/base" {
		t.Fatalf("requested PRBaseBranch = %#v, want gone/base", requested.PRBaseBranch)
	}
	if !requested.PRBaseBranchRequested {
		t.Fatal("the fresh --base-branch request lost its recorded authority")
	}
	waitForRunTerminalState(t, d, requested.ID)
	runs, listErr = d.GetRunsByRepo("pr-base-requested-missing-repo")
	if listErr != nil {
		t.Fatal(listErr)
	}
	for _, run := range runs {
		if run.ID != requested.ID {
			continue
		}
		if run.Status != types.RunFailed {
			t.Fatalf("run status = %q, want failed: a requested base branch that does not exist must be refused", run.Status)
		}
		if run.Error == nil || !strings.Contains(*run.Error, "gone/base") {
			t.Fatalf("run error = %#v, want it to name the missing requested base branch", run.Error)
		}
	}
}

func TestReplacementLineageIgnoresStrictAndEmptyRecords(t *testing.T) {
	for _, source := range []string{"strict", "legacy"} {
		t.Run(source, func(t *testing.T) {
			p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
				return []pipeline.Step{&mockPassStep{name: types.StepReview}}
			})
			repo, head := lineageRepo(t, p, d, "lineage-source-"+source)
			ordinary, err := d.InsertRunWithIntent(repo.ID, "epic/feature", head, head, nil, "develop")
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(ordinary.ID, types.RunCompleted); err != nil {
				t.Fatal(err)
			}
			var shipping *types.ShippingOptions
			base := ""
			if source == "strict" {
				shipping = &types.ShippingOptions{Mode: types.ShippingFull, TargetBranch: "main"}
				base = "main"
			}
			newer, err := d.InsertRunWithShipping(repo.ID, "epic/feature", head, head, nil, "", "", "", base, false, nil, shipping)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(newer.ID, types.RunCompleted); err != nil {
				t.Fatal(err)
			}
			service := &branchsync.Service{DB: d, Repo: repo, WorkDir: repo.WorkingPath}
			state := service.InspectCached(context.Background())
			if state.Pipeline.RunID != newer.ID {
				t.Fatalf("selected run = %q, want %q", state.Pipeline.RunID, newer.ID)
			}
			if state.Pipeline.PRBaseBranch != "develop" {
				t.Errorf("handoff base = %q, want develop", state.Pipeline.PRBaseBranch)
			}
			nextHead := advanceLineage(t, repo, "successor.txt")
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var result ipc.PushReceivedResult
			if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate: p.RepoDir(repo.ID), Ref: "refs/heads/epic/feature", Old: head, New: nextHead,
			}, &result); err != nil {
				t.Fatal(err)
			}
			successor := waitForRunTerminalState(t, d, result.RunID)
			if successor.PRBaseBranch == nil || *successor.PRBaseBranch != "develop" {
				t.Fatalf("successor base = %#v, want develop", successor.PRBaseBranch)
			}
			if successor.PRBaseBranchRequested {
				t.Fatal("inherited base gained retarget authority")
			}
		})
	}
}

// Issue #153: the selected legacy run has no base of its own. The public
// rerun handler must use the older ordinary lineage record, then let a fresh
// request move the lineage without granting later successors retarget authority.
func TestLegacyRerunAndReplacementKeepTheLatestRequestedBase(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	repo, head := lineageRepo(t, p, d, "legacy-rerun-latest-base")
	for _, base := range []string{"develop", ""} {
		run, err := d.InsertRunWithIntent(repo.ID, "epic/feature", head, head, nil, base)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy := runs[0]
	if legacy.PRBaseBranch != nil {
		t.Fatal("selected legacy record must have no base")
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var rerun ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{
		RepoID: repo.ID, Branch: "epic/feature", PreviousRunID: legacy.ID,
	}, &rerun); err != nil {
		t.Fatal(err)
	}
	continued := waitForRunTerminalState(t, d, rerun.RunID)
	if continued.PRBaseBranch == nil || *continued.PRBaseBranch != "develop" || continued.PRBaseBranchRequested {
		t.Fatalf("legacy rerun: base=%v requested=%v", continued.PRBaseBranch, continued.PRBaseBranchRequested)
	}
	t.Logf("persisted legacy rerun: selected_base=null successor_base=%q retarget_requested=%v", *continued.PRBaseBranch, continued.PRBaseBranchRequested)
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/release/1.x")
	for i, requested := range []string{"release/1.x", ""} {
		next := advanceLineage(t, repo, []string{"fresh-base.txt", "inherited-base.txt"}[i])
		var result ipc.PushReceivedResult
		if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
			Gate: p.RepoDir(repo.ID), Ref: "refs/heads/epic/feature", Old: head, New: next, PRBaseBranch: requested,
		}, &result); err != nil {
			t.Fatal(err)
		}
		run := waitForRunTerminalState(t, d, result.RunID)
		if run.PRBaseBranch == nil || *run.PRBaseBranch != "release/1.x" || run.PRBaseBranchRequested != (requested != "") {
			t.Fatalf("successor after request %q: base=%v requested=%v", requested, run.PRBaseBranch, run.PRBaseBranchRequested)
		}
		t.Logf("persisted successor: caller_base=%q recorded_base=%q retarget_requested=%v", requested, *run.PRBaseBranch, run.PRBaseBranchRequested)
		head = next
	}
}
