package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"modernc.org/sqlite"
)

func TestDocumentWritersRetryClosingIssueContention(t *testing.T) {
	for _, operation := range []string{"BeginDocumentPass", "CompleteDocumentPass", "CarryValidationOverDocOnlyCommits"} {
		for _, exhaust := range []bool{false, true} {
			name := operation + "/release"
			if exhaust {
				name = operation + "/exhaust"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				path := filepath.Join(t.TempDir(), "state.sqlite")
				d, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				repo, err := d.InsertRepo("/contention", "https://example.com/repo.git", "main")
				if err != nil {
					t.Fatal(err)
				}
				const from, to = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				run, err := d.InsertRun(repo.ID, "feature", to, from)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, from); err != nil {
					t.Fatal(err)
				}
				step, err := d.InsertStepResult(run.ID, types.StepTest)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := types.MarshalFindingsJSON(types.Findings{Summary: "validated", TestedHeadSHA: from})
				if err != nil {
					t.Fatal(err)
				}
				if err := d.SetStepFindings(step.ID, raw); err != nil {
					t.Fatal(err)
				}
				if err := d.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
					t.Fatal(err)
				}
				var passID string
				if operation != "BeginDocumentPass" {
					passID, err = d.BeginDocumentPass(run.ID, from)
					if err != nil {
						t.Fatal(err)
					}
				}
				if operation == "CarryValidationOverDocOnlyCommits" {
					if err := d.CompleteDocumentPass(passID, to, 1, raw); err != nil {
						t.Fatal(err)
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
				const refs = "example/repo#92"
				if _, err := tx.Exec(`UPDATE runs SET closing_issue_refs=? WHERE id=?`, refs, run.ID); err != nil {
					t.Fatal(err)
				}
				released := make(chan error, 1)
				if !exhaust {
					go func() {
						time.Sleep(6 * time.Second)
						released <- tx.Commit()
					}()
				}
				start := time.Now()
				var carried bool
				switch operation {
				case "BeginDocumentPass":
					passID, err = d.BeginDocumentPass(run.ID, from)
				case "CompleteDocumentPass":
					err = d.CompleteDocumentPass(passID, to, 1, raw)
				case "CarryValidationOverDocOnlyCommits":
					carried, err = d.CarryValidationOverDocOnlyCommits(run.ID, from, to)
				}
				elapsed := time.Since(start)
				if exhaust {
					if releaseErr := tx.Commit(); releaseErr != nil {
						t.Fatal(releaseErr)
					}
					var contention *sqlitewrite.ContentionError
					var cause *sqlite.Error
					if !errors.As(err, &contention) || !errors.As(err, &cause) {
						t.Fatalf("%s lost typed contention or SQLite cause after %v: %T %v", operation, elapsed, err, err)
					}
					if contention.Operation != operation || contention.Attempts < 2 || elapsed < sqlitewrite.Budget || elapsed > sqlitewrite.Budget+sqlitewrite.BusyTimeout+5*time.Second {
						t.Fatalf("unbounded or incomplete contention: %v after %v", contention, elapsed)
					}
				} else {
					if releaseErr := <-released; releaseErr != nil {
						t.Fatal(releaseErr)
					}
					if err != nil {
						t.Fatalf("%s failed after %v instead of retrying: %v", operation, elapsed, err)
					}
				}
				stored, err := d.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(stored.ClosingIssueRefs) != 1 || stored.ClosingIssueRefs[0] != refs {
					t.Fatalf("lost concurrent writer: %+v", stored)
				}
				passes, err := d.GetDocumentPasses(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if operation == "BeginDocumentPass" && exhaust {
					if len(passes) != 0 {
						t.Fatalf("failed begin recorded a pass: %+v", passes)
					}
				} else {
					if len(passes) != 1 || passes[0].ID != passID || passes[0].FromHeadSHA != from {
						t.Fatalf("lost or duplicated pass: %+v", passes)
					}
					completed := operation == "CarryValidationOverDocOnlyCommits" || operation == "CompleteDocumentPass" && !exhaust
					if completed {
						if passes[0].ToHeadSHA == nil || *passes[0].ToHeadSHA != to || passes[0].CommitCount != 1 || passes[0].FindingsJSON == nil || *passes[0].FindingsJSON != raw {
							t.Fatalf("lost Document budget/result: %+v", passes[0])
						}
					} else if passes[0].ToHeadSHA != nil || passes[0].CommitCount != 0 {
						t.Fatalf("failed completion changed pass: %+v", passes[0])
					}
					wantCarry := operation == "CarryValidationOverDocOnlyCommits" && !exhaust
					if carried != wantCarry || (passes[0].CarriedReviewFrom != nil) != wantCarry {
						t.Fatalf("incorrect carry: result=%v pass=%+v", carried, passes[0])
					}
				}
				wantHead := from
				if operation == "CarryValidationOverDocOnlyCommits" && !exhaust {
					wantHead = to
				}
				if stored.ReviewApprovedHeadSHA == nil || *stored.ReviewApprovedHeadSHA != wantHead {
					t.Fatalf("incorrect review credit: %+v", stored)
				}
				tested, err := d.GetStepResult(step.ID)
				if err != nil {
					t.Fatal(err)
				}
				findings, err := types.ParseFindingsJSON(*tested.FindingsJSON)
				if err != nil || findings.TestedHeadSHA != wantHead {
					t.Fatalf("half-applied Test credit: %+v %v", findings, err)
				}
				t.Logf("%s exhaust=%v elapsed=%v; closing-issue write preserved", operation, exhaust, elapsed)
			})
		}
	}
}
