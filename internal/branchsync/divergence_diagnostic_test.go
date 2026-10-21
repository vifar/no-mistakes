package branchsync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// rewrittenCopyFixture is the shape issue #151 reports. A target advance
// rewrote the operator's commit onto newer main, so the local head and the
// preserved pipeline head hold the SAME change under different commit
// identities, and the two histories conflict where they overlap. The
// containment proof cannot hold - merging them conflicts - so custody
// recovery must refuse, and the refusal must say what it compared and what it
// found.
type rewrittenCopyFixture struct {
	*recoverFixture
	localHead string
	rewritten string
}

// newRewrittenCopyRecoverFixture builds that shape from scratch: the
// operator's local head, and a preserved head whose history carries the same
// change as a different commit (the rewritten copy) followed by a pipeline
// commit that touches the same file, so the merge of the two histories
// conflicts.
func newRewrittenCopyRecoverFixture(t *testing.T) *rewrittenCopyFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustWrite(t, filepath.Join(local, "file.txt"), "base\n")
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")
	mustRun(t, local, "checkout", "-b", "feature/recover")
	mustWrite(t, filepath.Join(local, "file.txt"), "feature\n")
	mustRun(t, local, "commit", "-am", "operator work")
	localHead := mustRun(t, local, "rev-parse", "HEAD")

	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, local, "push", gate, "refs/heads/feature/recover:refs/heads/feature/recover")

	// The pipeline rewrote the branch onto newer main: the operator's change
	// is re-applied as its own commit (the same patch under a new identity,
	// because its parent is now a newer-main commit) and the pipeline then
	// commits on top of it, touching the same file so the two histories
	// conflict.
	pipeline := filepath.Join(root, "pipeline")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "-B", "rewritten", base)
	mustWrite(t, filepath.Join(pipeline, "other.txt"), "newer main\n")
	mustRun(t, pipeline, "add", "other.txt")
	mustRun(t, pipeline, "commit", "-m", "upstream(main): newer main")
	mustWrite(t, filepath.Join(pipeline, "file.txt"), "feature\n")
	mustRun(t, pipeline, "commit", "-am", "operator work")
	rewritten := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustWrite(t, filepath.Join(pipeline, "file.txt"), "feature + pipeline\n")
	mustRun(t, pipeline, "commit", "-am", "no-mistakes(lint): fix")
	preserved := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", localHead, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCancelled, preserved); err != nil {
		t.Fatal(err)
	}
	run, _ = database.GetRun(run.ID)
	return &rewrittenCopyFixture{
		recoverFixture: &recoverFixture{
			t: t, ctx: ctx, db: database, repo: repo, run: run,
			service: &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate},
			local:   local, gate: gate, remote: remote,
			base: base, submitted: localHead, preserved: preserved,
		},
		localHead: localHead,
		rewritten: rewritten,
	}
}

// TestRecoverDivergedRefusalNamesBothHeadsAndTheTestsThatRan pins the first
// half of #151: the refusal names the two commits it compared and says which
// tests it ran, so "diverged" stops reading as "your work was lost".
func TestRecoverDivergedRefusalNamesBothHeadsAndTheTestsThatRan(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)
	mustWrite(t, filepath.Join(f.local, "rescope.txt"), "rescope\n")
	mustRun(t, f.local, "add", "rescope.txt")
	mustRun(t, f.local, "commit", "-m", "diverging rescope")
	divergedHead := mustRun(t, f.local, "rev-parse", "HEAD")

	refused := f.service.Recover(f.ctx, false)
	if refused.Recovered || refused.Changed || refused.Safety != "blocked_recover_diverged" {
		t.Fatalf("recover diverged = %#v", refused)
	}
	for _, want := range []string{divergedHead, f.preserved, "ancestry", "three-way merge"} {
		if !strings.Contains(refused.Error, want) {
			t.Errorf("divergence refusal does not name %q: %q", want, refused.Error)
		}
	}
}

// TestRecoverDivergedRefusalReportsAPatchMatchAsADiagnosticOnly pins the
// second half of #151: when the local head's change is present in the
// preserved head under a different identity, the refusal SAYS SO and names
// the commit that carries it - and the diagnostic never becomes an automatic
// path. The refusal still stands, the head still does not move, and custody
// is still not stamped.
func TestRecoverDivergedRefusalReportsAPatchMatchAsADiagnosticOnly(t *testing.T) {
	t.Parallel()

	f := newRewrittenCopyRecoverFixture(t)
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)

	refused := f.service.Recover(f.ctx, false)
	if refused.Recovered || refused.Changed || refused.Safety != "blocked_recover_diverged" {
		t.Fatalf("recover diverged = %#v", refused)
	}
	t.Logf("refusal: %s", refused.Error)
	if !strings.Contains(refused.Error, f.rewritten) {
		t.Errorf("divergence refusal does not name the commit carrying the same patch (%s): %q", f.rewritten, refused.Error)
	}
	for _, want := range []string{f.localHead, f.preserved, "ancestry", "three-way merge"} {
		if !strings.Contains(refused.Error, want) {
			t.Errorf("divergence refusal does not name %q: %q", want, refused.Error)
		}
	}
	// The diagnostic is a diagnostic: nothing moved, nothing was adopted, and
	// the operator's decision is still required.
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.localHead {
		t.Fatalf("a patch match moved the local head to %s", got)
	}
	if f.custodyReturned() {
		t.Fatal("a patch match stamped custody")
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("the divergence refusal did not anchor the preserved commits: %s", got)
	}
}

// TestRecoverDivergedRefusalClaimsNoPatchMatchWhenNoneExists keeps the
// diagnostic honest: with no commit carrying the local head's change, the
// refusal must not imply that one exists.
func TestRecoverDivergedRefusalClaimsNoPatchMatchWhenNoneExists(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)
	mustWrite(t, filepath.Join(f.local, "rescope.txt"), "rescope\n")
	mustRun(t, f.local, "add", "rescope.txt")
	mustRun(t, f.local, "commit", "-m", "diverging rescope")

	refused := f.service.Recover(f.ctx, false)
	if refused.Safety != "blocked_recover_diverged" {
		t.Fatalf("recover diverged = %#v", refused)
	}
	if strings.Contains(refused.Error, "same patch") {
		t.Errorf("divergence refusal claims a patch match that does not exist: %q", refused.Error)
	}
}
