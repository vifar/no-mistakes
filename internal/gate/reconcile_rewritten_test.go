package gate

import (
	"context"
	"strings"
	"testing"
)

// Issue #97: `axi sync --recover` after a remote rewrite pins the superseded
// pipeline push head at refs/no-mistakes/recover-rewritten/<run>/<generation>,
// and that head is what the private mirror still holds. Reconciliation must
// credit that anchor, archive the mirror head before deleting the ref, and
// report the anchor so the retired commits stay findable.
func TestReconcileSupersededSubmissionRetiresMirrorPreservedByRewrittenAnchor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work, gateDir, privateHead, firstPrivateHead, liveHead := setupAnchoredPrivateBranch(t)

	anchor := "refs/no-mistakes/recover-rewritten/01M58ZH63SR5PYZ53RGXSBD8PC/1"
	reconcileGit(t, gateDir, "update-ref", anchor, privateHead)

	result, err := ReconcileSupersededSubmission(ctx, gateDir, work, "feature/reconcile", liveHead, SupersessionSource{})
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

// A mirror commit the recovered-rewrite anchor does not reach was never
// archived by the recovery step, so it is never dropped: the submission is
// refused, the refusal names exactly that commit and the rewritten-anchor
// satisfier, and the mirror ref is left untouched.
func TestReconcileSupersededSubmissionRefusesMirrorCommitsTheRewrittenAnchorMisses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work, gateDir, privateHead, firstPrivateHead, liveHead := setupAnchoredPrivateBranch(t)

	reconcileGit(t, gateDir, "update-ref", "refs/no-mistakes/recover-rewritten/01M58ZH63SR5PYZ53RGXSBD8PC/1", firstPrivateHead)

	_, err := ReconcileSupersededSubmission(ctx, gateDir, work, "feature/reconcile", liveHead, SupersessionSource{})
	var refusal *StaleBranchRefusal
	if err == nil || !asStaleBranchRefusal(err, &refusal) {
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
// another refs/no-mistakes/ prefix (here the worktree-only adoption anchor's
// namespace and a prefix sharing "recover") preserves nothing, so an
// unrecovered divergence keeps today's refusal.
func TestReconcileSupersededSubmissionIgnoresRefsOutsideTheRecoveryNamespaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	work, gateDir, privateHead, _, liveHead := setupAnchoredPrivateBranch(t)

	reconcileGit(t, gateDir, "update-ref", "refs/no-mistakes/recover-local/01M58ZH63SR5PYZ53RGXSBD8PC", privateHead)
	reconcileGit(t, gateDir, "update-ref", "refs/no-mistakes/recover-rewrittenx/01M58ZH63SR5PYZ53RGXSBD8PC/1", privateHead)

	_, err := ReconcileSupersededSubmission(ctx, gateDir, work, "feature/reconcile", liveHead, SupersessionSource{})
	var refusal *StaleBranchRefusal
	if err == nil || !asStaleBranchRefusal(err, &refusal) || len(refusal.AtRisk) != 2 {
		t.Fatalf("refs outside the recovery namespaces credited preservation: %v", err)
	}
	if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature/reconcile"); got != privateHead {
		t.Fatalf("refusal moved the mirror to %s, want %s", got, privateHead)
	}
}

func asStaleBranchRefusal(err error, target **StaleBranchRefusal) bool {
	refusal, ok := err.(*StaleBranchRefusal)
	if ok {
		*target = refusal
	}
	return ok
}
