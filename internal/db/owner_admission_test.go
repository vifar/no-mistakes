package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestOwnerAdmissionIsAtomicAndVersionOneUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo("fixture", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the previous released schema stamp without removing any other
	// columns. This upgrade must add the audit table without legacy backfills.
	if _, err := d.sql.Exec("DROP TABLE owner_admission_events; PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(d.sql); err != nil {
		t.Fatal(err)
	}
	event := &types.OwnerAdmissionEvent{Event: "launch_owner_policy_bypassed", OwnerlessAdmitted: true, Warning: "admitted ownerless"}
	run, err := d.InsertRunWithIntent(repo.ID, "accepted", "head", "base", &RunIntent{OwnerAdmission: event}, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetOwnerAdmission(run.ID)
	if err != nil || got == nil || got.RunID != run.ID || got.Branch != "accepted" {
		t.Fatalf("upgrade/audit: %+v %v", got, err)
	}
	if _, err := d.sql.Exec("DROP TABLE owner_admission_events"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertRunWithIntent(repo.ID, "must-rollback", "head", "base", &RunIntent{OwnerAdmission: event}, ""); err == nil {
		t.Fatal("admission silently succeeded without durable event")
	}
	rows, err := d.GetRunsByRepo(repo.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("run escaped audit transaction: %+v %v", rows, err)
	}
}
