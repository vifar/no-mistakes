package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if successor.Status == types.RunCompleted {
		t.Fatal("a successor whose lineage base branch no longer exists completed")
	}
}
