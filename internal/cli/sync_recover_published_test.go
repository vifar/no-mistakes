package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Reproduce #89: custody has already been returned, but the operator's branch
// still lacks the final pipeline fix. The published binding is the oracle.
func newCLIPublishedRecoveryFixture(t *testing.T) cliSyncFixture {
	t.Helper()
	f := newCLISyncFixture(t)
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.SetRunCustodyReturned(f.runID); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAxiSyncRecoverPublishedBehindFastForwards(t *testing.T) {
	f := newCLIPublishedRecoveryFixture(t)
	out, err := executeCmd("axi", "sync", "--recover")
	if err != nil {
		t.Fatalf("recover: %v\n%s", err, out)
	}
	for _, want := range []string{"state: synchronized", "changed: true", "recovered: true", "relation: equal"} {
		if !strings.Contains(out, want) {
			t.Errorf("recover missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
		t.Fatalf("HEAD = %s, want published head %s", got, f.pushed)
	}
}

func TestAxiSyncRecoverPublishedDivergedRefuses(t *testing.T) {
	f := newCLIPublishedRecoveryFixture(t)
	if err := os.WriteFile(filepath.Join(f.local, "local.txt"), []byte("local work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, f.local, "add", "local.txt")
	cliGit(t, f.local, "commit", "-m", "local work")
	head := cliGit(t, f.local, "rev-parse", "HEAD")
	out, err := executeCmd("axi", "sync", "--recover")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Errorf("recover error = %#v; want exit 1:\n%s", err, out)
	}
	// The refusal names the two heads it compared, and the handoff carries
	// both of them too (#151).
	for _, want := range []string{"changed: false", "state: diverged", f.pushed, "git log --oneline --left-right " + head + "..." + f.pushed} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != head {
		t.Fatalf("recovery discarded local head %s for %s", head, got)
	}
	if got := cliGit(t, f.local, "show", "HEAD:local.txt"); got != "local work" {
		t.Fatalf("local work = %q", got)
	}
}

func TestAxiSyncRecoverPublishedEqualIsNoOp(t *testing.T) {
	f := newCLIPublishedRecoveryFixture(t)
	cliGit(t, f.local, "fetch", f.remote, "refs/heads/feature/sync")
	cliGit(t, f.local, "merge", "--ff-only", f.pushed)
	out, err := executeCmd("axi", "sync", "--recover")
	if err != nil {
		t.Fatalf("recover: %v\n%s", err, out)
	}
	for _, want := range []string{"state: synchronized", "changed: false", "safety: already_synchronized"} {
		if !strings.Contains(out, want) {
			t.Errorf("no-op missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
		t.Fatalf("HEAD = %s, want %s", got, f.pushed)
	}
}

func TestAxiSyncRecoverPublishedAheadDoesNotRewind(t *testing.T) {
	f := newCLIPublishedRecoveryFixture(t)
	cliGit(t, f.local, "fetch", f.remote, "refs/heads/feature/sync")
	cliGit(t, f.local, "merge", "--ff-only", f.pushed)
	cliGit(t, f.local, "commit", "--allow-empty", "-m", "later local work")
	head := cliGit(t, f.local, "rev-parse", "HEAD")
	out, err := executeCmd("axi", "sync", "--recover")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Errorf("recover error = %#v; want explicit refusal:\n%s", err, out)
	}
	for _, want := range []string{"state: local_ahead", "changed: false", f.pushed, "code: run_pipeline"} {
		if !strings.Contains(out, want) {
			t.Errorf("ahead refusal missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != head {
		t.Fatalf("recovery rewound local head %s to %s", head, got)
	}
}

func TestAxiSyncRecoverPublishedFixSurvivesNextMerge(t *testing.T) {
	f := newCLIPublishedRecoveryFixture(t)
	cliGit(t, f.local, "checkout", "-b", "dev", f.base)
	if err := os.WriteFile(filepath.Join(f.local, "dev.txt"), []byte("dev change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, f.local, "add", "dev.txt")
	cliGit(t, f.local, "commit", "-m", "dev advances")
	cliGit(t, f.local, "checkout", "feature/sync")
	out, err := executeCmd("axi", "sync", "--recover")
	if err != nil {
		t.Fatalf("recover: %v\n%s", err, out)
	}
	// Assert the missing-commit scenario directly: dev ancestry alone used to
	// pass while fix.txt was absent both before and after this merge.
	fix, err := os.ReadFile(filepath.Join(f.local, "fix.txt"))
	if err != nil || string(fix) != "fix\n" {
		t.Errorf("recovered tree lacks pipeline fix: %q, %v", fix, err)
	}
	cliGit(t, f.local, "merge", "--no-ff", "--no-edit", "dev")
	cliGit(t, f.local, "merge-base", "--is-ancestor", "dev", "HEAD")
	fix, err = os.ReadFile(filepath.Join(f.local, "fix.txt"))
	if err != nil || string(fix) != "fix\n" {
		t.Fatalf("merge on recovered base lost pipeline fix: %q, %v", fix, err)
	}
	cliGit(t, f.local, "merge-base", "--is-ancestor", f.pushed, "HEAD")
}

func TestAxiSyncRecoverPublishedDirtyRefuses(t *testing.T) {
	f := newCLIPublishedRecoveryFixture(t)
	const contents = "uncommitted local work\n"
	path := filepath.Join(f.local, "file.txt")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := executeCmd("axi", "sync", "--recover")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Errorf("recover error = %#v; want exit 1:\n%s", err, out)
	}
	for _, want := range []string{"state: dirty", "changed: false", "command: git status"} {
		if !strings.Contains(out, want) {
			t.Errorf("dirty refusal missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.old {
		t.Fatalf("dirty recovery moved HEAD to %s", got)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != contents {
		t.Fatalf("dirty recovery changed local work: %q, %v", got, err)
	}
}

func TestAxiSyncRecoverPublishedEquivalentDivergenceStillRefuses(t *testing.T) {
	f := newCLIPublishedRecoveryFixture(t)
	rewriteCLIPipelineHead(t, &f, []pipelineCommitForCLI{
		{message: "feature rebased", files: map[string]string{"file.txt": "feature\n"}},
		{message: "pipeline fix", files: map[string]string{"fix.txt": "fix\n"}},
	})
	// Ordinary sync can adopt this equivalent rewrite. Recovery's published
	// head contract is stricter: divergence must require an operator decision.
	out, err := executeCmd("axi", "sync", "--check")
	if err != nil || !strings.Contains(out, "safety: safe_equivalent_advance") {
		t.Fatalf("fixture is not eligible for ordinary equivalent sync: %v\n%s", err, out)
	}
	out, err = executeCmd("axi", "sync", "--recover")
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("equivalent recovery error = %#v; want exit 1:\n%s", err, out)
	}
	for _, want := range []string{"safety: blocked_recover_diverged", "changed: false", f.pushed} {
		if !strings.Contains(out, want) {
			t.Errorf("equivalent refusal missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.old {
		t.Fatalf("recovery reset the equivalent divergent branch to %s", got)
	}
}

func TestHumanSyncRecoverPublishedBehindFastForwards(t *testing.T) {
	f := newCLIPublishedRecoveryFixture(t)
	out, err := executeCmd("sync", "--recover", "--yes")
	if err != nil {
		t.Fatalf("human recovery: %v\n%s", err, out)
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
		t.Fatalf("human recovery HEAD = %s, want %s", got, f.pushed)
	}
}
