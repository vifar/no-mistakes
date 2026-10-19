package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The current-schema contract forbids even a no-op UPDATE: a competing WAL
// writer is an independent oracle that rejects any attempted write transaction.
func TestCurrentOpenWithHeldWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	initial, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	initial.Close()
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer writer.Exec("ROLLBACK")
	for i := 0; i < 2; i++ {
		d, err := Open(path)
		if err != nil {
			t.Fatalf("current open %d while writer holds transaction: %v", i, err)
		}
		if _, err := d.GetRepos(); err != nil {
			t.Fatal(err)
		}
		d.Close()
	}
}

func TestPreOwnerUpgradePreservesHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo("/fixture", "https://example.com/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec("ALTER TABLE runs DROP COLUMN launch_owner; PRAGMA user_version=0"); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.GetRun(run.ID)
	if err != nil || got == nil || got.HeadSHA != "head" || got.LaunchOwner != nil {
		t.Fatalf("upgrade lost history: %+v, %v", got, err)
	}
	if !hasColumn(t, d, "runs", "launch_owner") {
		t.Fatal("missing launch_owner")
	}
}

func TestMigrationErrorNamesSQL(t *testing.T) {
	original := migrationStatements
	const broken = "ALTER TABLE absent_table ADD COLUMN test_value TEXT"
	migrationStatements = append(append([]string{}, original...), broken)
	defer func() { migrationStatements = original }()
	_, err := Open(filepath.Join(t.TempDir(), "broken.sqlite"))
	if err == nil || !strings.Contains(err.Error(), broken) {
		t.Fatalf("migration error must name constant SQL: %v", err)
	}
}

func TestWatcherStatusUpgrade(t *testing.T) {
	for version := 0; version <= 3; version++ {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.sqlite")
			d, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			repo, err := d.InsertRepo("fixture", "https://example.com/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRun(repo.ID, "feature", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			publication := CommitStatusPublication{SHA: "head", Host: "example.com", Repo: "repo", LastState: "success"}
			if err := d.PutCommitStatusPublication(run.ID, publication); err != nil {
				t.Fatal(err)
			}
			if _, err := d.sql.Exec(fmt.Sprintf(`DROP TABLE status_validation_steps;
DROP TABLE status_validation_epochs;
DROP TABLE controlled_shutdown_runs;
ALTER TABLE commit_status_publications DROP COLUMN strict_certificate;
ALTER TABLE commit_status_publications DROP COLUMN preserve_success;
PRAGMA user_version=%d`, version)); err != nil {
				t.Fatal(err)
			}
			d.Close()
			d, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			got, err := d.GetRun(run.ID)
			if err != nil || got == nil || got.HeadSHA != "head" {
				t.Fatalf("history: %+v, %v", got, err)
			}
			pub, err := d.GetCommitStatusPublication(context.Background(), run.ID, publication)
			if err != nil || pub == nil || pub.LastState != "success" || pub.StrictCertificate != nil || pub.PreserveSuccess != nil {
				t.Fatalf("publication: %+v, %v", pub, err)
			}
			if err := d.InvalidateStatusValidation(run.ID); err != nil {
				t.Fatal(err)
			}
			epoch, err := d.StatusValidationEpoch(run.ID)
			if err != nil || epoch != 1 {
				t.Fatalf("epoch = %d, %v", epoch, err)
			}
			step, err := d.InsertStepResult(run.ID, "review")
			if err != nil {
				t.Fatal(err)
			}
			if err := d.RecordStatusValidationStep(step.ID, run.ID, "head", "config", epoch); err != nil {
				t.Fatal(err)
			}
			if err := d.PrepareControlledShutdown([]string{run.ID}, "instance", "lifecycle", "planned"); err != nil {
				t.Fatal(err)
			}
			matches, err := d.ControlledShutdownMatches(run.ID, "instance", "lifecycle", "planned")
			if err != nil || !matches {
				t.Fatalf("shutdown binding: %v, %v", matches, err)
			}
		})
	}
}
