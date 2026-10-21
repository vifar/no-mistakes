package db

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestVersionSevenUpgradeKeepsEndingAndAddsRepairOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version7.sqlite")
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
	event := &types.RunEndingEvent{Event: "superseded", Caller: "fixture-owner", AttributionSource: "client", Reason: "new head", SupersedingSHA: "new-head", CreatedAt: 1234}
	if err := d.RecordRunEnding(run.ID, event); err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	description := "repair description"
	if _, err := d.InsertReviewStepRound(step.ID, 1, "auto_fix", nil, &description, "head", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec("ALTER TABLE step_rounds DROP COLUMN fix_applied; PRAGMA user_version=7"); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.EndingEvent, event) {
		t.Fatalf("ending audit changed: %#v, want %#v", got.EndingEvent, event)
	}
	replacement := *event
	replacement.Caller = "later request"
	if err := d.RecordRunEnding(run.ID, &replacement); err != nil {
		t.Fatal(err)
	}
	got, err = d.GetRun(run.ID)
	if err != nil || !reflect.DeepEqual(got.EndingEvent, event) {
		t.Fatalf("ending audit lost first-request ownership: %#v, %v", got, err)
	}
	for i, applied := range []bool{true, false} {
		if _, err := d.InsertReviewStepRound(step.ID, i+2, "auto_fix", nil, &description, "head", 0, &applied); err != nil {
			t.Fatal(err)
		}
	}
	rounds, err := d.GetRoundsByStep(step.ID)
	if err != nil || len(rounds) != 3 || rounds[0].FixApplied != nil || rounds[1].FixApplied == nil || !*rounds[1].FixApplied || rounds[2].FixApplied == nil || *rounds[2].FixApplied {
		t.Fatalf("nullable repair evidence changed: %#v, %v", rounds, err)
	}
	var version int
	if err := d.sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 8 {
		t.Fatalf("upgraded version = %d, %v; want 8", version, err)
	}
}
