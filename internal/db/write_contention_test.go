package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPublicWritersRetryPastBusyTimeout(t *testing.T) {
	for _, surface := range []string{"run_insert", "step_result", "launch_receipt", "migration"} {
		t.Run(surface, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "state.sqlite")
			d, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			repo, err := d.InsertRepo("/test", "git@github.com:test/test.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "nonce", "generation", "digest", "", false, nil)
			if err != nil {
				t.Fatal(err)
			}
			if surface == "migration" {
				if _, err := d.sql.Exec("DROP TABLE push_notifications; PRAGMA user_version=2"); err != nil {
					t.Fatal(err)
				}
				var version int
				if err := d.sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
					t.Fatalf("migration requires supported older schema 2, got %d: %v", version, err)
				}
				var tables int
				if err := d.sql.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='push_notifications'").Scan(&tables); err != nil || tables != 0 {
					t.Fatalf("version-two fixture already has notification table: %d, %v", tables, err)
				}
			}
			holder, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			holder.SetMaxOpenConns(1)
			defer holder.Close()
			if _, err := holder.Exec("BEGIN EXCLUSIVE"); err != nil {
				t.Fatal(err)
			}
			released := make(chan error, 1)
			go func() {
				time.Sleep(sqlitewrite.BusyTimeout + time.Second)
				_, err := holder.Exec("ROLLBACK")
				released <- err
			}()
			defer func() {
				if releaseErr := <-released; releaseErr != nil {
					t.Error(releaseErr)
				}
			}()
			start := time.Now()
			var elapsed time.Duration
			switch surface {
			case "run_insert":
				inserted, writeErr := d.InsertRun(repo.ID, "other", "head", "base")
				elapsed = time.Since(start)
				err = writeErr
				if err == nil && (inserted == nil || inserted.Status != types.RunPending) {
					t.Error("run not admitted")
				}
			case "step_result":
				step, writeErr := d.InsertStepResult(run.ID, types.StepReview)
				elapsed = time.Since(start)
				err = writeErr
				if err == nil && step.ID == "" {
					t.Error("step not recorded")
				}
			case "launch_receipt":
				receipt, claimed, writeErr := d.ClaimLaunchReceipt(repo.ID, "feature", "nonce", "head", "generation", "digest", "", false, nil)
				elapsed = time.Since(start)
				err = writeErr
				if err == nil && (receipt == nil || !claimed) {
					t.Error("receipt not claimed")
				}
				if err == nil {
					_, second, writeErr := d.ClaimLaunchReceipt(repo.ID, "feature", "nonce", "head", "generation", "digest", "", false, nil)
					if writeErr != nil || second {
						t.Errorf("receipt claimed twice: %v %v", second, writeErr)
					}
				}
			case "migration":
				opened, writeErr := Open(path)
				elapsed = time.Since(start)
				err = writeErr
				if opened != nil {
					defer opened.Close()
				}
				if err == nil {
					var version int
					if err := opened.sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
						t.Fatalf("migration schema version = %d, want %d: %v", version, schemaVersion, err)
					}
					savedRepo, readErr := opened.GetRepo(repo.ID)
					if readErr != nil || savedRepo == nil || *savedRepo != *repo {
						t.Fatalf("migration changed seeded repo: %+v, %v", savedRepo, readErr)
					}
					savedRun, readErr := opened.GetRun(run.ID)
					if readErr != nil || savedRun == nil || savedRun.RepoID != repo.ID || savedRun.Branch != "feature" || savedRun.HeadSHA != "head" || savedRun.BaseSHA != "base" || savedRun.Status != types.RunPending || savedRun.LaunchNonce == nil || *savedRun.LaunchNonce != "nonce" {
						t.Fatalf("migration changed seeded run: %+v, %v", savedRun, readErr)
					}
					if err := opened.BindPushNotification("admitted", run.ID); err != nil {
						t.Fatalf("upgraded admission binding: %v", err)
					}
					if err := opened.BindPushDiagnostic("diagnostic", run.ID); err != nil {
						t.Fatalf("upgraded diagnostic binding: %v", err)
					}
					admitted, diagnostic, readErr := opened.PushNotificationRuns("admitted")
					if readErr != nil || admitted == nil || admitted.ID != run.ID || diagnostic != nil {
						t.Fatalf("upgraded admission readback: %+v %+v %v", admitted, diagnostic, readErr)
					}
					admitted, diagnostic, readErr = opened.PushNotificationRuns("diagnostic")
					if readErr != nil || admitted != nil || diagnostic == nil || diagnostic.ID != run.ID {
						t.Fatalf("upgraded diagnostic readback: %+v %+v %v", admitted, diagnostic, readErr)
					}
					if err := opened.BindPushNotification("invalid", "missing-run"); err == nil {
						t.Fatal("upgraded notification table lost its run foreign key")
					}
					if err := opened.BindPushDiagnostic("invalid-diagnostic", "missing-run"); err == nil {
						t.Fatal("upgraded notification table lost its diagnostic run foreign key")
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if elapsed < sqlitewrite.BusyTimeout {
				t.Fatalf("did not wait through timeout: %v", elapsed)
			}
			t.Logf("%s succeeded after held exclusive writer: %v", surface, elapsed)
		})
	}
}
