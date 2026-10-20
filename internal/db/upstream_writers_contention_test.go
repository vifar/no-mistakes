package db

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"modernc.org/sqlite"
)

func TestUpstreamWritersContention(t *testing.T) {
	for _, operation := range []string{"CompleteCIStep", "InsertCarriedStepResult", "InsertRunRetry", "DeleteRun", "SetRunQuotaRouting", "SetStepResponseOrigin", "SetStepRoundUserDecision", "SetStepRoundDeclined"} {
		for _, exhaust := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/exhaust=%v", operation, exhaust), func(t *testing.T) {
				t.Parallel()
				path := filepath.Join(t.TempDir(), "state.sqlite")
				d, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				repo, err := d.InsertRepo("fixture", "https://example.com/repo", "main")
				if err != nil {
					t.Fatal(err)
				}
				source, err := d.InsertRun(repo.ID, "source", "head", "base")
				if err != nil {
					t.Fatal(err)
				}
				run, err := d.InsertRun(repo.ID, "retry", "head", "base")
				if err != nil {
					t.Fatal(err)
				}
				step, err := d.InsertStepResult(source.ID, types.StepReview)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
					t.Fatal(err)
				}
				step, err = d.GetStepResult(step.ID)
				if err != nil {
					t.Fatal(err)
				}
				round, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 1)
				if err != nil {
					t.Fatal(err)
				}
				ci, err := d.InsertStepResult(run.ID, types.StepCI)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.StartStep(ci.ID); err != nil {
					t.Fatal(err)
				}
				write := func() error {
					switch operation {
					case "CompleteCIStep":
						return d.CompleteCIStep(ci.ID, run.ID, true, 0, 7, "ci.log")
					case "InsertCarriedStepResult":
						_, err := d.InsertCarriedStepResult(run.ID, step)
						return err
					case "InsertRunRetry":
						return d.InsertRunRetry(RunRetry{RunID: run.ID, SourceRunID: source.ID, StepName: types.StepTest, CheckpointHeadSHA: "head"})
					case "DeleteRun":
						return d.DeleteRun(run.ID)
					case "SetRunQuotaRouting":
						return d.SetRunQuotaRouting(run.ID, "quota evidence")
					case "SetStepResponseOrigin":
						return d.SetStepResponseOrigin(step.ID, types.ResponseOriginAutoYes)
					case "SetStepRoundUserDecision":
						selected := `["r1"]`
						return d.SetStepRoundUserDecision(round.ID, &selected, RoundSelectionSourceUser, nil, types.ResponseOriginInteractive)
					default:
						return d.SetStepRoundDeclined(round.ID, types.ResponseOriginAutoYes)
					}
				}
				check := func(present bool) {
					t.Helper()
					got, err := d.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if operation == "DeleteRun" {
						if (got == nil) != present {
							t.Fatalf("deleted=%v want=%v", got == nil, present)
						}
						return
					}
					if got == nil {
						t.Fatal("run disappeared")
					}
					var applied bool
					switch operation {
					case "CompleteCIStep":
						stored, err := d.GetStepResult(ci.ID)
						if err != nil {
							t.Fatal(err)
						}
						applied = stored.Status == types.StepStatusCompleted
						if (got.CIReadyAt != nil) != applied || (got.CIReleasedAt != nil) != applied || got.CIReadyNoCI != applied {
							t.Fatalf("partial CI completion: %+v %+v", got, stored)
						}
					case "InsertCarriedStepResult":
						rows, err := d.GetStepsByRun(run.ID)
						if err != nil {
							t.Fatal(err)
						}
						applied = len(rows) == 2
						if applied && (rows[0].CarriedFrom == nil || *rows[0].CarriedFrom != step.ID) {
							t.Fatalf("lost carried origin: %+v", rows)
						}
					case "InsertRunRetry":
						row, err := d.GetRunRetry(run.ID)
						if err != nil {
							t.Fatal(err)
						}
						applied = row != nil
						if applied && row.SourceRunID != source.ID {
							t.Fatalf("wrong source: %+v", row)
						}
					case "SetRunQuotaRouting":
						applied = got.QuotaRouting == "quota evidence"
					case "SetStepResponseOrigin":
						row, err := d.GetStepResult(step.ID)
						if err != nil {
							t.Fatal(err)
						}
						applied = row.ResponseOrigin == types.ResponseOriginAutoYes
					default:
						rows, err := d.GetRoundsByStep(step.ID)
						if err != nil || len(rows) != 1 {
							t.Fatalf("rounds=%+v %v", rows, err)
						}
						applied = rows[0].SelectedFindingIDs != nil
						if applied && rows[0].ResponseOrigin == types.ResponseOriginUnknown {
							t.Fatal("lost response origin")
						}
					}
					if applied != present {
						t.Fatalf("%s applied=%v want=%v", operation, applied, present)
					}
				}
				holder, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer holder.Close()
				tx, err := holder.Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, err := tx.Exec("UPDATE runs SET intent='competing write' WHERE id=?", source.ID); err != nil {
					t.Fatal(err)
				}
				released := make(chan error, 1)
				if !exhaust {
					go func() { time.Sleep(sqlitewrite.BusyTimeout + time.Second); released <- tx.Commit() }()
				}
				started := time.Now()
				err = write()
				elapsed := time.Since(started)
				if exhaust {
					if releaseErr := tx.Commit(); releaseErr != nil {
						t.Fatal(releaseErr)
					}
					var contention *sqlitewrite.ContentionError
					var cause *sqlite.Error
					if !errors.As(err, &contention) || !errors.As(err, &cause) || contention.Operation != operation || contention.Attempts < 2 {
						t.Fatalf("lost contention classification: %v", err)
					}
					if elapsed < sqlitewrite.Budget || elapsed > sqlitewrite.Budget+sqlitewrite.BusyTimeout+5*time.Second {
						t.Fatalf("unbounded exhaustion: %v", elapsed)
					}
					check(false)
					if err := write(); err != nil {
						t.Fatal(err)
					}
				} else {
					if releaseErr := <-released; releaseErr != nil {
						t.Fatal(releaseErr)
					}
					if err != nil || elapsed < sqlitewrite.BusyTimeout {
						t.Fatalf("write before release: %v after %v", err, elapsed)
					}
				}
				check(true)
			})
		}
	}
}

func TestCompleteCIStepRollsBackOnReadinessFailure(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("fixture", "https://example.com/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`CREATE TRIGGER reject_release BEFORE UPDATE OF ci_released_at ON runs BEGIN SELECT RAISE(ABORT,'rejected release'); END`); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteCIStep(step.ID, run.ID, false, 0, 7, "ci.log"); err == nil || sqlitewrite.IsContention(err) {
		t.Fatalf("wrong rejection: %v", err)
	}
	stored, err := d.GetStepResult(step.ID)
	if err != nil || stored.Status != types.StepStatusRunning || stored.CompletedAt != nil {
		t.Fatalf("partial step completion: %+v %v", stored, err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil || got.CIReadyAt != nil || got.CIReleasedAt != nil {
		t.Fatalf("partial readiness: %+v %v", got, err)
	}
}
