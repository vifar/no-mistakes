package gate

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// rewrittenRun records the terminal run whose published head privateHead was
// superseded by a remote rewrite to liveHead. When recovered, the push binding
// is rebound the way recover_remote_rewritten does it (generation 1 -> 2);
// otherwise the run still binds the anchored head, as after a recovery that
// wrote its anchor and then refused. It returns the run's anchor ref and the
// evidence source for the reconciled branch.
func rewrittenRun(t *testing.T, work, privateHead, liveHead string, recovered bool) (string, SupersessionSource) {
	t.Helper()
	d := openTestDB(t, testPaths(t))
	repo, err := d.InsertRepo(work, "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature/reconcile", privateHead, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunHeadSHA(run.ID, privateHead); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: privateHead, TargetKind: "upstream", TargetFingerprint: "fp", Ref: "refs/heads/feature/reconcile"}); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	run, err = d.GetRun(run.ID)
	if err != nil || run.PushGeneration == nil {
		t.Fatalf("run binding: %v %+v", err, run)
	}
	generation := *run.PushGeneration
	if recovered {
		rebound, err := d.RebindRunPushedHead(run.ID, db.PushRebind{
			Status: run.Status, ExpectedPushed: privateHead, ExpectedGeneration: generation, ExpectedHead: run.HeadSHA, PRState: run.PRState,
			UpstreamURL: repo.UpstreamURL, TargetKind: "upstream", TargetFingerprint: "fp", Ref: "refs/heads/feature/reconcile", Head: liveHead,
		})
		if err != nil || !rebound {
			t.Fatalf("rebind: %v %v", rebound, err)
		}
	}
	return "refs/no-mistakes/recover-rewritten/" + run.ID + "/" + strconv.FormatInt(generation, 10), SupersessionSource{DB: d, RepoID: repo.ID, Branch: "feature/reconcile"}
}

// Issue #97: a completed `axi sync --recover` pins the superseded pipeline
// push head at refs/no-mistakes/recover-rewritten/<run>/<generation> and
// rebinds the run past that generation. Reconciliation credits that anchor,
// archives the mirror head before deleting the ref, and reports the anchor.
func TestReconcileSupersededSubmissionRetiresMirrorPreservedByRecoveredRewrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work, gateDir, privateHead, firstPrivateHead, liveHead := setupAnchoredPrivateBranch(t)
	anchor, source := rewrittenRun(t, work, privateHead, liveHead, true)
	reconcileGit(t, gateDir, "update-ref", anchor, privateHead)

	result, err := ReconcileSupersededSubmission(ctx, gateDir, work, "feature/reconcile", liveHead, source)
	if err != nil {
		t.Fatalf("mirror preserved by a recovered-rewrite anchor was refused: %v", err)
	}
	if !result.Reconciled || result.PreviousHead != privateHead {
		t.Fatalf("reconciliation = %+v, want the mirror head %s retired", result, privateHead)
	}
	if len(result.PreservedBy) != 1 || result.PreservedBy[0] != anchor {
		t.Fatalf("PreservedBy = %v, want exactly %s", result.PreservedBy, anchor)
	}
	if got := reconcileGit(t, gateDir, "rev-parse", result.ArchivedTag); got != privateHead {
		t.Fatalf("archive %s = %s, want the exact mirror head %s", result.ArchivedTag, got, privateHead)
	}
	if _, err := reconcileGitAllowFail(gateDir, "rev-parse", "--verify", "refs/heads/feature/reconcile"); err == nil {
		t.Fatal("the mirror ref survived its reconciliation")
	}
	for _, commit := range []string{firstPrivateHead, privateHead} {
		if _, err := reconcileGitAllowFail(gateDir, "merge-base", "--is-ancestor", commit, anchor); err != nil {
			t.Fatalf("retired commit %s is no longer reachable from %s", commit, anchor)
		}
	}
}

// A recovery that wrote its anchor and then refused (the live head moved
// again, the worktree changed, the binding raced) recovered nothing, so the
// leftover anchor earns no credit and the divergence keeps today's refusal.
// So does an anchor read with no run records at all (the publication path).
func TestReconcileSupersededSubmissionRefusesAnAnchorWhoseRecoveryNeverCommitted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work, gateDir, privateHead, _, liveHead := setupAnchoredPrivateBranch(t)
	anchor, source := rewrittenRun(t, work, privateHead, liveHead, false)
	reconcileGit(t, gateDir, "update-ref", anchor, privateHead)

	for name, src := range map[string]SupersessionSource{"failed recovery": source, "no run records": {}} {
		_, err := PlanSupersededSubmissionReconciliation(ctx, gateDir, work, "feature/reconcile", liveHead, src)
		var refusal *StaleBranchRefusal
		if !errors.As(err, &refusal) || len(refusal.AtRisk) != 2 {
			t.Fatalf("%s: leftover anchor credited preservation: %v", name, err)
		}
	}
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"); got != privateHead {
		t.Fatalf("refusal moved the mirror to %s, want %s", got, privateHead)
	}
}

// An anchor names its own run: a recovered run on another branch, or a
// malformed generation, cannot vouch for this mirror.
func TestReconcileSupersededSubmissionRefusesARewrittenAnchorNamingAnotherBindingOrShape(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work, gateDir, privateHead, _, liveHead := setupAnchoredPrivateBranch(t)
	anchor, source := rewrittenRun(t, work, privateHead, liveHead, true)
	// A non-canonical and a non-numeric generation for the same recovered run.
	runPrefix := anchor[:strings.LastIndex(anchor, "/")+1]
	reconcileGit(t, gateDir, "update-ref", runPrefix+"01", privateHead)
	reconcileGit(t, gateDir, "update-ref", runPrefix+"1x", privateHead)
	other := source
	other.Branch = "feature/other"

	for name, src := range map[string]SupersessionSource{"malformed refs": source} {
		if _, err := PlanSupersededSubmissionReconciliation(ctx, gateDir, work, "feature/reconcile", liveHead, src); !isRefusal(err) {
			t.Fatalf("%s credited preservation: %v", name, err)
		}
	}
	reconcileGit(t, gateDir, "update-ref", anchor, privateHead)
	if _, err := PlanSupersededSubmissionReconciliation(ctx, gateDir, work, "feature/reconcile", liveHead, other); !isRefusal(err) {
		t.Fatalf("a run on another branch vouched for this mirror: %v", err)
	}
}

// A mirror commit the recovered-rewrite anchor does not reach was never
// archived by the recovery step, so it is never dropped: the submission is
// refused, the refusal names exactly that commit and the rewritten-anchor
// satisfier, and the mirror ref is left untouched.
func TestReconcileSupersededSubmissionRefusesMirrorCommitsTheRewrittenAnchorMisses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work, gateDir, privateHead, firstPrivateHead, liveHead := setupAnchoredPrivateBranch(t)
	anchor, source := rewrittenRun(t, work, firstPrivateHead, liveHead, true)
	reconcileGit(t, gateDir, "update-ref", anchor, firstPrivateHead)

	_, err := ReconcileSupersededSubmission(ctx, gateDir, work, "feature/reconcile", liveHead, source)
	var refusal *StaleBranchRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("an unarchived mirror commit was not refused: %v", err)
	}
	if len(refusal.AtRisk) != 1 || !strings.HasPrefix(refusal.AtRisk[0], privateHead+" ") {
		t.Fatalf("at-risk = %v, want only the unarchived commit %s", refusal.AtRisk, privateHead)
	}
	if !strings.Contains(err.Error(), "refs/no-mistakes/recover-rewritten/<run>/<generation>") {
		t.Fatalf("refusal does not name the recovered-rewrite satisfier: %v", err)
	}
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"); got != privateHead {
		t.Fatalf("refusal moved the mirror to %s, want %s", got, privateHead)
	}
}

// The credit is bounded to the two recovery namespaces: a look-alike ref under
// another refs/no-mistakes/ prefix preserves nothing.
func TestReconcileSupersededSubmissionIgnoresRefsOutsideTheRecoveryNamespaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work, gateDir, privateHead, _, liveHead := setupAnchoredPrivateBranch(t)
	anchor, source := rewrittenRun(t, work, privateHead, liveHead, true)
	lookalike := strings.Replace(anchor, "recover-rewritten/", "recover-rewrittenx/", 1)
	reconcileGit(t, gateDir, "update-ref", "refs/no-mistakes/recover-local/01M58ZH63SR5PYZ53RGXSBD8PC", privateHead)
	reconcileGit(t, gateDir, "update-ref", lookalike, privateHead)

	_, err := ReconcileSupersededSubmission(ctx, gateDir, work, "feature/reconcile", liveHead, source)
	var refusal *StaleBranchRefusal
	if !errors.As(err, &refusal) || len(refusal.AtRisk) != 2 {
		t.Fatalf("refs outside the recovery namespaces credited preservation: %v", err)
	}
}

func isRefusal(err error) bool {
	var refusal *StaleBranchRefusal
	return errors.As(err, &refusal)
}
