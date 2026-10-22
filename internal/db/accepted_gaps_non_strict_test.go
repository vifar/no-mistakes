package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #158: an accepted Test gap is the owner-approved, audited way past a
// Test step parked on an inconclusive verdict. The rule used to refuse that
// gap on every non-strict run, so a repository that declares no_ci - a
// repository with no external check to fall back on - could not finish such a
// run at all. The rule below is the same on every run; only strict shipping's
// own admission wording differs, and strict shipping's behaviour is unchanged.
//
// Each eligibility rule has its own test: a table that lumped them together
// would not tell a reviewer which rule loosened.

func nonStrictGapRefusal(t *testing.T, row *StepResult, gaps AcceptedGaps) string {
	t.Helper()
	return row.AcceptedGapRefusal(false, gaps)
}

// The headline of issue #158: a non-strict run admits an eligible gap.
func TestAcceptedGapRefusalAdmitsANonStrictRun(t *testing.T) {
	if got := nonStrictGapRefusal(t, gapRow(gapFindings("inconclusive")), gapRequest("test-verdict")); got != "" {
		t.Fatalf("a non-strict run refused an eligible accepted gap: %s", got)
	}
}

// Rule: the gap applies only to the Test step.
func TestNonStrictAcceptedGapAppliesOnlyToTheTestStep(t *testing.T) {
	row := &StepResult{StepName: types.StepReview, FindingsJSON: ptr(gapFindings("inconclusive"))}
	if got := nonStrictGapRefusal(t, row, gapRequest("test-verdict")); !strings.Contains(got, "only to the Test step") {
		t.Fatalf("refusal = %q, want the Test-step rule", got)
	}
}

// Rule: the gap must name at least one finding id.
func TestNonStrictAcceptedGapMustNameAFindingID(t *testing.T) {
	if got := nonStrictGapRefusal(t, gapRow(gapFindings("inconclusive")), gapRequest()); !strings.Contains(got, "at least one finding id") {
		t.Fatalf("refusal = %q, want the finding-id rule", got)
	}
}

// Rule: the --reason must be at least the minimum length.
func TestNonStrictAcceptedGapReasonMustMeetTheMinimumLength(t *testing.T) {
	gaps := AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "ok", CoveredBy: "TestX"}
	if got := nonStrictGapRefusal(t, gapRow(gapFindings("inconclusive")), gaps); !strings.Contains(got, "at least 10 characters") {
		t.Fatalf("refusal = %q, want the minimum-length rule", got)
	}
}

// Rule: the --reason must be a single line.
func TestNonStrictAcceptedGapReasonMustBeSingleLine(t *testing.T) {
	gaps := AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "owner approved\nvia first mate", CoveredBy: "TestX"}
	if got := nonStrictGapRefusal(t, gapRow(gapFindings("inconclusive")), gaps); !strings.Contains(got, "single line") {
		t.Fatalf("refusal = %q, want the single-line rule", got)
	}
}

// Rule: --covered-by must be non-empty.
func TestNonStrictAcceptedGapCoveredByMustNotBeEmpty(t *testing.T) {
	gaps := AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "owner approved via first mate", CoveredBy: "   "}
	if got := nonStrictGapRefusal(t, gapRow(gapFindings("inconclusive")), gaps); !strings.Contains(got, "--covered-by") {
		t.Fatalf("refusal = %q, want the covered-by rule", got)
	}
}

// Rule: --covered-by must be a single line.
func TestNonStrictAcceptedGapCoveredByMustBeSingleLine(t *testing.T) {
	gaps := AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "owner approved via first mate", CoveredBy: "TestX\nTestY"}
	if got := nonStrictGapRefusal(t, gapRow(gapFindings("inconclusive")), gaps); !strings.Contains(got, "single line") {
		t.Fatalf("refusal = %q, want the single-line rule", got)
	}
}

// Rule: neither --reason nor --covered-by may hold a Unicode control
// character (C0, DEL, or C1 such as U+0085) or a line or paragraph separator
// (U+2028, U+2029).
func TestNonStrictAcceptedGapTextRefusesControlAndSeparatorCharacters(t *testing.T) {
	cases := map[string]AcceptedGaps{
		"NEL in reason":              {FindingIDs: []string{"test-verdict"}, Reason: "owner approved\u0085via first mate", CoveredBy: "TestX"},
		"line separator in reason":   {FindingIDs: []string{"test-verdict"}, Reason: "owner approved\u2028via first mate", CoveredBy: "TestX"},
		"paragraph sep in coveredby": {FindingIDs: []string{"test-verdict"}, Reason: "owner approved via first mate", CoveredBy: "TestX\u2029TestY"},
		"NEL in covered-by":          {FindingIDs: []string{"test-verdict"}, Reason: "owner approved via first mate", CoveredBy: "TestX\u0085TestY"},
		"tab in reason":              {FindingIDs: []string{"test-verdict"}, Reason: "owner approved\tvia first mate", CoveredBy: "TestX"},
	}
	for name, gaps := range cases {
		t.Run(name, func(t *testing.T) {
			if got := nonStrictGapRefusal(t, gapRow(gapFindings("inconclusive")), gaps); !strings.Contains(got, "single line") {
				t.Fatalf("refusal = %q, want the control-character rule", got)
			}
		})
	}
}

// Rule: every actionable finding on the step must be named, so a gap can
// never hide a failed scenario.
func TestNonStrictAcceptedGapMustNameEveryActionableFinding(t *testing.T) {
	other := `{"id":"test-2","severity":"error","action":"ask-user","description":"unrelated defect"}`
	row := gapRow(gapFindings("inconclusive", other))
	if got := nonStrictGapRefusal(t, row, gapRequest("test-verdict")); !strings.Contains(got, "missing: test-2") {
		t.Fatalf("refusal = %q, want the every-actionable-finding rule", got)
	}
}

// Rule: a gap can never hide a failing configured command.
func TestNonStrictAcceptedGapRefusesAFailingTestCommand(t *testing.T) {
	command := `{"id":"test-4","severity":"error","action":"auto-fix","category":"test-command","description":"command failed"}`
	row := gapRow(gapFindings("inconclusive", command))
	if got := nonStrictGapRefusal(t, row, gapRequest("test-verdict", "test-4")); !strings.Contains(got, "failing Test command") {
		t.Fatalf("refusal = %q, want the failing-command rule", got)
	}
}

// Rule: a gap can never hide a Test-agent budget cut.
func TestNonStrictAcceptedGapRefusesATestAgentTimeout(t *testing.T) {
	timeout := `{"id":"test-agent-timeout","severity":"warning","action":"ask-user","description":"cut"}`
	row := gapRow(gapFindings("inconclusive", timeout))
	if got := nonStrictGapRefusal(t, row, gapRequest("test-verdict", "test-agent-timeout")); !strings.Contains(got, "timeout") {
		t.Fatalf("refusal = %q, want the Test-agent-timeout rule", got)
	}
}

// Rule: a step whose exit code is non-zero cannot be gap-accepted.
func TestNonStrictAcceptedGapRefusesANonZeroExitCode(t *testing.T) {
	exit1 := 1
	row := &StepResult{StepName: types.StepTest, Status: types.StepStatusAwaitingApproval, ExitCode: &exit1, FindingsJSON: ptr(gapFindings("inconclusive"))}
	if got := nonStrictGapRefusal(t, row, gapRequest("test-verdict")); got == "" {
		t.Fatal("a non-zero exit code was accepted as a gap")
	}
}

// Rule: only an inconclusive verdict can be accepted as a gap.
func TestNonStrictAcceptedGapRefusesANoGoVerdict(t *testing.T) {
	if got := nonStrictGapRefusal(t, gapRow(gapFindings("no-go")), gapRequest("test-verdict")); !strings.Contains(got, "no-go") {
		t.Fatalf("refusal = %q, want the no-go rule", got)
	}
}

// The durable recording stores the audit verbatim on a non-strict run, exactly
// as the strict path stores it.
func TestRecordAcceptedGapsStoresTheAuditOnANonStrictRun(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo("/synthetic", "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	findings := gapFindings("inconclusive")
	if err := d.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 10, &findings); err != nil {
		t.Fatal(err)
	}
	const reason = "owner approved via first mate, 2026-10-21"
	const coveredBy = "TestBrokerTimeoutMocked; internal/broker/timeout_test.go"
	record, err := d.RecordAcceptedGaps(step.ID, AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: reason, CoveredBy: coveredBy}, false)
	if err != nil {
		t.Fatalf("non-strict recording refused: %v", err)
	}
	if record.Reason != reason || record.CoveredBy != coveredBy {
		t.Fatalf("record = %+v, want the reason and covered-by stored verbatim", record)
	}
	if record.Verdict != types.TestVerdictInconclusive || record.AcceptedAt == "" || len(record.Findings) != 1 || record.FindingIDs[0] != "test-verdict" {
		t.Fatalf("record = %+v", record)
	}
	row, err := d.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.ValidAcceptedGaps() == nil {
		t.Fatal("the recorded gap is not honored on a non-strict run")
	}
}

// A plain --reason Test exception on a non-strict run is still a plain
// exception: it records no gap, so the step is not a clean local validation.
func TestPlainReasonOnANonStrictRunRecordsNoGap(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo("/synthetic", "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	findings := gapFindings("inconclusive")
	if err := d.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 10, &findings); err != nil {
		t.Fatal(err)
	}
	if err := d.SetTestApprovalReason(step.ID, "operator approved the exception"); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteStep(step.ID, 0, 10, "test.log"); err != nil {
		t.Fatal(err)
	}
	row, err := d.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.ValidAcceptedGaps() != nil {
		t.Fatal("a plain approval reason was recorded as an accepted gap")
	}
	if got := row.TestOverrideReason(); got == "" {
		t.Fatal("a plain Test exception stopped qualifying as an exception")
	}
}

// Strict shipping's own refusals are unchanged: the strict wording, the
// strict admission rules and the strict receipt keep exactly what they had.
func TestStrictAcceptedGapRefusalsAreUnchanged(t *testing.T) {
	exit1 := 1
	cases := []struct {
		name string
		row  *StepResult
		want string
	}{
		{"exit code", &StepResult{StepName: types.StepTest, ExitCode: &exit1, FindingsJSON: ptr(gapFindings("inconclusive"))}, "strict shipping forbids passing command overrides"},
		{"override reason", &StepResult{StepName: types.StepTest, OverrideReason: ptr("x"), FindingsJSON: ptr(gapFindings("inconclusive"))}, "strict shipping forbids recorded overrides or skips"},
		{"skip reason", &StepResult{StepName: types.StepTest, SkipReason: ptr("x"), FindingsJSON: ptr(gapFindings("inconclusive"))}, "strict shipping forbids recorded overrides or skips"},
		{"approval reason", &StepResult{StepName: types.StepTest, ApprovalReason: ptr("x"), FindingsJSON: ptr(gapFindings("inconclusive"))}, "strict shipping forbids Test exceptions beyond an accepted gap"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.row.AcceptedGapRefusal(true, gapRequest("test-verdict")); got != tt.want {
				t.Fatalf("strict refusal = %q, want %q", got, tt.want)
			}
		})
	}
}
