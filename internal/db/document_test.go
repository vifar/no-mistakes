package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDocumentPassSurvivesResetAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "document.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	const from, to = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	run, err := d.InsertRun(repo.ID, "feature", from, from)
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.BeginDocumentPass(run.ID, from)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.BeginDocumentPass(run.ID, from); err == nil {
		t.Fatal("concurrent pending pass accepted")
	}
	if err := d.CompleteDocumentPass(id, to, 2, `{"findings":[],"summary":"updated docs"}`); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteDocumentPass(id, to, 0, `{}`); err == nil {
		t.Fatal("completed budget overwritten")
	}
	if err := d.ResetStepsFrom(run.ID, types.StepReview.Order()); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	passes, err := d.GetDocumentPasses(run.ID)
	if err != nil || len(passes) != 1 || passes[0].CommitCount != 2 || passes[0].ToHeadSHA == nil || *passes[0].ToHeadSHA != to {
		t.Fatalf("lost Document budget after reset/reopen: %+v, %v", passes, err)
	}
}
