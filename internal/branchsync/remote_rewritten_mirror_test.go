package branchsync

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/gate"
)

// newHandRebasedFixture models the issue #97 victim: the operator fetched the
// published pipeline head, hand-rebased it into a history that no longer
// contains it, and force-pushed that rebase to the push target. The private
// mirror in the gate still holds the published pipeline head, and the
// worktree holds that object too, so recovery anchors it in the worktree
// first. It returns the hand-rebased head, which is also the worktree HEAD.
func newHandRebasedFixture(t *testing.T) (*syncFixture, string) {
	t.Helper()
	f := newSyncFixture(t)
	gateDir := filepath.Join(filepath.Dir(f.local), "gate.git")
	mustRun(t, filepath.Dir(f.local), "clone", "--bare", f.remote, gateDir)
	f.service.GateDir = gateDir
	mustRun(t, f.local, "fetch", f.remote, "refs/heads/feature/sync:refs/remotes/origin/feature/sync")
	mustRun(t, f.local, "reset", "--hard", f.base)
	mustWrite(t, filepath.Join(f.local, "file.txt"), "feature, rebased by hand\n")
	mustWrite(t, filepath.Join(f.local, "fix.txt"), "pipeline fix, rebased by hand\n")
	mustRun(t, f.local, "add", "file.txt", "fix.txt")
	mustRun(t, f.local, "commit", "-m", "hand-rebased feature")
	rebased := mustRun(t, f.local, "rev-parse", "HEAD")
	mustRun(t, f.local, "push", "--force", f.remote, "HEAD:refs/heads/feature/sync")
	if got := mustRun(t, gateDir, "rev-parse", "refs/heads/feature/sync"); got != f.pushed {
		t.Fatalf("fixture mirror = %s, want the published pipeline head %s", got, f.pushed)
	}
	return f, rebased
}

// Before recovery the private mirror refuses the hand-rebased head exactly as
// it always has; after `sync --recover` anchors the superseded head in the
// gate too, the same submission retires the mirror and names the anchor.
func TestRecoveredRewriteLetsTheNextSubmissionRetireThePrivateMirror(t *testing.T) {
	t.Parallel()

	f, rebased := newHandRebasedFixture(t)
	gateDir := f.service.GateDir
	if _, err := gate.PlanSupersededSubmissionReconciliation(f.ctx, gateDir, f.local, "feature/sync", rebased, gate.SupersessionSource{}); !isStaleBranchRefusal(err) {
		t.Fatalf("unrecovered rewrite must keep today's mirror refusal, got %v", err)
	}

	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered || recovered.Recovery == nil || recovered.Recovery.Source != "remote_rewritten" {
		t.Fatalf("recover = %#v", recovered)
	}
	anchor := recovered.Recovery.ArchiveRef
	if recovered.Recovery.Proof != "worktree" {
		t.Fatalf("proof = %q, want the worktree anchor taken first", recovered.Recovery.Proof)
	}
	for _, dir := range []string{f.local, gateDir} {
		if got := mustRun(t, dir, "rev-parse", anchor+"^{commit}"); got != f.pushed {
			t.Fatalf("anchor %s in %s = %s, want %s", anchor, dir, got, f.pushed)
		}
	}
	if got := mustRun(t, gateDir, "rev-parse", "refs/heads/feature/sync"); got != f.pushed {
		t.Fatalf("recovery itself moved the mirror to %s", got)
	}
	for _, want := range []string{
		"the push binding was rebound from " + f.pushed + " to " + rebased,
		"anchored at " + anchor,
		"the private mirror refs/heads/feature/sync at " + f.pushed + " is preserved by " + anchor,
	} {
		if !strings.Contains(recovered.Error, want) {
			t.Errorf("recovery note missing %q:\n%s", want, recovered.Error)
		}
	}

	result, err := gate.ReconcileSupersededSubmission(f.ctx, gateDir, f.local, "feature/sync", rebased, gate.SupersessionSource{})
	if err != nil {
		t.Fatalf("submission after a recovered rewrite was refused: %v", err)
	}
	if !result.Reconciled || result.PreviousHead != f.pushed || len(result.PreservedBy) != 1 || result.PreservedBy[0] != anchor {
		t.Fatalf("reconciliation = %+v, want the mirror retired on %s", result, anchor)
	}
	if got := mustRun(t, gateDir, "rev-parse", result.ArchivedTag); got != f.pushed {
		t.Fatalf("archive %s = %s, want %s", result.ArchivedTag, got, f.pushed)
	}
}

// A mirror that moved past the superseded head holds a commit the recovery
// never archived. The note says so, and the next submission still refuses
// that commit instead of dropping it.
func TestRecoveredRewriteNeverRetiresMirrorCommitsTheAnchorMisses(t *testing.T) {
	t.Parallel()

	f, rebased := newHandRebasedFixture(t)
	gateDir := f.service.GateDir
	extra := filepath.Join(filepath.Dir(f.local), "extra")
	mustRun(t, filepath.Dir(f.local), "-c", "core.autocrlf=false", "clone", gateDir, extra)
	configureIdentity(t, extra)
	mustRun(t, extra, "checkout", "feature/sync")
	mustWrite(t, filepath.Join(extra, "late.txt"), "late private work\n")
	mustRun(t, extra, "add", "late.txt")
	mustRun(t, extra, "commit", "-m", "late private work")
	late := mustRun(t, extra, "rev-parse", "HEAD")
	mustRun(t, extra, "push", gateDir, "HEAD:refs/heads/feature/sync")

	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered {
		t.Fatalf("recover = %#v", recovered)
	}
	if want := "holds 1 commit(s) " + recovered.Recovery.ArchiveRef + " does not preserve"; !strings.Contains(recovered.Error, want) {
		t.Fatalf("recovery note missing %q:\n%s", want, recovered.Error)
	}

	_, err := gate.ReconcileSupersededSubmission(f.ctx, gateDir, f.local, "feature/sync", rebased, gate.SupersessionSource{})
	var refusal *gate.StaleBranchRefusal
	if !errors.As(err, &refusal) || len(refusal.AtRisk) != 1 || !strings.HasPrefix(refusal.AtRisk[0], late+" ") {
		t.Fatalf("unarchived mirror commit %s was not refused alone: %v", late, err)
	}
	if got := mustRun(t, gateDir, "rev-parse", "refs/heads/feature/sync"); got != late {
		t.Fatalf("mirror moved to %s, want %s", got, late)
	}
}

// When only the worktree holds the superseded head, recovery stages it into
// the gate so the gate anchor exists there too.
func TestRecoverRewrittenRemoteStagesTheSupersededHeadIntoTheGate(t *testing.T) {
	t.Parallel()

	f, _ := newHandRebasedFixture(t)
	emptyGate := filepath.Join(t.TempDir(), "empty-gate.git")
	mustRun(t, filepath.Dir(emptyGate), "init", "--bare", emptyGate)
	f.service.GateDir = emptyGate

	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered || recovered.Recovery == nil {
		t.Fatalf("recover = %#v", recovered)
	}
	if got := mustRun(t, emptyGate, "rev-parse", recovered.Recovery.ArchiveRef+"^{commit}"); got != f.pushed {
		t.Fatalf("gate anchor = %s, want %s", got, f.pushed)
	}
}

// A gate that exists but cannot take the anchor would leave the recovery
// half-done (issue #97), so it refuses before anything is rebound.
func TestRecoverRewrittenRemoteRefusesWhenTheGateAnchorConflicts(t *testing.T) {
	t.Parallel()

	f, _ := newHandRebasedFixture(t)
	mustRun(t, f.service.GateDir, "update-ref", rewrittenAnchorRef(f.run.ID, value(f.run.PushGeneration)), f.base)

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Safety != "blocked_recover_preserve_failed" {
		t.Fatalf("recover with a conflicting gate anchor = %#v", state)
	}
	run, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ptr(run.LastPushedSHA) != f.pushed || value(run.PushGeneration) != value(f.run.PushGeneration) {
		t.Fatalf("binding changed: last_pushed %s generation %d", ptr(run.LastPushedSHA), value(run.PushGeneration))
	}
}

func isStaleBranchRefusal(err error) bool {
	var refusal *gate.StaleBranchRefusal
	return errors.As(err, &refusal)
}
