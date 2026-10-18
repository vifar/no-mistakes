package cli

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Public reader commands share one journey's real WAL database with a live
// daemon and a synchronized writer. SQLite's one-writer rule rejects DDL and
// even no-op backfills; success therefore proves the readers acquire no write
// transaction. Concurrent db.Open is the harness-side opening contract.
func TestCLIReadersWithLiveDaemonAndHeldWriter(t *testing.T) {
	bin := buildNoMistakesBinary(t)
	repoDir, p, database, repo := setupReaderRepo(t)
	chdir(t, repoDir)
	head := cliGit(t, repoDir, "rev-parse", "HEAD")
	r, err := database.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(r.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	startTestDaemon(t, p, database)
	parked, err := database.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(parked.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(parked.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"summary":"Choose behavior","findings":[{"id":"question","severity":"warning","description":"Choose behavior","action":"ask-user"}]}`
	if _, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(parked.ID, step.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
		t.Fatal(err)
	}
	writer, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer writer.Exec("ROLLBACK")
	var before int
	if err := writer.QueryRow("PRAGMA schema_version").Scan(&before); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d, err := db.Open(p.DB())
		if err != nil {
			t.Errorf("concurrent harness open: %v", err)
			return
		}
		defer d.Close()
		if _, err := d.GetRunsByRepo(repo.ID); err != nil {
			t.Error(err)
		}
	}()
	for _, args := range [][]string{{"status"}, {"runs"}, {"axi"}, {"axi", "status"}, {"axi", "status", "--awaiting"}, {"axi", "status", "--json"}, {"axi", "status", "--awaiting", "--json"}, {"axi", "logs", "--step", "review"}} {
		wg.Add(1)
		go func(args []string) {
			defer wg.Done()
			out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
			t.Logf("nm %s: %v\n%s", strings.Join(args, " "), err, out)
			if err != nil {
				t.Errorf("reader %v while writer held: %v\n%s", args, err, out)
				return
			}
			want := parked.ID
			if args[0] == "runs" {
				want = head[:8]
			}
			if !strings.Contains(string(out), want) {
				t.Errorf("reader %v omitted persisted state %q: %s", args, want, out)
			}
		}(args)
	}
	wg.Wait()
	var after int
	if err := writer.QueryRow("PRAGMA schema_version").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("reader changed schema: %d -> %d", before, after)
	}
}

func TestCLIReadersRefuseOldSchemaUnchanged(t *testing.T) {
	repoDir, p, database, _ := setupReaderRepo(t)
	chdir(t, repoDir)
	database.Close()
	old, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec("ALTER TABLE runs DROP COLUMN launch_owner; PRAGMA user_version=0; PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	old.Close()
	before, err := os.ReadFile(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"status"}, {"runs"}, {"stats"}, {"axi"}, {"axi", "status"}, {"axi", "status", "--awaiting", "--json"}, {"axi", "logs", "--step", "review"}} {
		out, err := executeCmd(args...)
		if err == nil || !strings.Contains(out+err.Error(), "schema too old") || !strings.Contains(out+err.Error(), "daemon") {
			t.Errorf("%v must refuse old schema with daemon guidance: %v\n%s", args, err, out)
		}
	}
	after, err := os.ReadFile(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("reader changed old database bytes")
	}
}

func setupReaderRepo(t *testing.T) (string, *paths.Paths, *db.DB, *db.Repo) {
	t.Helper()
	dir := t.TempDir()
	cliGit(t, dir, "init", "-b", "main")
	cliGit(t, dir, "config", "user.name", "Test")
	cliGit(t, dir, "config", "user.email", "test@example.com")
	cliGit(t, dir, "commit", "--allow-empty", "-m", "initial")
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := paths.WithRoot(makeSocketSafeTempDir(t))
	t.Setenv("NM_HOME", p.Root())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	r, err := d.InsertRepo(dir, "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	return dir, p, d, r
}
