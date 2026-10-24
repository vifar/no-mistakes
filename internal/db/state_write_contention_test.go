package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// competingWriter holds SQLite's write lock from another connection, as a
// second process or a long daemon transaction would.
func competingWriter(t *testing.T, d *DB) *sql.DB {
	t.Helper()
	var path string
	if err := d.sql.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close() })
	holder.SetMaxOpenConns(1)
	if _, err := holder.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	return holder
}

func stateWriteFixture(t *testing.T) (*DB, *Run, *StepResult) {
	t.Helper()
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	return d, run, step
}

// The writes that failed finished runs on a busy root land once the lock frees.
func TestContendedRunStateWritesSucceedAfterRetry(t *testing.T) {
	for _, operation := range []string{"UpdateRunPRState", "SetStepFindings"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			d, run, step := stateWriteFixture(t)
			holder := competingWriter(t, d)
			released := make(chan error, 1)
			go func() {
				time.Sleep(sqlitewrite.BusyTimeout + time.Second)
				_, err := holder.Exec("ROLLBACK")
				released <- err
			}()
			var err error
			switch operation {
			case "UpdateRunPRState":
				err = d.UpdateRunPRState(run.ID, "open")
			case "SetStepFindings":
				err = d.SetStepFindings(step.ID, `{"summary":"ok"}`)
			}
			if releaseErr := <-released; releaseErr != nil {
				t.Fatal(releaseErr)
			}
			if err != nil {
				t.Fatalf("contended %s did not retry to success: %v", operation, err)
			}
			stored, err := d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			storedStep, err := d.GetStepResult(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "UpdateRunPRState" && (stored.PRState == nil || *stored.PRState != "open") {
				t.Fatalf("PR state not written: %+v", stored.PRState)
			}
			if operation == "SetStepFindings" && (storedStep.FindingsJSON == nil || *storedStep.FindingsJSON != `{"summary":"ok"}`) {
				t.Fatalf("findings not written: %+v", storedStep.FindingsJSON)
			}
		})
	}
}

// Step activity is a heartbeat: under a held lock it is skipped within its
// short budget instead of queueing for the full state-write budget.
func TestTouchStepActivityIsBestEffortUnderContention(t *testing.T) {
	t.Parallel()
	d, _, step := stateWriteFixture(t)
	holder := competingWriter(t, d)
	defer holder.Exec("ROLLBACK")
	start := time.Now()
	err := d.TouchStepActivity(step.ID, "agent output")
	elapsed := time.Since(start)
	if !sqlitewrite.IsContention(err) {
		t.Fatalf("error=%T %v", err, err)
	}
	if elapsed >= sqlitewrite.Budget || elapsed > sqlitewrite.HeartbeatBudget+sqlitewrite.BusyTimeout+2*time.Second {
		t.Fatalf("heartbeat waited %v; want about %v", elapsed, sqlitewrite.HeartbeatBudget)
	}
}
