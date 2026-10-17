package db

import (
	"path/filepath"
	"testing"
)

func TestRunLaunchOwnerSurvivesIntentUpdate(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "owner.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo("/tmp/owner", "https://example.com/owner.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	r, err := d.InsertRunWithIntent(repo.ID, "feature", "head", "base", &RunIntent{Owner: "firstmate", SessionID: "firstmate", Source: RunIntentSourceAgent}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunIntent(r.ID, RunIntent{Summary: "inferred goal", Source: "codex", SessionID: "unrelated-transcript"}); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Owner() != "firstmate" {
		t.Fatalf("inference erased launch owner: %q", got.Owner())
	}
	legacy, err := d.InsertRunWithIntent(repo.ID, "legacy", "head", "base", &RunIntent{Summary: "goal", Source: RunIntentSourceAgent, SessionID: "historical-provenance"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Owner() != "" {
		t.Fatalf("invented legacy owner from provenance: %q", legacy.Owner())
	}
}
