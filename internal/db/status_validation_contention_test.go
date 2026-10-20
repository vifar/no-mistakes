package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestStatusValidationWritersUseSharedContentionPolicy(t *testing.T) {
	for _, operation := range []string{"RecordStatusValidationStep", "InvalidateStatusValidation", "RevokeStatusCertificate", "TerminalizeCertifiedWatcher", "PrepareControlledShutdown", "RecordIneligibleTerminal", "SupersedeUncommittedCancellations", "ReleaseShutdownForRecovery", "ReconcileRecoveredRun"} {
		for _, exhaust := range []bool{false, true} {
			name := operation + "/release"
			if exhaust {
				name = operation + "/exhaust"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				d, run, publication := certifiedStatusFixture(t)
				steps, err := d.GetStepsByRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if operation == "ReleaseShutdownForRecovery" || operation == "ReconcileRecoveredRun" {
					if err := d.PrepareControlledShutdown([]string{run.ID}, "old", "lifecycle", "planned"); err != nil {
						t.Fatal(err)
					}
				}
				var path string
				if err := d.sql.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
					t.Fatal(err)
				}
				holder, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer holder.Close()
				holder.SetMaxOpenConns(1)
				if _, err := holder.Exec("BEGIN IMMEDIATE"); err != nil {
					t.Fatal(err)
				}
				released := make(chan error, 1)
				if !exhaust {
					go func() {
						time.Sleep(sqlitewrite.BusyTimeout + time.Second)
						_, err := holder.Exec("ROLLBACK")
						released <- err
					}()
				}
				var obsolete bool
				start := time.Now()
				switch operation {
				case "RecordStatusValidationStep":
					err = d.RecordStatusValidationStep(steps[0].ID, run.ID, "head", "config", 0)
				case "InvalidateStatusValidation":
					err = d.InvalidateStatusValidation(run.ID)
				case "RevokeStatusCertificate":
					err = d.RevokeStatusCertificate(context.Background(), run.ID, publication)
				case "TerminalizeCertifiedWatcher":
					err = d.TerminalizeCertifiedWatcher(run.ID, types.RunCancelReasonAbortedByUser, "head", "config", types.RunCancelled)
				case "PrepareControlledShutdown":
					err = d.PrepareControlledShutdown([]string{run.ID}, "runtime", "lifecycle", "planned")
				case "RecordIneligibleTerminal":
					err = d.RecordIneligibleTerminal(run.ID, "independent failure", types.RunFailed, nil)
				case "SupersedeUncommittedCancellations":
					err = d.SupersedeUncommittedCancellations([]string{run.ID}, "runtime")
				case "ReleaseShutdownForRecovery":
					err = d.ReleaseShutdownForRecovery(run.ID)
				case "ReconcileRecoveredRun":
					obsolete, err = d.ReconcileRecoveredRun(run.ID, "runtime")
				}
				elapsed := time.Since(start)
				if exhaust {
					_, releaseErr := holder.Exec("ROLLBACK")
					if releaseErr != nil {
						t.Fatal(releaseErr)
					}
					var contention *sqlitewrite.ContentionError
					if !errors.As(err, &contention) || contention.Operation != operation || contention.Attempts < 2 {
						t.Fatalf("untyped or premature exhaustion: %T %v", err, err)
					}
					if elapsed < sqlitewrite.Budget {
						t.Fatalf("shared retry budget ended early: %v", elapsed)
					}
					if obsolete {
						t.Fatal("rolled-back reconciliation leaked its result")
					}
				} else {
					if releaseErr := <-released; releaseErr != nil {
						t.Fatal(releaseErr)
					}
					if err != nil {
						t.Fatalf("write did not recover beyond busy timeout: %v", err)
					}
					if elapsed < sqlitewrite.BusyTimeout {
						t.Fatalf("contention was not exercised: %v", elapsed)
					}
				}
				stored, err := d.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				saved, err := d.GetCommitStatusPublication(context.Background(), run.ID, publication)
				if err != nil {
					t.Fatal(err)
				}
				epoch, err := d.StatusValidationEpoch(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				var shutdownRecords int
				if err := d.sql.QueryRow(`SELECT count(*) FROM controlled_shutdown_runs WHERE run_id=?`, run.ID).Scan(&shutdownRecords); err != nil {
					t.Fatal(err)
				}
				if exhaust {
					if stored.Status != types.RunRunning || stored.Error != nil || saved.StrictCertificate == nil || *saved.StrictCertificate != *publication.StrictCertificate || epoch != 0 {
						t.Fatalf("exhausted transaction changed durable state: run=%+v publication=%+v epoch=%d", stored, saved, epoch)
					}
					expected := 0
					if operation == "ReleaseShutdownForRecovery" || operation == "ReconcileRecoveredRun" {
						expected = 1
					}
					if shutdownRecords != expected {
						t.Fatal("exhaustion changed shutdown records")
					}
					return
				}
				invalidates := operation == "InvalidateStatusValidation" || operation == "SupersedeUncommittedCancellations" || operation == "ReleaseShutdownForRecovery" || operation == "ReconcileRecoveredRun"
				if invalidates && (epoch != 1 || saved.StrictCertificate != nil) {
					t.Fatalf("retry did not atomically invalidate once: epoch=%d publication=%+v", epoch, saved)
				}
				switch operation {
				case "RecordStatusValidationStep":
					certificate, err := d.BuildStatusCertificate(run.ID, "head", "config")
					if err != nil || certificate == nil || *certificate != *publication.StrictCertificate {
						t.Fatalf("proof write lost certification: %v", err)
					}
				case "RevokeStatusCertificate":
					if saved.StrictCertificate != nil || saved.PreserveSuccess != nil {
						t.Fatal("revocation did not settle")
					}
				case "TerminalizeCertifiedWatcher":
					if stored.Status != types.RunCancelled || saved.PreserveSuccess == nil || !*saved.PreserveSuccess {
						t.Fatalf("terminal retry lost certified success: %+v %+v", stored, saved)
					}
				case "PrepareControlledShutdown", "SupersedeUncommittedCancellations":
					if shutdownRecords != 1 {
						t.Fatal("shutdown record missing")
					}
				case "RecordIneligibleTerminal":
					if stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != "independent failure" || saved.StrictCertificate != nil {
						t.Fatalf("terminal truth/revocation lost: %+v %+v", stored, saved)
					}
				case "ReleaseShutdownForRecovery":
					if shutdownRecords != 0 || stored.Status != types.RunRunning {
						t.Fatal("release did not leave a recoverable run")
					}
				case "ReconcileRecoveredRun":
					if !obsolete || shutdownRecords != 0 || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != "daemon crashed during execution" {
						t.Fatalf("reconciliation lost its committed result: %+v", stored)
					}
				}
			})
		}
	}
}
