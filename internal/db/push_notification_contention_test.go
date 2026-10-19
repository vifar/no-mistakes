package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"modernc.org/sqlite"
)

func TestPushBindingsContention(t *testing.T) {
	for _, operation := range []string{"BindPushNotification", "BindPushDiagnostic"} {
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
				repo, err := d.InsertRepo("/binding", "https://example.com/repo.git", "main")
				if err != nil {
					t.Fatal(err)
				}
				run, err := d.InsertRun(repo.ID, "feature", "head", "base")
				if err != nil {
					t.Fatal(err)
				}
				other, err := d.InsertRun(repo.ID, "other", "other-head", "base")
				if err != nil {
					t.Fatal(err)
				}
				bind := d.BindPushNotification
				if operation == "BindPushDiagnostic" {
					bind = d.BindPushDiagnostic
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
				if _, err := tx.Exec("UPDATE runs SET closing_issue_refs=? WHERE id=?", "example/repo#92", run.ID); err != nil {
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
				err = bind("retained", run.ID)
				elapsed := time.Since(start)
				if exhaust {
					if releaseErr := tx.Commit(); releaseErr != nil {
						t.Fatal(releaseErr)
					}
					var contention *sqlitewrite.ContentionError
					var cause *sqlite.Error
					if !errors.As(err, &contention) || !errors.As(err, &cause) {
						t.Fatalf("lost typed contention or SQLite cause after %v: %T %v", elapsed, err, err)
					}
					if contention.Operation != operation || contention.Attempts < 2 || (cause.Code()&0xff != 5 && cause.Code()&0xff != 6) {
						t.Fatalf("incorrect exhaustion diagnostic: %+v, SQLite code %d", contention, cause.Code())
					}
					if elapsed < sqlitewrite.Budget || elapsed > sqlitewrite.Budget+sqlitewrite.BusyTimeout+5*time.Second {
						t.Fatalf("retry budget not respected: %v", elapsed)
					}
					admitted, diagnostic, readErr := d.PushNotificationRuns("retained")
					if readErr != nil || admitted != nil || diagnostic != nil {
						t.Fatalf("exhaustion persisted a binding: %+v %+v %v", admitted, diagnostic, readErr)
					}
				} else {
					if releaseErr := <-released; releaseErr != nil {
						t.Fatal(releaseErr)
					}
					if err != nil {
						t.Fatalf("binding failed before competing writer released: %v", err)
					}
					if elapsed < sqlitewrite.BusyTimeout {
						t.Fatalf("writer did not wait past busy timeout: %v", elapsed)
					}
				}
				if err := bind("retained", run.ID); err != nil {
					t.Fatalf("binding after release or duplicate delivery failed: %v", err)
				}
				if err := bind("retained", run.ID); err != nil {
					t.Fatalf("duplicate delivery was not idempotent: %v", err)
				}
				if err := bind("retained", other.ID); err == nil || sqlitewrite.IsContention(err) {
					t.Fatalf("conflicting binding must fail without contention classification: %v", err)
				}
				admitted, diagnostic, err := d.PushNotificationRuns("retained")
				if err != nil {
					t.Fatal(err)
				}
				bound, absent := admitted, diagnostic
				if operation == "BindPushDiagnostic" {
					bound, absent = diagnostic, admitted
				}
				if bound == nil || bound.ID != run.ID || absent != nil {
					t.Fatalf("binding changed or leaked to sibling: admitted=%+v diagnostic=%+v", admitted, diagnostic)
				}
				saved, err := d.GetRun(run.ID)
				if err != nil || saved == nil || len(saved.ClosingIssueRefs) != 1 || saved.ClosingIssueRefs[0] != "example/repo#92" {
					t.Fatalf("competing closing-issue write lost: %+v %v", saved, err)
				}
				t.Logf("%s exhaust=%v elapsed=%v", operation, exhaust, elapsed)
			})
		}
	}
}
