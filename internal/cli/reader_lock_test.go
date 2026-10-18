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

// The user journey's outside-Git contract is independent of whether the user
// has initialized a database. Exercise the command tree and its exit/output
// contract: a missing database must not mask the repository diagnostic or be
// created just to produce it.
func TestRepositoryReadersOutsideGit(t *testing.T) {
	chdir(t, t.TempDir())
	for _, initialized := range []bool{false, true} {
		name := "missing_database"
		if initialized {
			name = "current_database"
		}
		t.Run(name, func(t *testing.T) {
			p := paths.WithRoot(filepath.Join(t.TempDir(), "nmhome"))
			t.Setenv("NM_HOME", p.Root())
			if initialized {
				if err := p.EnsureDirs(); err != nil {
					t.Fatal(err)
				}
				d, err := db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				d.Close()
			}
			for _, args := range [][]string{{"status"}, {"runs"}, {"axi"}, {"axi", "status"}, {"axi", "status", "--json"}, {"axi", "status", "--awaiting"}, {"axi", "status", "--awaiting", "--json"}, {"axi", "logs", "--step", "review"}} {
				t.Run(strings.Join(args, " "), func(t *testing.T) {
					out, err := executeCmd(args...)
					if (err == nil) != (args[0] == "status") {
						t.Errorf("exit error = %v; only human status should succeed outside Git", err)
					}
					if err != nil {
						out += err.Error()
					}
					if !strings.Contains(out, "not in a git repository") {
						t.Errorf("missing repository diagnostic: %s", out)
					}
				})
			}
			if !initialized {
				if _, err := os.Stat(p.Root()); !os.IsNotExist(err) {
					t.Fatalf("reader created application state: %v", err)
				}
			}
		})
	}
}

func TestRepositoryReadersOutsideGitIgnoreAdverseState(t *testing.T) {
	chdir(t, t.TempDir())
	for _, fixture := range []string{"blocked_home", "old_schema"} {
		t.Run(fixture, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "nmhome")
			var before []byte
			if fixture == "blocked_home" {
				if err := os.WriteFile(root, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				old, err := sql.Open("sqlite", filepath.Join(root, "state.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := old.Exec("CREATE TABLE legacy (id INTEGER)"); err != nil {
					t.Fatal(err)
				}
				if err := old.Close(); err != nil {
					t.Fatal(err)
				}
				before, err = os.ReadFile(filepath.Join(root, "state.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("NM_HOME", root)
			for _, args := range [][]string{{"status"}, {"runs"}, {"axi"}, {"axi", "status"}, {"axi", "status", "--json"}, {"axi", "status", "--awaiting"}, {"axi", "status", "--awaiting", "--json"}, {"axi", "logs", "--step", "review"}} {
				t.Run(strings.Join(args, " "), func(t *testing.T) {
					out, err := executeCmd(args...)
					if args[0] == "status" {
						if err != nil || out != "not in a git repository\n" {
							t.Fatalf("human status must succeed with exact Git diagnostic before accessing adverse state: err=%v output=%q", err, out)
						}
					} else if err == nil || !strings.Contains(out+err.Error(), "not in a git repository") {
						t.Fatalf("repository query must fail with Git diagnostic before accessing adverse state: err=%v output=%q", err, out)
					}
				})
			}
			if fixture == "old_schema" {
				after, err := os.ReadFile(filepath.Join(root, "state.sqlite"))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("outside-Git queries changed old database: %v", err)
				}
			}
		})
	}
}

func TestExplicitAndGlobalReadersOutsideGit(t *testing.T) {
	_, _, database, repo := setupReaderRepo(t)
	r, err := database.InsertRun(repo.ID, "main", "abcdef123", "abcdef123")
	if err != nil {
		t.Fatal(err)
	}
	chdir(t, t.TempDir())
	for _, args := range [][]string{{"stats"}, {"axi", "status", "--run", r.ID}, {"axi", "status", "--run", r.ID, "--json"}, {"axi", "logs", "--run", r.ID, "--step", "review"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, err := executeCmd(args...)
			if err != nil {
				t.Fatalf("reader independent of current repository failed: %v\n%s", err, out)
			}
			if args[0] == "axi" && !strings.Contains(out, r.ID) {
				t.Fatalf("explicit reader omitted selected run %s: %s", r.ID, out)
			}
		})
	}
}

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
