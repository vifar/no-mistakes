package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// recoveredRewriteEnv reproduces issue #97 (vifar/controller#8777): a
// published branch is hand-rebased and force-pushed outside the pipeline,
// so the private mirror still holds the published pipeline head the rebase
// no longer contains.
type recoveredRewriteEnv struct {
	f       cliSyncFixture
	env     *axiEnv
	gateDir string
	rebased string
}

func newRecoveredRewriteEnv(t *testing.T) *recoveredRewriteEnv {
	t.Helper()
	f := newCLISyncFixture(t)
	if out, err := executeCmd("axi", "sync"); err != nil {
		t.Fatalf("initial sync: %v\n%s", err, out)
	}
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	registeredRoot, err := git.FindGitRoot(f.local)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.GetRepoByPath(registeredRoot)
	if err != nil || repo == nil {
		t.Fatalf("registered repo: %v", err)
	}
	gateDir := p.RepoDir(repo.ID)
	cliGit(t, filepath.Dir(gateDir), "clone", "--bare", f.remote, gateDir)
	cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
	cliGit(t, f.local, "remote", "add", gate.RemoteName, gateDir)

	cliGit(t, f.local, "reset", "--hard", f.base)
	for name, content := range map[string]string{"file.txt": "feature, rebased by hand\n", "fix.txt": "fix, rebased by hand\n"} {
		if err := os.WriteFile(filepath.Join(f.local, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cliGit(t, f.local, "add", "file.txt", "fix.txt")
	cliGit(t, f.local, "commit", "-m", "hand-rebased feature")
	rebased := cliGit(t, f.local, "rev-parse", "HEAD")
	cliGit(t, f.local, "push", "--force", f.remote, "HEAD:refs/heads/feature/sync")
	return &recoveredRewriteEnv{
		f: f, gateDir: gateDir, rebased: rebased,
		env: &axiEnv{p: p, d: d, repo: repo, cfg: config.DefaultGlobalConfig()},
	}
}

// serveNoRun answers the submission's daemon calls the way a daemon that
// started nothing would, so the assertions are the mirror and the push.
func (e *recoveredRewriteEnv) serveNoRun(t *testing.T) {
	t.Helper()
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetRunsResult{}, nil
	})
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetActiveRunResult{}, nil
	})
	srv.Handle(ipc.MethodRerun, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.RerunResult{}, nil
	})
	// The fixture's NM_HOME is too deep for a Unix socket; the submission
	// reaches the daemon only through env.client.
	socket := paths.WithRoot(makeSocketSafeTempDir(t)).Socket()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(socket) }()
	t.Cleanup(func() { srv.Close(); <-done })
	deadline := time.Now().Add(3 * time.Second)
	for {
		client, err := ipc.Dial(socket)
		if err == nil {
			e.env.client = client
			t.Cleanup(func() { client.Close() })
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// After `axi sync --recover` accepts the rewrite, the next `axi run`
// submission retires the private mirror instead of refusing, and its output
// names the archive tag and the recovery anchor that keep the retired commits.
func TestAxiRunAfterRecoveredRewriteRetiresTheMirrorAndNamesTheArchive(t *testing.T) {
	e := newRecoveredRewriteEnv(t)
	anchor := "refs/no-mistakes/recover-rewritten/" + e.f.runID + "/1"

	out, err := executeCmd("axi", "sync", "--recover")
	if err != nil {
		t.Fatalf("recover: %v\n%s", err, out)
	}
	for _, want := range []string{"recovered: true", "archive_ref: " + anchor, "is preserved by " + anchor} {
		if !strings.Contains(out, want) {
			t.Errorf("recover output missing %q:\n%s", want, out)
		}
	}
	if got := cliGit(t, e.gateDir, "rev-parse", anchor); got != e.f.pushed {
		t.Fatalf("gate anchor = %s, want %s", got, e.f.pushed)
	}

	e.serveNoRun(t)
	notices := new(bytes.Buffer)
	e.env.notices = notices
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := triggerRun(ctx, e.env, "feature/sync", nil, "Closes #97", "", false, "", "", nil); err != nil {
		var refusal *staleMirrorError
		if errors.As(err, &refusal) {
			t.Fatalf("axi run still refused the recovered rewrite: %v", err)
		}
	}
	if got := cliGit(t, e.gateDir, "rev-parse", "refs/heads/feature/sync"); got != e.rebased {
		t.Fatalf("gate branch = %s, want the submitted head %s", got, e.rebased)
	}
	archive := "refs/tags/no-mistakes-abandoned/feature/sync/" + e.f.pushed
	if got := cliGit(t, e.gateDir, "rev-parse", archive); got != e.f.pushed {
		t.Fatalf("archive = %s, want %s", got, e.f.pushed)
	}
	note := notices.String()
	for _, want := range []string{"retired the private mirror", e.f.pushed, archive, "preserved by " + anchor} {
		if !strings.Contains(note, want) {
			t.Errorf("axi run notice missing %q:\n%s", want, note)
		}
	}
}

// Without the recovery, the same submission keeps today's refusal and leaves
// the mirror exactly where it was.
func TestAxiRunWithoutRecoveredRewriteStillRefusesTheMirror(t *testing.T) {
	e := newRecoveredRewriteEnv(t)
	notices := new(bytes.Buffer)
	e.env.notices = notices

	_, err := preparePrivateMirror(context.Background(), e.env, "feature/sync", e.rebased)
	var refusal *staleMirrorError
	if !errors.As(err, &refusal) {
		t.Fatalf("unrecovered rewrite was not refused: %v", err)
	}
	want := "the private mirror for \"feature/sync\" holds 2 commit(s) the current head " + e.rebased + " does not contain (mirror at " + e.f.pushed + "); it was left untouched"
	if got := refusal.Summary(); got != want {
		t.Fatalf("summary = %q, want today's %q", got, want)
	}
	if got := cliGit(t, e.gateDir, "rev-parse", "refs/heads/feature/sync"); got != e.f.pushed {
		t.Fatalf("refusal moved the mirror to %s", got)
	}
	if notices.Len() != 0 {
		t.Fatalf("a refused submission reported a retirement: %s", notices.String())
	}
}
