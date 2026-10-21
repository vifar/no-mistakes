package db

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

// Version 6 is also allocated by open PR #132, without ending_event.
// Its database must receive this independent feature rather than silently
// treating a same-number schema stamp as proof the audit column exists.
func TestEndingRecordMigratesOldSchema(t *testing.T) {
	for _, version := range []int{0, 5, 6} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old.sqlite")
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
			if !hasColumn(t, d, "runs", "ending_event") {
				t.Fatal("runs has no ending_event column to migrate")
			}
			if _, err = d.sql.Exec(fmt.Sprintf("ALTER TABLE runs DROP COLUMN ending_event; PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			d.Close()
			for i := 0; i < 2; i++ {
				d, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
				got, err := d.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(got)
				var doc map[string]any
				_ = json.Unmarshal(raw, &doc)
				if got.HeadSHA != "head" || doc["EndingEvent"] != nil {
					t.Fatalf("legacy row changed: %s", raw)
				}
				if !hasColumn(t, d, "runs", "ending_event") {
					t.Fatal("migration did not add column")
				}
				d.Close()
			}
		})
	}
}
