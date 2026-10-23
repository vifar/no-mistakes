package branchsync

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
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
	checkRecoverPatchMatchDiagnostic(t, 0)
}

func TestRecoverDivergedRefusalReportsBoundedSearchBeyondLimit(t *testing.T) {
	t.Parallel()
	checkRecoverPatchMatchDiagnostic(t, 501)
}

func checkRecoverPatchMatchDiagnostic(t *testing.T, padding int) {
	t.Helper()
	f := newRewrittenCopyRecoverFixture(t)
	if padding > 0 {
		configureIdentity(t, f.gate)
		tree := mustRun(t, f.gate, "rev-parse", f.preserved+"^{tree}")
		for i := 0; i < padding; i++ {
			f.preserved = mustRun(t, f.gate, "commit-tree", tree, "-p", f.preserved, "-m", fmt.Sprintf("later history %d", i))
		}
		if err := f.db.UpdateRunHeadSHA(f.run.ID, f.preserved); err != nil {
			t.Fatal(err)
		}
		if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunCancelled, f.preserved); err != nil {
			t.Fatal(err)
		}
		mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.preserved)
	}
	if padding == 0 {
		assertRecoverPatchMatchDiagnostic(t, f)
		return
	}
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)
	refused := f.service.Recover(f.ctx, false)
	if refused.Safety != "blocked_recover_diverged" || refused.Recovered || refused.Changed || !strings.Contains(refused.Error, "the search was bounded") {
		t.Fatalf("bounded search refusal = %#v", refused)
	}
	if f.custodyReturned() || mustRun(t, f.local, "rev-parse", "HEAD") != f.localHead {
		t.Fatal("bounded search changed custody or the local head")
	}
}

func TestRecoverDivergedRefusalReportsPatchMatchesWithConfiguredDiff(t *testing.T) {
	t.Parallel()
	for _, setting := range []struct{ key, value string }{
		{"color.ui", "always"},
		{"diff.external", "printf 'not a Git diff'"},
		{"diff.custom.command", "printf 'not a Git diff'"},
		{"diff.custom.textconv", "printf 'converted bytes'"},
	} {
		t.Run(setting.key, func(t *testing.T) {
			f := newRewrittenCopyRecoverFixture(t)
			mustWrite(t, filepath.Join(f.local, ".git", "info", "attributes"), "file.txt diff=custom\n")
			mustRun(t, f.local, "config", setting.key, setting.value)
			assertRecoverPatchMatchDiagnostic(t, f)
		})
	}
}

func assertRecoverPatchMatchDiagnostic(t *testing.T, f *rewrittenCopyFixture) {
	t.Helper()
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)

	refused := f.service.Recover(f.ctx, false)
	if refused.Recovered || refused.Changed || refused.Safety != "blocked_recover_diverged" {
		t.Fatalf("recover diverged = %#v", refused)
	}
	t.Logf("refusal: %s", refused.Error)
	if !strings.Contains(refused.Error, shortCommit(f.ctx, f.local, f.rewritten)) {
		t.Errorf("divergence refusal does not name the commit carrying the same patch (%s): %q", f.rewritten, refused.Error)
	}
	for _, want := range []string{f.localHead, f.preserved, "ancestry", "three-way merge", "patch-ID matches", "a lead for the operator, not a finding of containment", "hunk locations and whitespace", "this needs an operator decision"} {
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
	if !strings.Contains(refused.Error, "no commit in the preserved pipeline head carries the same patch identity") {
		t.Errorf("divergence refusal omits the completed no-match result: %q", refused.Error)
	}
	if strings.Contains(refused.Error, "patch-ID matches") {
		t.Errorf("divergence refusal claims a patch match that does not exist: %q", refused.Error)
	}
}

func TestDivergenceDiagnosticFindsReplayDespitePresentationConfig(t *testing.T) {
	for _, setting := range []struct{ key, value string }{
		{"color.ui", "always"},
		{"diff.submodule", "log"},
		{"diff.ignoreSubmodules", "all"},
	} {
		t.Run(setting.key, func(t *testing.T) {
			dir := t.TempDir()
			mustRun(t, dir, "init", "-b", "main")
			configureIdentity(t, dir)
			mustRun(t, dir, "commit", "--allow-empty", "-m", "old module")
			old := mustRun(t, dir, "rev-parse", "HEAD")
			mustRun(t, dir, "commit", "--allow-empty", "-m", "new module")
			newHead := mustRun(t, dir, "rev-parse", "HEAD")
			mustRun(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+old+",module")
			mustRun(t, dir, "commit", "-m", "base pointer")
			base := mustRun(t, dir, "rev-parse", "HEAD")
			baseTree := mustRun(t, dir, "rev-parse", "HEAD^{tree}")
			mustRun(t, dir, "update-index", "--cacheinfo", "160000,"+newHead+",module")
			mustRun(t, dir, "commit", "-m", "local pointer change")
			local := mustRun(t, dir, "rev-parse", "HEAD")
			tree := mustRun(t, dir, "rev-parse", "HEAD^{tree}")
			upstream := mustRun(t, dir, "commit-tree", baseTree, "-p", base, "-m", "upstream")
			replay := mustRun(t, dir, "commit-tree", tree, "-p", upstream, "-m", "replay")
			ctx := context.Background()
			patch := fmt.Sprintf("diff --git a/module b/module\nindex %.7s..%.7s 160000\n--- a/module\n+++ b/module\n@@ -1 +1 @@\n-Subproject commit %s\n+Subproject commit %s\n", old, newHead, old, newHead)
			oracle, err := git.RunWithInput(ctx, dir, patch, "patch-id", "--stable")
			if err != nil || len(strings.Fields(oracle)) != 2 {
				t.Fatalf("specified gitlink patch identity = %q, %v", oracle, err)
			}
			want := strings.Fields(oracle)[0]
			mustRun(t, dir, "config", setting.key, setting.value)
			legacyStream := mustRun(t, dir, "log", "--format=commit %H", "-p", "-M", "--no-textconv", "--no-merges", "--no-ext-diff", replay, "^"+local)
			legacyIDs, err := git.StablePatchIdentities(ctx, dir, legacyStream)
			if err != nil {
				t.Fatal(err)
			}
			if len(legacyIDs) != 0 {
				t.Fatalf("fixture does not reproduce the lost search identity: %v", legacyIDs)
			}
			identity, ok := commitPatchIdentity(ctx, dir, local)
			if !ok || identity != want {
				t.Fatalf("local identity = %q, %v; want %s", identity, ok, want)
			}
			diagnostic := collectDivergenceDiagnostic(ctx, dir, local, replay, "preserved pipeline head")
			if diagnostic.PatchIdentity.Status != patchIdentityMatch || diagnostic.PatchIdentity.Match != mustRun(t, dir, "rev-parse", "--short", replay) {
				t.Fatalf("replay diagnostic = %#v", diagnostic.PatchIdentity)
			}
			if got := mustRun(t, dir, "rev-parse", "HEAD"); got != local {
				t.Fatalf("diagnostic moved local head to %s", got)
			}
		})
	}
}

func TestDivergenceDescriptionPreservesSearchOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		evidence patchIdentityEvidence
		want     string
	}{
		{"completed", patchIdentityEvidence{Status: patchIdentityNoMatch}, "no commit in the preserved pipeline head carries the same patch identity"},
		{"bounded", patchIdentityEvidence{Status: patchIdentityNoMatch, Bounded: true}, "the search was bounded"},
		{"interrupted", patchIdentityEvidence{Status: patchIdentityIncomplete}, "did not finish"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := divergenceDiagnostic{LocalHead: "local", PreservedHead: "preserved", PatchIdentity: tc.evidence}
			if got := d.divergenceDescription(); !strings.Contains(got, tc.want) {
				t.Fatalf("search outcome missing %q: %s", tc.want, got)
			}
		})
	}
}
