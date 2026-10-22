package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #158: on a repository that declares no_ci there is no external check
// to fall back on, so the local Test verdict is the only validation there is.
// A run whose Test verdict is inconclusive could not finish at all: the
// accepted gap was refused on every non-strict run, and a plain --reason
// exception leaves the status pending. These tests pin the two halves of the
// fix: an accepted gap publishes an honestly named success, and a plain
// exception still does not.

// recordNonStrictAcceptedTestGap records the gap the way a non-strict run
// records it: through the durable recording, with the run's own (non-strict)
// eligibility rule.
func recordNonStrictAcceptedTestGap(t *testing.T, sc *pipeline.StepContext, id, head, reason, coveredBy string) {
	t.Helper()
	if err := sc.DB.SetStepFindings(id, inconclusiveTestFindings(head)); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.DB.RecordAcceptedGaps(id, db.AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: reason, CoveredBy: coveredBy}, false); err != nil {
		t.Fatal(err)
	}
}

// The headline of issue #158: a repository that declares no_ci finishes a run
// whose Test verdict is inconclusive with an owner-approved gap, and the
// published success names that gap.
func TestNoCIWithAnAcceptedTestGapPublishesSuccess(t *testing.T) {
	const reason = "owner approved via first mate on the laptop, 2026-10-21: the unreachable scenario has no live runtime"
	const coveredBy = "TestBrokerTimeoutMocked; internal/broker/timeout_test.go"
	sc, head := acceptedGapCommitStatusFixture(t, func(sc *pipeline.StepContext, id, head string) {
		recordNonStrictAcceptedTestGap(t, sc, id, head, reason, coveredBy)
	})
	// The repository declares no_ci: there is no external check to fall back
	// on, which is exactly the class of repository the gap has to serve.
	sc.Config.NoCI = true
	if sc.Run.Shipping != nil {
		t.Fatal("the fixture run is not a plain run")
	}
	if !localValidationPassed(sc, head) || !localValidationPassed(sc, "") {
		t.Fatal("an accepted, recorded Test gap did not count as local validation on a no_ci run")
	}
	requests := postCommitStatus(t, sc, db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true, LastState: "pending"}, "success", false)
	if len(requests) != 1 || requests[0].State != "success" || requests[0].Context != "no-mistakes" {
		t.Fatalf("requests = %+v, want one no-mistakes success", requests)
	}
	if !strings.HasPrefix(requests[0].Description, "passed with accepted test gap: owner approved via first mate") {
		t.Fatalf("description %q does not name the accepted gap", requests[0].Description)
	}
	// The gap stays recorded and reported: it is not converted to a clean pass.
	steps, err := sc.DB.GetStepsByRun(sc.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, step := range steps {
		if step.StepName != types.StepTest {
			continue
		}
		gap := step.ValidAcceptedGaps()
		if gap == nil {
			t.Fatalf("the accepted gap is no longer recorded: %+v", step)
		}
		if gap.Reason != reason || gap.CoveredBy != coveredBy {
			t.Fatalf("recorded gap = %+v, want the reason and covered-by stored verbatim", gap)
		}
		if !strings.Contains(step.TestOverrideReason(), "Test gap accepted (test-verdict)") {
			t.Fatalf("TestOverrideReason = %q", step.TestOverrideReason())
		}
		found = true
	}
	if !found {
		t.Fatal("the run holds no Test step")
	}
}

// The guard against turning the change into a way to skip a gate: a plain
// --reason Test exception on a non-strict run still leaves the status pending
// and still is not local validation passed.
func TestNonStrictPlainReasonLeavesTheStatusPending(t *testing.T) {
	sc, head := acceptedGapCommitStatusFixture(t, func(sc *pipeline.StepContext, id, head string) {
		if err := sc.DB.SetStepFindings(id, inconclusiveTestFindings(head)); err != nil {
			t.Fatal(err)
		}
		if err := sc.DB.SetTestApprovalReason(id, "operator approved the exception"); err != nil {
			t.Fatal(err)
		}
	})
	if sc.Run.Shipping != nil {
		t.Fatal("the fixture run is not a plain run")
	}
	if localValidationPassed(sc, head) || localValidationPassed(sc, "") {
		t.Fatal("a plain Test exception counted as local validation")
	}
	// A completed run posts failure, not a green claim.
	requests := postCommitStatus(t, sc, db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", LastState: "pending"}, "success", true)
	if len(requests) != 1 || requests[0].State != "failure" {
		t.Fatalf("requests = %+v, want failure", requests)
	}
	if strings.Contains(requests[0].Description, "accepted test gap") {
		t.Fatalf("a plain exception was published as an accepted gap: %q", requests[0].Description)
	}
	// The plain exception is still reported as an exception, with no gap.
	steps, err := sc.DB.GetStepsByRun(sc.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName != types.StepTest {
			continue
		}
		if step.ValidAcceptedGaps() != nil {
			t.Fatalf("a plain exception was recorded as an accepted gap: %+v", step)
		}
		if !strings.Contains(step.TestOverrideReason(), "Test exception approved") {
			t.Fatalf("TestOverrideReason = %q", step.TestOverrideReason())
		}
	}
}

// A gap on a non-strict run is audited exactly as the strict path audits it:
// the reason and the covered-by are stored verbatim and the success status
// names the gap rather than claiming a clean validation.
func TestNonStrictAcceptedGapIsAuditedLikeTheStrictPath(t *testing.T) {
	const reason = "owner approved via first mate, 2026-10-21"
	const coveredBy = "TestBrokerTimeoutMocked; internal/broker/timeout_test.go"
	sc, _ := acceptedGapCommitStatusFixture(t, func(sc *pipeline.StepContext, id, head string) {
		recordNonStrictAcceptedTestGap(t, sc, id, head, reason, coveredBy)
	})
	row, err := sc.DB.GetStepResult(firstTestStepID(t, sc))
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := row.RecordedAcceptedGaps()
	if err != nil || recorded == nil {
		t.Fatalf("recorded gaps = %+v, %v", recorded, err)
	}
	if recorded.Reason != reason || recorded.CoveredBy != coveredBy {
		t.Fatalf("recorded gap = %+v, want the audit stored verbatim", recorded)
	}
	if recorded.Verdict != types.TestVerdictInconclusive || recorded.AcceptedAt == "" {
		t.Fatalf("recorded gap = %+v", recorded)
	}
	if row.ApprovalReason != nil || row.OverrideReason != nil {
		t.Fatalf("the gap was also recorded as an ordinary exception: %+v", row)
	}
	description := acceptedTestGapStatusDescription(recorded)
	if !strings.HasPrefix(description, acceptedTestGapStatusPrefix+reason) {
		t.Fatalf("status description %q does not name the gap", description)
	}
}

// firstTestStepID returns the run's Test step id.
func firstTestStepID(t *testing.T, sc *pipeline.StepContext) string {
	t.Helper()
	rows, err := sc.DB.GetStepsByRun(sc.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.StepName == types.StepTest {
			return row.ID
		}
	}
	t.Fatal("the run holds no Test step")
	return ""
}
