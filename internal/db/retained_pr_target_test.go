package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRetainedPRTargetSurvivesReopenAndRejectsStaleBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	id := testPRContextRun(t, d)
	candidate := testPRContextCandidate()
	candidate.PRURL = "https://example.com/acme/repo/pull/42"
	candidate.ForgeHeadSHA = candidate.LocalHeadSHA
	if _, err := d.BindRunPRContext(id, candidate, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		d.UpdateRunHeadSHA(id, candidate.LocalHeadSHA),
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
	target := strings.Repeat("e", 40)
	if err := d.RecordRetainedPRTarget(id, receipt, "", target); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordRetainedPRTarget(id, receipt, "", strings.Repeat("1", 40)); err == nil {
		t.Fatal("stale target observation overwrote a verified target")
	}
	if got, err := d.GetRetainedPRTarget(id, receipt); err != nil || got != target {
		t.Fatalf("stale writer changed target: %q %v", got, err)
	}
	nextTarget := strings.Repeat("3", 40)
	if err := d.RecordRetainedPRTarget(id, receipt, target, nextTarget); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordRetainedPRTarget(id, receipt, target, strings.Repeat("4", 40)); err == nil {
		t.Fatal("stale writer replaced the latest retained target")
	}
	target = nextTarget
	if err := d.SetRunCIReady(id, false); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := d.GetRetainedPRTarget(id, receipt); err != nil || got != target {
		t.Fatalf("lost verified target after readiness loss/reopen: %q %v", got, err)
	}
	if after, err := d.GetRunPRContext(id); err != nil || *after != *receipt {
		t.Fatalf("original receipt moved: %+v %v", after, err)
	}
	if err := d.UpdateRunReviewApprovedHeadSHA(id, strings.Repeat("f", 40)); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordRetainedPRTarget(id, receipt, target, strings.Repeat("1", 40)); err == nil {
		t.Fatal("changed review authority accepted an observation")
	}
	if got, err := d.GetRetainedPRTarget(id, receipt); err != nil || got != target {
		t.Fatalf("refused write changed target: %q %v", got, err)
	}
	candidate.TargetSHA = strings.Repeat("2", 40)
	if _, err := d.BindRunPRContext(id, candidate, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordRetainedPRTarget(id, receipt, "", target); err == nil {
		t.Fatal("stale comparison generation accepted an observation")
	}
	fresh, err := d.GetRunPRContext(id)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := d.GetRetainedPRTarget(id, fresh); err != nil || got != "" {
		t.Fatalf("new comparison inherited verified target: %q %v", got, err)
	}
}
