//go:build e2e && !windows

package e2e

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A supported version-two database must actually migrate at daemon startup,
// survive a writer held beyond the busy timeout, and preserve existing history.
// The current-schema control reads CLI status while another writer is held.
// Daemon recovery itself may write; the DB's current-schema Open control lives
// in TestCurrentOpenWithHeldWriter, separately from that recovery behavior.
func TestSQLiteMigrationContentionOnRealCLI(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statusCommand(t, h, h.WorkDir, "init")
	statusCommand(t, h, h.WorkDir, "daemon", "stop", "--force")
	path := filepath.Join(h.NMHome, "state.sqlite")
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repos, err := database.GetRepos()
	if err != nil || len(repos) != 1 {
		t.Fatalf("expected initialized repository: %+v %v", repos, err)
	}
	head := h.UpstreamBranchSHA("main")
	run, err := database.InsertRun(repos[0].ID, "historical", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	var currentVersion int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&currentVersion); err != nil || currentVersion <= 2 {
		t.Fatalf("unexpected current schema %d: %v", currentVersion, err)
	}
	// Version three added this table. Removing it models the supported
	// version-two schema, rather than relabeling a complete current schema.
	if _, err := raw.Exec("DROP TABLE push_notifications; PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	var version, tables int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("old-schema precondition: version=%d err=%v", version, err)
	}
	if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='push_notifications'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("old schema already has notification table: %d %v", tables, err)
	}
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE runs SET closing_issue_refs='example/repo#92' WHERE id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(sqlitewrite.BusyTimeout + time.Second)
		released <- tx.Commit()
	}()
	started := time.Now()
	out, startErr := h.Run("daemon", "start")
	elapsed := time.Since(started)
	releaseErr := <-released
	if startErr != nil || releaseErr != nil {
		t.Fatalf("startup=%v release=%v\n%s", startErr, releaseErr, out)
	}
	if elapsed < sqlitewrite.BusyTimeout {
		t.Fatalf("startup bypassed the required migration: %v", elapsed)
	}
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != currentVersion {
		t.Fatalf("migration version=%d want=%d: %v", version, currentVersion, err)
	}
	if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='push_notifications'").Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("migration omitted notification table: %d %v", tables, err)
	}
	stored := h.RunInfo(run.ID)
	if stored.Status != types.RunCompleted || stored.HeadSHA != head || stored.RepoID != repos[0].ID {
		t.Fatalf("migration changed historical run: %+v", stored)
	}
	var refs string
	if err := raw.QueryRow("SELECT closing_issue_refs FROM runs WHERE id=?", run.ID).Scan(&refs); err != nil || refs != "example/repo#92" {
		t.Fatalf("migration lost competing writer: %q %v", refs, err)
	}
	t.Logf("real CLI startup migrated schema 2 -> %d in %v after six-second writer; retained completed run %s, head %s, closing refs %s\n%s", version, elapsed, run.ID, head, refs, out)
	control, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer control.Rollback()
	if _, err := control.Exec("UPDATE runs SET closing_issue_refs=closing_issue_refs WHERE id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	out, startErr = h.Run("status")
	elapsed = time.Since(started)
	if startErr != nil || elapsed >= sqlitewrite.BusyTimeout || !strings.Contains(out, "running") {
		t.Fatalf("current-schema status blocked under held writer: elapsed=%v err=%v\n%s", elapsed, startErr, out)
	}
	t.Logf("current-schema real CLI status completed in %v while writer remained held\n%s", elapsed, out)
	if err := control.Rollback(); err != nil {
		t.Fatal(err)
	}
}
