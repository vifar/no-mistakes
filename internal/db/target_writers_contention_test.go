package db

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"modernc.org/sqlite"
)

func TestTargetWritersContention(t *testing.T) {
	for _, operation := range []string{"RecordRetainedPRTarget", "PinTargetRewindPRObservation", "RecordTargetRewindBoundary"} {
		for _, exhaust := range []bool{false, true} {
			mode := "release"
			if exhaust {
				mode = "exhaust"
			}
			t.Run(operation+"/"+mode, func(t *testing.T) {
				t.Parallel()
				d, id, candidate, request := targetRewindDBFixture(t)
				target := strings.Repeat("f", 40)
				prURL := "https://example.com/acme/repo/pull/42"
				outcome := `{"Findings":"boundary evidence"}`
				var write func() error
				var rejectStale func() error
				var check func(bool)
				switch operation {
				case "RecordRetainedPRTarget":
					for _, err := range []error{
						d.UpdateRunReviewApprovedHeadSHA(id, candidate.LocalHeadSHA),
						d.UpdateRunPushBinding(id, PushBinding{HeadSHA: candidate.LocalHeadSHA}),
						d.SetRunCIReady(id, true),
					} {
						if err != nil {
							t.Fatal(err)
						}
					}
					receipt, err := d.GetRunPRContext(id)
					if err != nil {
						t.Fatal(err)
					}
					write = func() error { return d.RecordRetainedPRTarget(id, receipt, "", target) }
					rejectStale = func() error { return d.RecordRetainedPRTarget(id, receipt, "", strings.Repeat("1", 40)) }
					check = func(present bool) {
						got, err := d.GetRetainedPRTarget(id, receipt)
						want := ""
						if present {
							want = target
						}
						if err != nil || got != want {
							t.Fatalf("retained target=%q, want %q: %v", got, want, err)
						}
						current, err := d.GetRunPRContext(id)
						if err != nil || current == nil || *current != *receipt {
							t.Fatalf("comparison changed: %+v %v", current, err)
						}
					}
				case "PinTargetRewindPRObservation":
					write = func() error { return d.PinTargetRewindPRObservation(id, prURL, candidate.LocalHeadSHA) }
					rejectStale = func() error { return d.PinTargetRewindPRObservation(id, prURL+"0", candidate.LocalHeadSHA) }
					check = func(present bool) {
						got, err := d.GetTargetRewindPRObservation(id)
						if err != nil {
							t.Fatal(err)
						}
						if !present {
							if got != nil {
								t.Fatalf("unexpected observation: %+v", got)
							}
							return
						}
						if got == nil || got.PRURL != prURL || got.HeadSHA != candidate.LocalHeadSHA {
							t.Fatalf("incorrect observation: %+v", got)
						}
					}
				case "RecordTargetRewindBoundary":
					if _, err := d.RewindRunPRContext(id, candidate, request); err != nil {
						t.Fatal(err)
					}
					write = func() error { return d.RecordTargetRewindBoundary(id, 1, "test", outcome) }
					rejectStale = func() error { return d.RecordTargetRewindBoundary(id, 99, "review", "wrong generation") }
					check = func(present bool) {
						got, err := d.GetTargetRewindHistory(id)
						if err != nil || len(got) != 1 {
							t.Fatalf("history=%+v: %v", got, err)
						}
						if !present {
							if got[0].BoundaryStep != nil || got[0].BoundaryJSON != nil {
								t.Fatalf("unexpected boundary: %+v", got[0])
							}
							return
						}
						if got[0].BoundaryStep == nil || *got[0].BoundaryStep != "test" || got[0].BoundaryJSON == nil || *got[0].BoundaryJSON != outcome {
							t.Fatalf("incorrect boundary: %+v", got[0])
						}
					}
				}
				var sequence int
				var name, path string
				if err := d.sql.QueryRow("PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
					t.Fatal(err)
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
				if _, err := tx.Exec("UPDATE runs SET closing_issue_refs=? WHERE id=?", "example/repo#92", id); err != nil {
					t.Fatal(err)
				}
				released := make(chan error, 1)
				if !exhaust {
					go func() {
						time.Sleep(sqlitewrite.BusyTimeout + time.Second)
						released <- tx.Commit()
					}()
				}
				start := time.Now()
				err = write()
				elapsed := time.Since(start)
				if exhaust {
					if releaseErr := tx.Commit(); releaseErr != nil {
						t.Fatal(releaseErr)
					}
					var contention *sqlitewrite.ContentionError
					var cause *sqlite.Error
					if !errors.As(err, &contention) || !errors.As(err, &cause) {
						t.Fatalf("lost typed contention/SQLite cause after %v: %T %v", elapsed, err, err)
					}
					if contention.Operation != operation || contention.Attempts < 2 || (cause.Code()&0xff != 5 && cause.Code()&0xff != 6) {
						t.Fatalf("incorrect exhaustion: %+v, code %d", contention, cause.Code())
					}
					if elapsed < sqlitewrite.Budget || elapsed > sqlitewrite.Budget+sqlitewrite.BusyTimeout+5*time.Second {
						t.Fatalf("retry budget not respected: %v", elapsed)
					}
					check(false)
					if err := write(); err != nil {
						t.Fatalf("write after release: %v", err)
					}
				} else {
					if releaseErr := <-released; releaseErr != nil {
						t.Fatal(releaseErr)
					}
					if err != nil {
						t.Fatalf("write failed before release after %v: %v", elapsed, err)
					}
					if elapsed < sqlitewrite.BusyTimeout {
						t.Fatalf("write did not wait past busy timeout: %v", elapsed)
					}
				}
				check(true)
				if err := rejectStale(); err == nil || sqlitewrite.IsContention(err) {
					t.Fatalf("stale write must fail without contention classification: %v", err)
				}
				check(true)
				if operation != "RecordRetainedPRTarget" {
					if err := write(); err != nil {
						t.Fatalf("idempotent repeat failed: %v", err)
					}
					check(true)
				}
				run, err := d.GetRun(id)
				if err != nil || run == nil || len(run.ClosingIssueRefs) != 1 || run.ClosingIssueRefs[0] != "example/repo#92" {
					t.Fatalf("competing write lost: %+v %v", run, err)
				}
				t.Logf("%s %s elapsed=%v", operation, mode, elapsed)
			})
		}
	}
}
