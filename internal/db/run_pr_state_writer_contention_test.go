package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// The CI writer must survive a concurrent closing-issue writer, preserve its
// update, and record the observed PR state after the lock is released (#92).
func TestUpdateRunPRStateRetriesConcurrentClosingIssueWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo("/contention", "https://github.com/example/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	holder.SetMaxOpenConns(1)
	tx, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE runs SET closing_issue_refs = ? WHERE id = ?`, `example/repo#92`, run.ID); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		// Exceed the existing five-second busy timeout without changing it.
		time.Sleep(6 * time.Second)
		released <- tx.Commit()
	}()
	start := time.Now()
	writeErr := d.UpdateRunPRState(run.ID, "open")
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if writeErr != nil {
		t.Fatalf("CI PR-state update failed instead of retrying concurrent writer: %v", writeErr)
	}
	var state, refs string
	if err := d.sql.QueryRow(`SELECT pr_state, closing_issue_refs FROM runs WHERE id = ?`, run.ID).Scan(&state, &refs); err != nil {
		t.Fatal(err)
	}
	if state != "open" || refs != `example/repo#92` {
		t.Fatalf("lost concurrent write or PR observation: state=%q refs=%q", state, refs)
	}
	stored, err := d.GetRun(run.ID)
	if err != nil || len(stored.ClosingIssueRefs) != 1 || stored.ClosingIssueRefs[0] != "example/repo#92" {
		t.Fatalf("concurrent write is not readable through the run interface: %+v %v", stored, err)
	}
	t.Logf("closing-issue transaction held 6s; PR-state write succeeded after %v; pr_state=%q closing_issue_refs=%s", time.Since(start), state, refs)
}
