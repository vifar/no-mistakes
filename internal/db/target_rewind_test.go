package db

import (
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
	if _, err = d.sql.Exec(`UPDATE step_results SET status='completed',findings_json='old',pending_response='old' WHERE id=?`, review.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.InsertStepRound(review.ID, 1, "initial", nil, nil, 1); err != nil {
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
	if err != nil || row.Status != types.StepStatusPending || row.FindingsJSON != nil || row.PendingResponse != nil {
		t.Fatalf("stale gate survived: %+v %v", row, err)
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
