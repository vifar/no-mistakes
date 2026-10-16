package db

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func targetRewindDBFixture(t *testing.T) (*DB, string, PRContextCandidate, TargetAdvanceRewindRequest) {
	t.Helper()
	d := openTestDB(t)
	id := testPRContextRun(t, d)
	c := testPRContextCandidate()
	if err := d.UpdateRunHeadSHA(id, c.LocalHeadSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := d.BindRunPRContext(id, c, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepResult(id, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	c.TargetSHA = strings.Repeat("e", 40)
	return d, id, c, TargetAdvanceRewindRequest{ExpectedGeneration: 1, Step: types.StepTest, MaxRewinds: 3}
}

func assertRewindCharge(t *testing.T, d *DB, id string, want int64, pending bool) {
	t.Helper()
	r, err := d.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	var rows int64
	if err := d.sql.QueryRow(`SELECT count(*) FROM run_target_rewinds WHERE run_id=?`, id).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	p, err := d.GetPendingTargetRewind(id)
	if err != nil {
		t.Fatal(err)
	}
	if r.TargetAdvanceRewinds != want || rows != want || (p != nil) != pending {
		t.Fatalf("charge=%d rows=%d pending=%+v; want %d, %t", r.TargetAdvanceRewinds, rows, p, want, pending)
	}
}

func TestTargetRewindTransactionChargesOnlyCommittedFastForward(t *testing.T) {
	d, id, c, req := targetRewindDBFixture(t)
	review, err := d.InsertStepResult(id, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	round, err := d.InsertStepRound(review.ID, 1, "initial", nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.ParkStepForApproval(id, review.ID, types.StepStatusAwaitingApproval, 0, 1, nil, round.ID); err != nil {
		t.Fatal(err)
	}
	parked, err := d.GetStepResult(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldGate := parked.GateIdentity()
	response, err := json.Marshal(map[string]any{"gate": oldGate, "action": types.ActionApprove})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SetStepPendingResponse(review.ID, string(response), "answered", oldGate); err != nil {
		t.Fatal(err)
	}
	if err = d.UpdateRunReviewApprovedHeadSHA(id, c.LocalHeadSHA); err != nil {
		t.Fatal(err)
	}
	b, err := d.RewindRunPRContext(id, c, req)
	if err != nil || !b.Changed {
		t.Fatalf("rewind=%+v %v", b, err)
	}
	assertRewindCharge(t, d, id, 1, true)
	row, err := d.GetStepResult(review.ID)
	if err != nil || row.Status != types.StepStatusPending || row.FindingsJSON != nil || row.PendingResponse != nil || row.GateNonce != nil || row.GateRoundID != nil {
		t.Fatalf("stale gate survived: %+v %v", row, err)
	}
	if err := d.SetStepPendingResponse(review.ID, `{"action":"approve"}`, "obsolete response", oldGate); err == nil {
		t.Fatal("pre-rewind gate still accepted a response")
	}
	if err := d.ResumeStepFromApproval(id, review.ID, "obsolete dispatch", 1, oldGate); err == nil {
		t.Fatal("pre-rewind response was dispatched")
	}
	rounds, err := d.GetRoundsByStep(review.ID)
	if err != nil || len(rounds) != 0 {
		t.Fatalf("rounds survived: %+v %v", rounds, err)
	}
	b, err = d.RewindRunPRContext(id, c, req)
	if err != nil || b.Changed {
		t.Fatalf("identical retry=%+v %v", b, err)
	}
	assertRewindCharge(t, d, id, 1, true)
}

func TestTargetRewindTransactionRechecksEligibilityAndGeneration(t *testing.T) {
	for _, kind := range []string{"last pushed", "push generation", "status attempt", "G attachment", "PR completed", "retarget attempted", "strict", "skipped rebase", "push step", "stale generation", "wrong local head", "cap"} {
		t.Run(kind, func(t *testing.T) {
			d, id, c, req := targetRewindDBFixture(t)
			var err error
			switch kind {
			case "last pushed":
				_, err = d.sql.Exec(`UPDATE runs SET last_pushed_sha=? WHERE id=?`, c.LocalHeadSHA, id)
			case "push generation":
				_, err = d.sql.Exec(`UPDATE runs SET push_generation=1 WHERE id=?`, id)
			case "status attempt":
				_, err = d.sql.Exec(`INSERT INTO commit_status_attempts (run_id,sha,host,repo,target,ref) VALUES (?,?,'h','r','t','ref')`, id, c.LocalHeadSHA)
			case "G attachment":
				_, err = d.sql.Exec(`INSERT INTO commit_status_publications (run_id,sha,host,repo) VALUES (?,?,'h','r')`, id, c.LocalHeadSHA)
			case "PR completed":
				var row *StepResult
				row, err = d.InsertStepResult(id, types.StepPR)
				if err == nil {
					err = d.UpdateStepStatus(row.ID, types.StepStatusCompleted)
				}
			case "retarget attempted":
				err = d.MarkRunRetargetAttempted(id)
				if err == nil {
					err = d.ConsumePRBaseBranchRequest(id)
				}
			case "strict":
				_, err = d.sql.Exec(`UPDATE runs SET shipping_options='{"mode":"full"}' WHERE id=?`, id)
			case "skipped rebase":
				_, err = d.sql.Exec(`UPDATE step_results SET status='skipped' WHERE run_id=? AND step_name='rebase'`, id)
			case "push step":
				req.Step = types.StepPush
			case "stale generation":
				req.ExpectedGeneration = 2
			case "wrong local head":
				err = d.UpdateRunHeadSHA(id, strings.Repeat("f", 40))
			case "cap":
				req.MaxRewinds = 0
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.RewindRunPRContext(id, c, req); err == nil {
				t.Fatal("ineligible transaction committed")
			}
			assertRewindCharge(t, d, id, 0, false)
			receipt, err := d.GetRunPRContext(id)
			if err != nil || receipt.Generation != 1 {
				t.Fatalf("receipt changed: %+v %v", receipt, err)
			}
		})
	}
}

func TestTargetRewindTransactionRollbackDoesNotCharge(t *testing.T) {
	d, id, c, req := targetRewindDBFixture(t)
	if _, err := d.sql.Exec(`CREATE TRIGGER fail_rewind BEFORE UPDATE OF review_approved_head_sha ON runs BEGIN SELECT RAISE(ABORT,'forced failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RewindRunPRContext(id, c, req); err == nil {
		t.Fatal("failed transaction committed")
	}
	assertRewindCharge(t, d, id, 0, false)
}

func TestTargetRewindPendingKeepsOriginalSourceAndConsumesIdenticalReceipt(t *testing.T) {
	for _, source := range []string{"", strings.Repeat("9", 40)} {
		t.Run(source, func(t *testing.T) {
			d, id, c, req := targetRewindDBFixture(t)
			if source != "" {
				req.SourceSHA = &source
			}
			if _, err := d.RewindRunPRContext(id, c, req); err != nil {
				t.Fatal(err)
			}
			original, err := d.GetPendingTargetRewind(id)
			if err != nil {
				t.Fatal(err)
			}
			req.ExpectedGeneration = 2
			c.TargetSHA = strings.Repeat("f", 40)
			changed := strings.Repeat("8", 40)
			req.SourceSHA = &changed
			if _, err := d.RewindRunPRContext(id, c, req); err == nil {
				t.Fatal("replaced original source baseline")
			}
			req.SourceSHA = original.SourceSHA
			if _, err := d.RewindRunPRContext(id, c, req); err != nil {
				t.Fatal(err)
			}
			assertRewindCharge(t, d, id, 2, true)
			current, err := d.GetPendingTargetRewind(id)
			if err != nil {
				t.Fatal(err)
			}
			if !sameOptionalSHA(current.SourceSHA, original.SourceSHA) {
				t.Fatal("source baseline changed")
			}
			if _, err := d.CompleteRebasePRContext(id, c, 3); err != nil {
				t.Fatal(err)
			}
			assertRewindCharge(t, d, id, 2, false)
		})
	}
}

func TestTargetRewindTerminalTransitionsAndNewRuns(t *testing.T) {
	for _, kind := range []string{"fail", "cancel", "supersede", "status", "verified failure", "verified status", "crash"} {
		t.Run(kind, func(t *testing.T) {
			d, id, c, req := targetRewindDBFixture(t)
			if _, err := d.RewindRunPRContext(id, c, req); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "fail":
				err = d.UpdateRunError(id, "failed")
			case "cancel", "supersede":
				_, err = d.TerminateActiveRun(id, kind, types.RunCancelled)
			case "status":
				err = d.UpdateRunStatus(id, types.RunCompleted)
			case "verified failure":
				err = d.UpdateRunErrorStatusWithVerifiedHead(id, "failed", types.RunFailed, c.LocalHeadSHA)
			case "verified status":
				err = d.UpdateRunStatusWithVerifiedHead(id, types.RunCompleted, c.LocalHeadSHA)
			case "crash":
				_, err = d.RecoverStaleRuns("crash")
			}
			if err != nil {
				t.Fatal(err)
			}
			assertRewindCharge(t, d, id, 1, false)
			r, err := d.GetRun(id)
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := d.InsertRun(r.RepoID, r.Branch, r.HeadSHA, r.BaseSHA)
			if err != nil {
				t.Fatal(err)
			}
			assertRewindCharge(t, d, fresh.ID, 0, false)
		})
	}
}

func TestTargetRewindOrdinaryBindsNeverCharge(t *testing.T) {
	d, id, c, _ := targetRewindDBFixture(t)
	c.TargetSHA = strings.Repeat("b", 40)
	if _, err := d.BindRunPRContext(id, c, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	c.LocalHeadSHA = strings.Repeat("f", 40)
	if _, err := d.AdvanceRunPRContext(id, c); err != nil {
		t.Fatal(err)
	}
	c.PRURL = "https://example.com/acme/repo/pull/1"
	c.ForgeHeadSHA = c.LocalHeadSHA
	if _, err := d.BindRunPRContext(id, c, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	assertRewindCharge(t, d, id, 0, false)
}

func TestTargetRewindConcurrentIdenticalClassificationChargesOnce(t *testing.T) {
	d, id, c, req := targetRewindDBFixture(t)
	type result struct {
		bind PRContextBindResult
		err  error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() { b, err := d.RewindRunPRContext(id, c, req); results <- result{b, err} }()
	}
	changes := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.bind.Changed {
			changes++
		}
	}
	if changes != 1 {
		t.Fatalf("identical concurrent classifications changed %d times", changes)
	}
	assertRewindCharge(t, d, id, 1, true)
}

func TestTargetRewindOriginalPinSurvivesDatabaseReopen(t *testing.T) {
	d, id, c, req := targetRewindDBFixture(t)
	source := strings.Repeat("9", 40)
	req.SourceSHA = &source
	if _, err := d.RewindRunPRContext(id, c, req); err != nil {
		t.Fatal(err)
	}
	var sequence int
	var name, path string
	if err := d.sql.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
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
	pending, err := reopened.GetPendingTargetRewind(id)
	if err != nil || pending == nil || pending.SourceSHA == nil || *pending.SourceSHA != source || pending.LocalHeadSHA != c.LocalHeadSHA || pending.TargetSHA != c.TargetSHA {
		t.Fatalf("pending pin did not survive reopen: %+v %v", pending, err)
	}
	assertRewindCharge(t, reopened, id, 1, true)
}

func TestTargetRewindPreservesCoreSkipChoicesWithoutPreservingEvidence(t *testing.T) {
	d, id, c, req := targetRewindDBFixture(t)
	testStep, err := d.InsertStepResult(id, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	custom, err := d.InsertStepResult(id, types.CustomGateStepName(types.StepTest, "audit"))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []*StepResult{testStep, custom} {
		if _, err := d.sql.Exec(`UPDATE step_results SET status='skipped',findings_json='obsolete',pending_response='obsolete' WHERE id=?`, row.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.RewindRunPRContext(id, c, req); err != nil {
		t.Fatal(err)
	}
	preserved, err := d.GetStepResult(testStep.ID)
	if err != nil || preserved.Status != types.StepStatusSkipped || preserved.FindingsJSON != nil || preserved.PendingResponse != nil {
		t.Fatalf("requested core skip was lost or kept evidence: %+v %v", preserved, err)
	}
	reset, err := d.GetStepResult(custom.ID)
	if err != nil || reset.Status != types.StepStatusPending || reset.FindingsJSON != nil || reset.PendingResponse != nil {
		t.Fatalf("custom gate was not reset: %+v %v", reset, err)
	}
	c.LocalHeadSHA = strings.Repeat("f", 40)
	if err := d.UpdateRunHeadSHA(id, c.LocalHeadSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CompleteRebasePRContext(id, c, 2); err != nil {
		t.Fatal(err)
	}
	preserved, err = d.GetStepResult(testStep.ID)
	if err != nil || preserved.Status != types.StepStatusSkipped || preserved.FindingsJSON != nil {
		t.Fatalf("Rebase completion lost the core skip choice: %+v %v", preserved, err)
	}

}

func TestTargetRewindPersistsPRObservation(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, attached := range []bool{false, true} {
			t.Run(map[bool]string{false: "current", true: "upgrade"}[legacy]+"/"+map[bool]string{false: "absent", true: "attached"}[attached], func(t *testing.T) {
				d, id, c, req := targetRewindDBFixture(t)
				var want *string
				if attached {
					previous, err := d.GetRunPRContext(id)
					if err != nil {
						t.Fatal(err)
					}
					pinned := previous.PRContextCandidate
					pinned.PRURL = "https://github.com/test/repo/pull/42"
					pinned.ForgeHeadSHA = pinned.LocalHeadSHA
					bound, err := d.BindRunPRContext(id, pinned, types.StepRebase)
					if err != nil {
						t.Fatal(err)
					}
					c.PRURL, c.ForgeHeadSHA = pinned.PRURL, pinned.ForgeHeadSHA
					req.ExpectedGeneration, req.HasPR = bound.Generation, true
					want = &c.ForgeHeadSHA
				}
				for i := 0; i < 2; i++ {
					result, err := d.RewindRunPRContext(id, c, req)
					if err != nil {
						t.Fatal(err)
					}
					req.ExpectedGeneration = result.Generation
					c.TargetSHA = strings.Repeat("f", 40)
				}
				// Remove only the new column to model the already-supported pending
				// rewind schema before reopening through the real migration path.
				if legacy {
					if _, err := d.sql.Exec(`ALTER TABLE run_target_rewinds DROP COLUMN forge_head_sha`); err != nil {
						t.Fatal(err)
					}
				}
				var sequence int
				var name, path string
				if err := d.sql.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
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
				var got *string
				if err = reopened.sql.QueryRow(`SELECT forge_head_sha FROM run_target_rewinds WHERE id=(SELECT pending_target_rewind_id FROM runs WHERE id=?)`, id).Scan(&got); err != nil {
					t.Fatalf("pending PR observation was not persisted: %v", err)
				}
				if (want == nil) != (got == nil) || want != nil && got != nil && *want != *got {
					t.Fatalf("recovery PR pin=%v want=%v", got, want)
				}
			})
		}
	}
}
