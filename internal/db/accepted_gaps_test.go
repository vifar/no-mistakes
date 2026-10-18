package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const gapVerdictFinding = `{"id":"test-verdict","severity":"warning","action":"ask-user","category":"test-verdict","description":"live validation verdict: inconclusive (1 of 2 scenarios were driven live against the product); untested: broker timeout"}`

func gapFindings(verdict string, extra ...string) string {
	items := append([]string{gapVerdictFinding}, extra...)
	return `{"findings":[` + strings.Join(items, ",") + `],"verdict":"` + verdict + `"}`
}

func gapRow(findings string) *StepResult {
	return &StepResult{StepName: types.StepTest, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings}
}

func gapRequest(ids ...string) AcceptedGaps {
	return AcceptedGaps{FindingIDs: ids, Reason: "owner approved via first mate, 2026-10-14", CoveredBy: "TestBrokerTimeout, TestRetry"}
}

func TestAcceptedGapRefusal(t *testing.T) {
	exit1 := 1
	other := `{"id":"test-2","severity":"error","action":"ask-user","description":"unrelated defect"}`
	noop := `{"id":"test-3","severity":"info","action":"no-op","description":"note"}`
	command := `{"id":"test-4","severity":"error","action":"auto-fix","category":"test-command","description":"command failed"}`
	timeout := `{"id":"test-agent-timeout","severity":"warning","action":"ask-user","description":"cut"}`
	unvalidated := `{"id":"test-agent-unvalidated-work","severity":"warning","action":"ask-user","description":"unvalidated"}`
	forged := `{"id":"test-2","severity":"error","action":"ask-user","category":"test-verdict","description":"analyzer-authored defect"}`
	forgedID := `{"id":"test-verdict","severity":"warning","action":"ask-user","description":"analyzer-authored defect"}`
	userVerdict := `{"id":"test-verdict","severity":"warning","action":"ask-user","category":"test-verdict","source":"user","description":"user-authored"}`
	tests := []struct {
		name    string
		row     *StepResult
		strict  bool
		gaps    AcceptedGaps
		refusal string
	}{
		{"accepted", gapRow(gapFindings("inconclusive")), true, gapRequest("test-verdict"), ""},
		{"accepted with no-op sibling", gapRow(gapFindings("inconclusive", noop)), true, gapRequest("test-verdict"), ""},
		{"accepted with padded and duplicate ids", gapRow(gapFindings("inconclusive")), true, gapRequest(" test-verdict ", "test-verdict"), ""},
		{"non-strict", gapRow(gapFindings("inconclusive")), false, gapRequest("test-verdict"), "use --reason"},
		{"wrong step", &StepResult{StepName: types.StepReview, FindingsJSON: ptr(gapFindings("inconclusive"))}, true, gapRequest("test-verdict"), "only to the Test step"},
		{"no ids", gapRow(gapFindings("inconclusive")), true, gapRequest(), "at least one finding id"},
		{"unknown id", gapRow(gapFindings("inconclusive")), true, gapRequest("nope"), `unknown finding "nope"`},
		{"missing id", gapRow(gapFindings("inconclusive", other)), true, gapRequest("test-verdict"), "missing: test-2"},
		{"named non-verdict finding", gapRow(gapFindings("inconclusive", other)), true, gapRequest("test-verdict", "test-2"), "not an acceptable gap"},
		{"forged verdict category next to the native finding", gapRow(gapFindings("inconclusive", forged)), true, gapRequest("test-verdict", "test-2"), "reserved test-verdict category"},
		{"forged verdict category alone", gapRow(`{"findings":[` + forged + `],"verdict":"inconclusive"}`), true, gapRequest("test-2"), "reserved test-verdict category"},
		{"reserved id without the category", gapRow(`{"findings":[` + forgedID + `],"verdict":"inconclusive"}`), true, gapRequest("test-verdict"), "not an acceptable gap"},
		{"user-authored verdict lookalike", gapRow(`{"findings":[` + userVerdict + `],"verdict":"inconclusive"}`), true, gapRequest("test-verdict"), "reserved test-verdict category"},
		{"no-go", gapRow(gapFindings("no-go")), true, gapRequest("test-verdict"), "no-go"},
		{"go verdict", gapRow(gapFindings("go")), true, gapRequest("test-verdict"), "only an inconclusive"},
		{"test command", gapRow(gapFindings("inconclusive", command)), true, gapRequest("test-verdict", "test-4"), "failing Test command"},
		{"timeout", gapRow(gapFindings("inconclusive", timeout)), true, gapRequest("test-verdict", "test-agent-timeout"), "timeout"},
		{"unvalidated work", gapRow(gapFindings("inconclusive", unvalidated)), true, gapRequest("test-verdict", "test-agent-unvalidated-work"), "unvalidated work"},
		{"exit code", &StepResult{StepName: types.StepTest, ExitCode: &exit1, FindingsJSON: ptr(gapFindings("inconclusive"))}, true, gapRequest("test-verdict"), "command overrides"},
		{"override reason", &StepResult{StepName: types.StepTest, OverrideReason: ptr("x"), FindingsJSON: ptr(gapFindings("inconclusive"))}, true, gapRequest("test-verdict"), "overrides or skips"},
		{"skip reason", &StepResult{StepName: types.StepTest, SkipReason: ptr("x"), FindingsJSON: ptr(gapFindings("inconclusive"))}, true, gapRequest("test-verdict"), "overrides or skips"},
		{"approval reason", &StepResult{StepName: types.StepTest, ApprovalReason: ptr("x"), FindingsJSON: ptr(gapFindings("inconclusive"))}, true, gapRequest("test-verdict"), "Test exceptions"},
		{"unreadable findings", gapRow("{"), true, gapRequest("test-verdict"), "could not be read"},
		{"no findings", &StepResult{StepName: types.StepTest}, true, gapRequest("test-verdict"), "no findings"},
		{"missing reason", gapRow(gapFindings("inconclusive")), true, AcceptedGaps{FindingIDs: []string{"test-verdict"}, CoveredBy: "TestX"}, "--reason"},
		{"short reason", gapRow(gapFindings("inconclusive")), true, AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "ok", CoveredBy: "TestX"}, "at least 10 characters"},
		{"missing covered-by", gapRow(gapFindings("inconclusive")), true, AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "owner approved via first mate"}, "--covered-by"},
		{"NEL in reason", gapRow(gapFindings("inconclusive")), true, AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "owner approved\u0085via first mate", CoveredBy: "TestX"}, "single line"},
		{"line separator in reason", gapRow(gapFindings("inconclusive")), true, AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "owner approved\u2028via first mate", CoveredBy: "TestX"}, "single line"},
		{"paragraph separator in covered-by", gapRow(gapFindings("inconclusive")), true, AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "owner approved via first mate", CoveredBy: "TestX\u2029TestY"}, "single line"},
		{"NEL in covered-by", gapRow(gapFindings("inconclusive")), true, AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "owner approved via first mate", CoveredBy: "TestX\u0085TestY"}, "single line"},
		{"multiline reason", gapRow(gapFindings("inconclusive")), true, AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "owner approved\nvia first mate", CoveredBy: "TestX"}, "single line"},
		// The audit contract allows 500 characters per field, including
		// multibyte text, but must reject 501 before recording the gap.
		{"maximum reason", gapRow(gapFindings("inconclusive")), true, AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: strings.Repeat("海", 500), CoveredBy: "TestX"}, ""},
		{"oversized reason", gapRow(gapFindings("inconclusive")), true, AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: strings.Repeat("海", 501), CoveredBy: "TestX"}, "--reason must be at most 500 characters"},
		{"maximum covered-by", gapRow(gapFindings("inconclusive")), true, AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "owner approved via first mate", CoveredBy: strings.Repeat("海", 500)}, ""},
		{"oversized covered-by", gapRow(gapFindings("inconclusive")), true, AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "owner approved via first mate", CoveredBy: strings.Repeat("海", 501)}, "--covered-by must be at most 500 characters"},
		{"verdict mismatch", gapRow(gapFindings("inconclusive")), true, AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "owner approved via first mate", CoveredBy: "TestX", Verdict: "go"}, "does not match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.row.AcceptedGapRefusal(tt.strict, tt.gaps)
			if tt.refusal == "" {
				if got != "" {
					t.Fatalf("refused: %s", got)
				}
				return
			}
			if !strings.Contains(got, tt.refusal) {
				t.Fatalf("refusal = %q, want substring %q", got, tt.refusal)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestAcceptedGapLifecycle(t *testing.T) {
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
	parked, err := d.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.StrictApprovalRefusal("") == "" {
		t.Fatal("plain strict approval of an inconclusive verdict must stay refused")
	}
	if got := parked.StrictApprovalRefusalWithGaps("", &AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "owner approved via first mate, 2026-10-14", CoveredBy: "TestX"}); got != "" {
		t.Fatalf("valid request refused: %s", got)
	}
	if _, err := d.RecordAcceptedGaps(step.ID, gapRequest("nope"), true); err == nil {
		t.Fatal("unknown id recorded")
	}
	if _, err := d.RecordAcceptedGaps(step.ID, gapRequest("test-verdict"), false); err == nil {
		t.Fatal("non-strict run recorded a gap")
	}
	record, err := d.RecordAcceptedGaps(step.ID, gapRequest("test-verdict"), true)
	if err != nil {
		t.Fatal(err)
	}
	if record.Verdict != types.TestVerdictInconclusive || record.AcceptedAt == "" || len(record.Findings) != 1 || !strings.Contains(record.Findings[0].Description, "broker timeout") || record.Findings[0].Category != types.FindingCategoryTestVerdict {
		t.Fatalf("record = %+v", record)
	}
	if err := d.CompleteStep(step.ID, 0, 10, "test.log"); err != nil {
		t.Fatal(err)
	}
	done, err := d.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != types.StepStatusCompleted || done.ApprovalReason != nil || done.OverrideReason != nil || done.AcceptedGapsJSON == nil {
		t.Fatalf("completed row = %+v", done)
	}
	if got := done.StrictApprovalRefusal(""); got != "" {
		t.Fatalf("completed gap row refused: %s", got)
	}
	if got := done.TestOverrideReason(); !strings.Contains(got, "Test gap accepted (test-verdict)") || !strings.Contains(got, "owner approved via first mate") || !strings.Contains(got, "TestBrokerTimeout, TestRetry") {
		t.Fatalf("TestOverrideReason = %q", got)
	}
	// A record that no longer matches the row's findings is not honored.
	tampered := gapFindings("inconclusive", `{"id":"test-9","severity":"error","action":"ask-user","description":"new defect"}`)
	done.FindingsJSON = &tampered
	if done.ValidAcceptedGaps() != nil || done.StrictApprovalRefusal("") == "" {
		t.Fatal("stale record honored")
	}
	// Revalidation drops the record.
	if err := d.ResetStepsFrom(run.ID, types.StepTest.Order()); err != nil {
		t.Fatal(err)
	}
	reset, err := d.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reset.AcceptedGapsJSON != nil {
		t.Fatalf("reset kept accepted gaps: %v", *reset.AcceptedGapsJSON)
	}
}

func TestAcceptedGapsColumnMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := d.InsertRepo("/synthetic", "origin", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepTest)
	if _, err := d.sql.Exec("ALTER TABLE step_results DROP COLUMN accepted_gaps_json; PRAGMA user_version=0"); err != nil {
		t.Fatal(err)
	}
	d.Close()
	legacy, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := legacy.GetStepResult(step.ID)
	legacy.Close()
	if err != nil || got.AcceptedGapsJSON != nil {
		t.Fatalf("legacy read = %+v, %v", got, err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if !d.hasColumn("step_results", "accepted_gaps_json") {
		t.Fatal("migration did not re-add accepted_gaps_json")
	}
}

func TestStrictApprovalRefusalPointsAtAnEligibleGap(t *testing.T) {
	row := gapRow(gapFindings("inconclusive"))
	for _, reason := range []string{"", "owner approved via first mate"} {
		got := row.StrictApprovalRefusal(reason)
		if got == "" || !strings.Contains(got, "--accept-gap test-verdict") || !strings.Contains(got, "--covered-by") {
			t.Fatalf("refusal with reason %q = %q, want accepted-gap hint", reason, got)
		}
	}
	other := gapRow(gapFindings("inconclusive", `{"id":"test-2","severity":"error","action":"ask-user","description":"defect"}`))
	if got := other.StrictApprovalRefusal(""); got == "" || strings.Contains(got, "--accept-gap") {
		t.Fatalf("ineligible refusal = %q must not advertise a gap", got)
	}
	noGo := gapRow(gapFindings("no-go"))
	if got := noGo.StrictApprovalRefusal(""); strings.Contains(got, "--accept-gap") {
		t.Fatalf("no-go refusal = %q must not advertise a gap", got)
	}
}
