package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
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

func TestUpstream199UpgradeFromForkSchemas(t *testing.T) {
	for _, version := range []int{0, 4, 5, 7} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.sqlite")
			var d *DB
			if version == 7 {
				schema, err := os.ReadFile("testdata/migrations/base-7b2fb05b.sql")
				if err != nil {
					t.Fatal(err)
				}
				sqlDB, err := openMigrationSQLite(path)
				if err != nil {
					t.Fatal(err)
				}
				d = &DB{sql: sqlDB}
				if _, err := d.sql.Exec(string(schema)); err != nil {
					d.Close()
					t.Fatal(err)
				}
			} else {
				var err error
				d, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			repo, err := d.InsertRepo("fixture", "https://example.com/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRun(repo.ID, "feature", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			if version != 7 {
				if _, err := d.sql.Exec(fmt.Sprintf(`
DROP TABLE run_retries;
ALTER TABLE runs DROP COLUMN quota_routing;
ALTER TABLE runs DROP COLUMN ci_released_at;
ALTER TABLE step_results DROP COLUMN response_origin;
ALTER TABLE step_rounds DROP COLUMN response_origin;
ALTER TABLE step_results DROP COLUMN failure_cause;
ALTER TABLE step_results DROP COLUMN carried_from;
PRAGMA user_version=%d`, version)); err != nil {
					t.Fatal(err)
				}
			}
			for table, columns := range map[string][]string{
				"runs":         {"quota_routing", "ci_released_at"},
				"step_results": {"response_origin", "failure_cause", "carried_from"},
				"step_rounds":  {"response_origin"},
			} {
				for _, column := range columns {
					if hasColumn(t, d, table, column) {
						t.Fatalf("historical fixture contains %s.%s", table, column)
					}
				}
			}
			var retryTables int
			if err := d.sql.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='run_retries'").Scan(&retryTables); err != nil || retryTables != 0 {
				t.Fatalf("historical retry table count = %d, %v", retryTables, err)
			}
			var historicalVersion int
			if err := d.sql.QueryRow("PRAGMA user_version").Scan(&historicalVersion); err != nil || historicalVersion != version {
				t.Fatalf("historical schema = %d, %v; want %d", historicalVersion, err, version)
			}
			d.Close()
			if reader, err := OpenReader(path); err == nil {
				reader.Close()
				t.Fatal("reader accepted an outdated schema")
			}
			for i := 0; i < 2; i++ {
				d, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
				var upgradedVersion int
				if err := d.sql.QueryRow("PRAGMA user_version").Scan(&upgradedVersion); err != nil || upgradedVersion != 8 {
					t.Fatalf("upgraded schema = %d, %v; want 8", upgradedVersion, err)
				}
				got, err := d.GetRun(run.ID)
				if err != nil || got == nil || got.HeadSHA != "head" {
					t.Fatalf("history after upgrade: %+v, %v", got, err)
				}
				for table, columns := range map[string][]string{
					"runs":         {"quota_routing", "ci_released_at"},
					"step_results": {"response_origin", "failure_cause", "carried_from"},
					"step_rounds":  {"response_origin"},
				} {
					for _, column := range columns {
						if !hasColumn(t, d, table, column) {
							t.Fatalf("missing %s.%s", table, column)
						}
					}
				}
				if retry, err := d.GetRunRetry(run.ID); err != nil || retry != nil {
					t.Fatalf("retry history after upgrade: %+v, %v", retry, err)
				}
				d.Close()
			}
			reader, err := OpenReader(path)
			if err != nil {
				t.Fatal(err)
			}
			reader.Close()
		})
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

// TestUpgradeAddsTheLeftoverValidationBudget proves an EXISTING database
// receives the new column. A database that does not get it fails every
// leftover-preservation path with "no such column", so the upgrade is the
// difference between the feature working and the feature refusing.
func TestUpgradeAddsTheLeftoverValidationBudget(t *testing.T) {
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
	// An existing installation: the column is absent and the schema is the
	// version before this one.
	if _, err := d.sql.Exec(fmt.Sprintf("ALTER TABLE runs DROP COLUMN leftover_validation_commits; PRAGMA user_version=%d", schemaVersion-1)); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if !hasColumn(t, d, "runs", "leftover_validation_commits") {
		t.Fatal("the upgrade did not add the leftover-validation budget column")
	}
	allowed, err := d.CanCommitLeftoverValidationEdits(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("the upgraded run cannot spend its leftover-validation budget")
	}
	// The upgrade preserved the run's history.
	got, err := d.GetRun(run.ID)
	if err != nil || got == nil || got.HeadSHA != "head" {
		t.Fatalf("the upgrade lost history: %+v, %v", got, err)
	}
}

// TestLegacyUpgradeAddsTheLeftoverValidationBudget proves the LEGACY
// (unversioned) upgrade also receives the new column. That path creates its
// tables with CREATE TABLE IF NOT EXISTS, so an existing runs table keeps its
// old shape, and the database is stamped with the current version afterwards
// - so skipping the additive statements there would leave it permanently
// missing them.
func TestLegacyUpgradeAddsTheLeftoverValidationBudget(t *testing.T) {
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
	// A legacy database: the column is absent and the schema is unversioned.
	if _, err := d.sql.Exec("ALTER TABLE runs DROP COLUMN leftover_validation_commits; PRAGMA user_version=0"); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if !hasColumn(t, d, "runs", "leftover_validation_commits") {
		t.Fatal("the legacy upgrade did not add the leftover-validation budget column")
	}
	allowed, err := d.CanCommitLeftoverValidationEdits(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("the upgraded run cannot spend its leftover-validation budget")
	}
	got, err := d.GetRun(run.ID)
	if err != nil || got == nil || got.HeadSHA != "head" {
		t.Fatalf("the legacy upgrade lost history: %+v, %v", got, err)
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
	for version := 0; version <= 4; version++ {
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
