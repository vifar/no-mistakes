package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPushNotificationVersionTwoUpgradeAndAtomicBinding(t *testing.T) {
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
	if _, err := d.sql.Exec("DROP TABLE push_notifications; PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(d.sql); err != nil {
		t.Fatal(err)
	}
	event := &types.OwnerAdmissionEvent{Event: "launch_owner_policy_bypassed", OwnerlessAdmitted: true}
	creation := RunCreation{NotificationID: "retained", ClosingIssueRefs: []string{"67"}, PRURL: "https://example.com/pr/66"}
	run, err := d.InsertRunWithAllOptions(repo.ID, "fresh", "head", "base", &RunIntent{OwnerAdmission: event}, "", "", "", "", false, false, "", nil, nil, nil, creation)
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := d.PushNotificationRuns("retained")
	if err != nil || bound == nil || bound.ID != run.ID || len(bound.ClosingIssueRefs) != 1 || bound.PRURL == nil {
		t.Fatalf("missing creation metadata: %+v %v", bound, err)
	}
	if _, err := d.sql.Exec("CREATE TRIGGER reject_binding BEFORE INSERT ON push_notifications BEGIN SELECT RAISE(ABORT,'binding failure'); END"); err != nil {
		t.Fatal(err)
	}
	creation.NotificationID = "second"
	if _, err := d.InsertRunWithAllOptions(repo.ID, "fresh", "head2", "base", &RunIntent{OwnerAdmission: event}, "", "", "", "", false, false, "", nil, nil, nil, creation); err == nil {
		t.Fatal("binding failure silently admitted run")
	}
	rows, err := d.GetRunsByRepo(repo.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("binding failure escaped transaction: %+v %v", rows, err)
	}
	var n int
	if err := d.sql.QueryRow("SELECT count(*) FROM owner_admission_events").Scan(&n); err != nil || n != 1 {
		t.Fatalf("event escaped transaction: %d %v", n, err)
	}
}
