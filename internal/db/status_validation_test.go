package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func certifiedStatusFixture(t *testing.T) (*DB, *Run, CommitStatusPublication) {
	t.Helper()
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, "head"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPublication(run.ID, PushBinding{HeadSHA: "head", TargetKind: "upstream", TargetFingerprint: "fingerprint", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
		step, err := d.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if name == types.StepTest {
			if err := d.SetStepFindings(step.ID, `{"findings":[],"tested_head_sha":"head"}`); err != nil {
				t.Fatal(err)
			}
		}
		if err := d.CompleteStep(step.ID, 0, 1, ""); err != nil {
			t.Fatal(err)
		}
		if err := d.RecordStatusValidationStep(step.ID, run.ID, "head", "config", 0); err != nil {
			t.Fatal(err)
		}
	}
	certificate, err := d.BuildStatusCertificate(run.ID, "head", "config")
	if err != nil || certificate == nil {
		t.Fatalf("certificate = %v,%v", certificate, err)
	}
	p := CommitStatusPublication{SHA: "head", Host: "example.com", Repo: "test/repo", Validated: true, LastState: "success", StrictCertificate: certificate}
	if err := d.PutCommitStatusPublication(run.ID, p); err != nil {
		t.Fatal(err)
	}
	return d, run, p
}

func TestCommitStatus_RepairEntryInvalidatesBeforeTools(t *testing.T) {
	d, run, p := certifiedStatusFixture(t)
	ci, err := d.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStepFixRound(ci.ID, 1); err != nil {
		t.Fatal(err)
	}
	epoch, err := d.StatusValidationEpoch(run.ID)
	if err != nil || epoch == 0 {
		t.Fatalf("repair did not advance epoch: %d,%v", epoch, err)
	}
	got, err := d.GetCommitStatusPublication(context.Background(), run.ID, p)
	if err != nil || got.StrictCertificate != nil {
		t.Fatalf("repair retained certificate: %+v,%v", got, err)
	}
}

func TestCommitStatus_ResetInvalidatesEvenSameHead(t *testing.T) {
	d, run, p := certifiedStatusFixture(t)
	if err := d.ResetStepsFrom(run.ID, types.StepReview.Order()); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetCommitStatusPublication(context.Background(), run.ID, p)
	if err != nil || got.StrictCertificate != nil {
		t.Fatalf("reset retained certificate: %+v,%v", got, err)
	}
}

func TestCommitStatus_CommittedDecisionSurvivesReopen(t *testing.T) {
	d, run, p := certifiedStatusFixture(t)
	if err := d.TerminalizeCertifiedWatcher(run.ID, types.RunCancelReasonAbortedByUser, "head", "config", types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetCommitStatusPublication(context.Background(), run.ID, p)
	if err != nil || got.PreserveSuccess == nil || !*got.PreserveSuccess {
		t.Fatalf("terminal proof = %+v,%v", got, err)
	}
	ids, err := d.CommitStatusRecoveryRunIDs(context.Background())
	if err != nil || len(ids) != 0 {
		t.Fatalf("preserved decision needs POST: %v,%v", ids, err)
	}
	var path string
	if err := d.sql.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	ids, err = reopened.CommitStatusRecoveryRunIDs(context.Background())
	if err != nil || len(ids) != 0 {
		t.Fatalf("recovery lost decision: %v,%v", ids, err)
	}
}

func TestCommitStatus_MissingOrChangedProofCannotPreserve(t *testing.T) {
	for _, change := range []string{"config", "head", "repair", "missing", "owner"} {
		t.Run(change, func(t *testing.T) {
			d, run, p := certifiedStatusFixture(t)
			head, digest := "head", "config"
			switch change {
			case "config":
				digest = "changed"
			case "head":
				head = "unpublished"
			case "repair":
				if err := d.InvalidateStatusValidation(run.ID); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := d.RevokeStatusCertificate(context.Background(), run.ID, p); err != nil {
					t.Fatal(err)
				}
			case "owner":
				newer, err := d.InsertRun(run.RepoID, "feature", "head", "base")
				if err != nil {
					t.Fatal(err)
				}
				if err := d.PutCommitStatusPublication(newer.ID, CommitStatusPublication{SHA: p.SHA, Host: p.Host, Repo: p.Repo, LastState: "pending"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.TerminalizeCertifiedWatcher(run.ID, types.RunCancelReasonAbortedByUser, head, digest, types.RunCancelled); err != nil {
				t.Fatal(err)
			}
			got, err := d.GetCommitStatusPublication(context.Background(), run.ID, p)
			if err != nil {
				t.Fatal(err)
			}
			if got != nil && got.PreserveSuccess != nil && *got.PreserveSuccess {
				t.Fatalf("%s incorrectly preserved: %+v", change, got)
			}
		})
	}
}

func TestCommitStatus_FailedAndNoopSameHeadRepairsCannotPreserve(t *testing.T) {
	d, run, p := certifiedStatusFixture(t)
	if err := d.InvalidateStatusValidation(run.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunPushActive(run.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := d.TerminalizeCertifiedWatcher(run.ID, types.RunCancelReasonAbortedByUser, "head", "config", types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetCommitStatusPublication(context.Background(), run.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.PreserveSuccess != nil && *got.PreserveSuccess {
		t.Fatal("no-op repair revived proof")
	}
	ids, err := d.CommitStatusRecoveryRunIDs(context.Background())
	if err != nil || fmt.Sprint(ids) != fmt.Sprint([]string{run.ID}) {
		t.Fatalf("failure retry missing: %v,%v", ids, err)
	}
}
