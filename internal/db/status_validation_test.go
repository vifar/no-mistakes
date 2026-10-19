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

func TestCommitStatus_PlannedShutdownRecognizesProofBeforeCancellation(t *testing.T) {
	d, run, p := certifiedStatusFixture(t)
	// Acknowledged strict success must route planned shutdown to cancellation
	// before TerminalizeCertifiedWatcher has made the preservation decision.
	if p.PreserveSuccess != nil {
		t.Fatal("fixture already contains a terminal preservation decision")
	}
	held, err := d.HasStatusPreservationProof(run.ID)
	if err != nil || !held {
		t.Fatalf("acknowledged certificate before cancellation = %v,%v", held, err)
	}
	if err := d.InvalidateStatusValidation(run.ID); err != nil {
		t.Fatal(err)
	}
	held, err = d.HasStatusPreservationProof(run.ID)
	if err != nil || held {
		t.Fatalf("revoked certificate still routes to preservation = %v,%v", held, err)
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

func TestRawShutdown_SupersessionIsAtomicAndPreservesCommittedDecision(t *testing.T) {
	d, committed, p := certifiedStatusFixture(t)
	binding := &ControlledShutdownBinding{Instance: "runtime", Lifecycle: "lifecycle", Reason: "planned"}
	if err := d.PrepareControlledShutdown([]string{committed.ID}, binding.Instance, binding.Lifecycle, binding.Reason); err != nil {
		t.Fatal(err)
	}
	if err := d.TerminalizeCertifiedWatcher(committed.ID, "planned", "head", "config", types.RunCancelled, binding); err != nil {
		t.Fatal(err)
	}
	active, err := d.InsertRun(committed.RepoID, "other", "other-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(active.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.PrepareControlledShutdown([]string{active.ID}, binding.Instance, binding.Lifecycle, binding.Reason); err != nil {
		t.Fatal(err)
	}
	if err := d.SupersedeUncommittedCancellations([]string{active.ID, "missing-run"}, binding.Instance); err == nil {
		t.Fatal("uncertain batch committed")
	}
	stillActive, err := d.GetRun(active.ID)
	if err != nil || stillActive.Status != types.RunRunning {
		t.Fatalf("failed batch partially terminalized: %+v,%v", stillActive, err)
	}
	matches, err := d.ControlledShutdownMatches(active.ID, binding.Instance, binding.Lifecycle, binding.Reason)
	if err != nil || !matches {
		t.Fatalf("failed batch partially superseded binding: %v,%v", matches, err)
	}
	for range 2 {
		if err := d.SupersedeUncommittedCancellations([]string{committed.ID, active.ID}, binding.Instance); err != nil {
			t.Fatal(err)
		}
	}
	failed, err := d.GetRun(active.ID)
	if err != nil || failed.Status != types.RunRunning || failed.Error != nil {
		t.Fatalf("raw supersession preempted executor terminal authority: %+v,%v", failed, err)
	}
	matches, err = d.ControlledShutdownMatches(active.ID, binding.Instance, binding.Lifecycle, binding.Reason)
	if err != nil || matches {
		t.Fatalf("raw batch retained uncommitted binding: %v,%v", matches, err)
	}
	preserved, err := d.GetCommitStatusPublication(context.Background(), committed.ID, p)
	if err != nil || preserved.StrictCertificate == nil || preserved.PreserveSuccess == nil || !*preserved.PreserveSuccess {
		t.Fatalf("raw batch changed committed preservation: %+v,%v", preserved, err)
	}
}

func TestRawShutdown_RevocationCannotRenewOrPreemptIndependentError(t *testing.T) {
	d, run, p := certifiedStatusFixture(t)
	if err := d.SupersedeUncommittedCancellations([]string{run.ID}, "runtime"); err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunRunning || stored.Error != nil {
		t.Fatalf("raw shutdown stole terminal authority: %+v,%v", stored, err)
	}
	if err := d.PrepareControlledShutdown([]string{run.ID}, "runtime", "retry", "planned"); err == nil {
		t.Fatal("planned retry restored raw-superseded eligibility")
	}
	if err := d.InvalidateStatusValidation(run.ID); err != nil {
		t.Fatal(err)
	}
	epoch, err := d.StatusValidationEpoch(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := d.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if err := d.RecordStatusValidationStep(step.ID, run.ID, "head", "config", epoch); err != nil {
			t.Fatal(err)
		}
	}
	certificate, err := d.BuildStatusCertificate(run.ID, "head", "config")
	if err != nil || certificate != nil {
		t.Fatalf("fresh bindings renewed superseded runtime eligibility: %v,%v", certificate, err)
	}
	for _, state := range []string{"pending", "success"} {
		p.LastState = state
		if err := d.PutCommitStatusPublication(run.ID, p); err == nil {
			t.Errorf("late %s ledger crossed raw supersession", state)
		}
	}
	if err := d.RecordIneligibleTerminal(run.ID, "independent provider failure", types.RunFailed, nil); err != nil {
		t.Fatal(err)
	}
	stored, err = d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != "independent provider failure" {
		t.Fatalf("real failure lost precedence: %+v,%v", stored, err)
	}
}

func TestCommitStatus_CustomGateUnresolvedFindingsCannotCertifyOrPreserve(t *testing.T) {
	for _, anchor := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
		for _, tc := range []struct {
			name, findings string
			eligible       bool
		}{
			{"clean", "", true},
			{"empty", `{"findings":[]}`, true},
			{"warning", `{"findings":[{"severity":"warning","description":"near limit"}]}`, true},
			{"error", `{"findings":[{"severity":"error","description":"below budget"}]}`, false},
			{"error no-op", `{"findings":[{"severity":"error","description":"below budget","action":"no-op"}]}`, false},
			{"legacy error", `{"items":[{"severity":"error","description":"below budget"}]}`, false},
			{"unreadable", "{", false},
		} {
			t.Run(string(anchor)+"/"+tc.name, func(t *testing.T) {
				d, run, p := certifiedStatusFixture(t)
				step, err := d.InsertStepResult(run.ID, types.CustomGateStepName(anchor, "budget"))
				if err != nil {
					t.Fatal(err)
				}
				if tc.findings != "" {
					if err := d.SetStepFindings(step.ID, tc.findings); err != nil {
						t.Fatal(err)
					}
				}
				if err := d.CompleteStep(step.ID, 0, 1, ""); err != nil {
					t.Fatal(err)
				}
				if err := d.RecordStatusValidationStep(step.ID, run.ID, "head", "config", 0); err != nil {
					t.Fatal(err)
				}
				certificate, err := d.BuildStatusCertificate(run.ID, "head", "config")
				if err != nil {
					t.Fatal(err)
				}
				if (certificate != nil) != tc.eligible {
					t.Errorf("certification eligible=%v, want %v", certificate != nil, tc.eligible)
				}
				if certificate != nil {
					p.StrictCertificate = certificate
					if err := d.PutCommitStatusPublication(run.ID, p); err != nil {
						t.Fatal(err)
					}
				}
				if err := d.TerminalizeCertifiedWatcher(run.ID, types.RunCancelReasonAbortedByUser, "head", "config", types.RunCancelled); err != nil {
					t.Fatal(err)
				}
				stored, err := d.GetCommitStatusPublication(context.Background(), run.ID, p)
				if err != nil {
					t.Fatal(err)
				}
				preserved := stored.PreserveSuccess != nil && *stored.PreserveSuccess
				if preserved != tc.eligible {
					t.Errorf("preserved success=%v, want %v", preserved, tc.eligible)
				}
				if !tc.eligible && stored.StrictCertificate != nil {
					t.Error("unresolved gate retained certificate")
				}
				terminal, err := d.GetRun(run.ID)
				if err != nil || terminal.Status != types.RunCancelled {
					t.Fatalf("truthful cancellation lost: %+v, %v", terminal, err)
				}
			})
		}
	}
}

func TestCommitStatus_CustomGateRequiresCurrentExecutionBinding(t *testing.T) {
	for _, binding := range []string{"missing", "head", "epoch", "config", "current"} {
		t.Run(binding, func(t *testing.T) {
			d, run, p := certifiedStatusFixture(t)
			sr, err := d.InsertStepResult(run.ID, types.CustomGateStepName(types.StepRebase, "budget"))
			if err != nil {
				t.Fatal(err)
			}
			if err := d.CompleteStep(sr.ID, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
			head, digest, epoch := "head", "config", int64(0)
			switch binding {
			case "head":
				head = "previous-head"
			case "epoch":
				epoch = -1
			case "config":
				digest = "previous-config"
			}
			if binding != "missing" {
				if err := d.RecordStatusValidationStep(sr.ID, run.ID, head, digest, 0); err != nil {
					t.Fatal(err)
				}
				if binding == "epoch" {
					if _, err := d.sql.Exec(`UPDATE status_validation_steps SET epoch=? WHERE step_id=?`, epoch, sr.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			certificate, err := d.BuildStatusCertificate(run.ID, "head", "config")
			if err != nil {
				t.Fatal(err)
			}
			eligible := binding == "current"
			if (certificate != nil) != eligible {
				t.Errorf("binding %s certificate=%v", binding, certificate != nil)
			}
			if certificate != nil {
				p.StrictCertificate = certificate
				if err := d.PutCommitStatusPublication(run.ID, p); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.TerminalizeCertifiedWatcher(run.ID, types.RunCancelReasonAbortedByUser, "head", "config", types.RunCancelled); err != nil {
				t.Fatal(err)
			}
			stored, err := d.GetCommitStatusPublication(context.Background(), run.ID, p)
			if err != nil {
				t.Fatal(err)
			}
			if (stored.PreserveSuccess != nil && *stored.PreserveSuccess) != eligible {
				t.Fatalf("binding %s preserved success: %+v", binding, stored)
			}
			if !eligible && stored.StrictCertificate != nil {
				t.Fatal("invalid execution binding retained certificate")
			}
		})
	}
}

func TestCommitStatus_CustomGateProofDriftRevokesAcknowledgedCertificate(t *testing.T) {
	for _, drift := range []string{"head_sha", "epoch", "config_digest"} {
		t.Run(drift, func(t *testing.T) {
			d, run, p := certifiedStatusFixture(t)
			sr, err := d.InsertStepResult(run.ID, types.CustomGateStepName(types.StepRebase, "budget"))
			if err != nil {
				t.Fatal(err)
			}
			if err := d.CompleteStep(sr.ID, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
			if err := d.RecordStatusValidationStep(sr.ID, run.ID, "head", "config", 0); err != nil {
				t.Fatal(err)
			}
			certificate, err := d.BuildStatusCertificate(run.ID, "head", "config")
			if err != nil || certificate == nil {
				t.Fatalf("current gate cannot certify: %v", err)
			}
			p.StrictCertificate = certificate
			if err := d.PutCommitStatusPublication(run.ID, p); err != nil {
				t.Fatal(err)
			}
			var stale any = "stale"
			if drift == "epoch" {
				stale = int64(-1)
			}
			if _, err := d.sql.Exec("UPDATE status_validation_steps SET "+drift+"=? WHERE step_id=?", stale, sr.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.TerminalizeCertifiedWatcher(run.ID, types.RunCancelReasonAbortedByUser, "head", "config", types.RunCancelled); err != nil {
				t.Fatal(err)
			}
			stored, err := d.GetCommitStatusPublication(context.Background(), run.ID, p)
			if err != nil {
				t.Fatal(err)
			}
			if stored.StrictCertificate != nil || stored.PreserveSuccess != nil {
				t.Fatalf("%s drift preserved acknowledged success: %+v", drift, stored)
			}
		})
	}
}
