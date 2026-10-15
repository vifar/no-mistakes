package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// MinAcceptedGapReasonLen is the shortest --reason an accepted gap takes. The
// reason must name who approved the gap and where, so a bare word is not an
// audit trail.
const MinAcceptedGapReasonLen = 10

// MaxAcceptedGapTextLen bounds --reason and --covered-by. Both are stored and
// rendered verbatim into a PR-body block that is never trimmed, so they stay
// single-line and short.
const MaxAcceptedGapTextLen = 500

// AcceptedGapTextRefusal reports why text cannot be recorded verbatim as the
// named flag, or "" when it can.
func AcceptedGapTextRefusal(flag, text string) string {
	if len([]rune(text)) > MaxAcceptedGapTextLen {
		return fmt.Sprintf("%s must be at most %d characters", flag, MaxAcceptedGapTextLen)
	}
	// Every Unicode control rune (C0, DEL and C1 such as U+0085) and every
	// line or paragraph separator (U+2028, U+2029: categories Zl and Zp) can
	// break the single-line audit block, so none is stored.
	if strings.ContainsFunc(text, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
	}) {
		return flag + " must be a single line without control or line-separator characters"
	}
	return ""
}

// AcceptedGapFinding snapshots one accepted finding so the audit record stays
// readable after the step's findings are rewritten.
type AcceptedGapFinding struct {
	ID          string `json:"id"`
	Category    string `json:"category"`
	Description string `json:"description"`
}

// AcceptedGaps is the owner-approved exception that lets a strict Test step
// complete past an inconclusive live-validation verdict. A request carries
// only FindingIDs, Reason and CoveredBy; RecordAcceptedGaps fills the rest.
// Reason and CoveredBy are stored verbatim.
type AcceptedGaps struct {
	FindingIDs []string             `json:"finding_ids"`
	Findings   []AcceptedGapFinding `json:"findings,omitempty"`
	Reason     string               `json:"reason"`
	CoveredBy  string               `json:"covered_by"`
	Verdict    string               `json:"verdict,omitempty"`
	AcceptedAt string               `json:"accepted_at,omitempty"`
}

// AcceptedGapRefusal is the single eligibility rule for an accepted Test gap.
// The manager pre-check, the executor's live gate, the durable recording and
// the strict receipt all call it, so the four cannot drift. It returns "" when
// gaps may be accepted for this step, otherwise the reason it may not.
//
// Only a strict run takes a gap; a non-strict run records its Test exception
// with --reason instead. Only an inconclusive verdict's test-verdict finding
// is acceptable, and every actionable finding on the step must be named, so a
// gap can never hide a failed scenario, a failing configured command, or a
// Test-agent budget cut.
func (s *StepResult) AcceptedGapRefusal(strict bool, gaps AcceptedGaps) string {
	if !strict {
		return "--accept-gap applies only to strict shipping runs; use --reason for a Test exception"
	}
	if s.StepName != types.StepTest {
		return "an accepted gap applies only to the Test step"
	}
	if len(gaps.FindingIDs) == 0 {
		return "an accepted gap must name at least one finding id"
	}
	if len([]rune(strings.TrimSpace(gaps.Reason))) < MinAcceptedGapReasonLen {
		return fmt.Sprintf("an accepted gap needs a --reason of at least %d characters naming who approved it and where", MinAcceptedGapReasonLen)
	}
	if strings.TrimSpace(gaps.CoveredBy) == "" {
		return "an accepted gap needs --covered-by naming the tests that cover it"
	}
	if refusal := AcceptedGapTextRefusal("--reason", gaps.Reason); refusal != "" {
		return refusal
	}
	if refusal := AcceptedGapTextRefusal("--covered-by", gaps.CoveredBy); refusal != "" {
		return refusal
	}
	if s.ExitCode != nil && *s.ExitCode != 0 {
		return "strict shipping forbids passing command overrides"
	}
	if s.OverrideReason != nil || s.SkipReason != nil {
		return "strict shipping forbids recorded overrides or skips"
	}
	if s.ApprovalReason != nil {
		return "strict shipping forbids Test exceptions beyond an accepted gap"
	}
	if s.FindingsJSON == nil {
		return "the parked Test step has no findings to accept"
	}
	findings, err := types.ParseFindingsJSON(*s.FindingsJSON)
	if err != nil {
		return "the parked Test findings could not be read"
	}
	if findings.Verdict == types.TestVerdictNoGo {
		return "a no-go Test verdict cannot be accepted as a gap; fix the failing scenarios"
	}
	if findings.Verdict != types.TestVerdictInconclusive {
		return fmt.Sprintf("only an inconclusive Test verdict can be accepted as a gap (verdict is %q)", findings.Verdict)
	}
	if gaps.Verdict != "" && gaps.Verdict != findings.Verdict {
		return "the recorded gap does not match the Test verdict"
	}
	byID := make(map[string]types.Finding, len(findings.Items))
	for _, item := range findings.Items {
		switch {
		case item.ID == types.FindingIDTestAgentTimeout:
			return "a Test-agent timeout cannot be accepted as a gap"
		case item.ID == types.FindingIDTestAgentUnvalidatedWork:
			return "unvalidated work after a Test-agent timeout cannot be accepted as a gap"
		case item.Category == types.FindingCategoryTestCommand:
			return "a failing Test command cannot be accepted as a gap"
		}
		// Only the native verdict finding may carry the verdict category; a
		// lookalike from the analyzer or a user is refused outright.
		if item.Category == types.FindingCategoryTestVerdict && !types.IsNativeTestVerdictFinding(item) {
			return fmt.Sprintf("finding %q claims the reserved test-verdict category but is not the Test step's own verdict finding", item.ID)
		}
		if item.ID != "" {
			byID[item.ID] = item
		}
	}
	named := make(map[string]bool, len(gaps.FindingIDs))
	for _, raw := range gaps.FindingIDs {
		id := strings.TrimSpace(raw)
		item, ok := byID[id]
		if !ok {
			return fmt.Sprintf("accepted gap names unknown finding %q", id)
		}
		if !types.IsNativeTestVerdictFinding(item) {
			return fmt.Sprintf("finding %q is not an acceptable gap; only the inconclusive live-validation verdict can be accepted", id)
		}
		named[id] = true
	}
	var missing []string
	for _, item := range findings.Items {
		if item.ActionOrDefault() != types.ActionNoOp && !named[item.ID] {
			missing = append(missing, item.ID)
		}
	}
	if len(missing) > 0 {
		return "accepted gap must name every actionable finding; missing: " + strings.Join(missing, ", ")
	}
	return ""
}

// RecordedAcceptedGaps decodes the audit record, or nil when none was recorded.
func (s *StepResult) RecordedAcceptedGaps() (*AcceptedGaps, error) {
	if s.AcceptedGapsJSON == nil || *s.AcceptedGapsJSON == "" {
		return nil, nil
	}
	var gaps AcceptedGaps
	if err := json.Unmarshal([]byte(*s.AcceptedGapsJSON), &gaps); err != nil {
		return nil, fmt.Errorf("decode accepted gaps: %w", err)
	}
	return &gaps, nil
}

// ValidAcceptedGaps returns the recorded gaps only if they still satisfy
// AcceptedGapRefusal against this row's findings. A tampered, truncated or
// stale record yields nil.
func (s *StepResult) ValidAcceptedGaps() *AcceptedGaps {
	gaps, err := s.RecordedAcceptedGaps()
	if err != nil || gaps == nil || s.AcceptedGapRefusal(true, *gaps) != "" {
		return nil
	}
	return gaps
}

// RecordAcceptedGaps validates gaps against the parked Test step and writes
// the audit record. It re-reads the row so the record is derived from the
// findings the step actually holds, never from the caller's description of
// them. The step status is untouched: it completes normally afterward.
func (d *DB) RecordAcceptedGaps(id string, gaps AcceptedGaps, strict bool) (AcceptedGaps, error) {
	row, err := d.GetStepResult(id)
	if err != nil {
		return AcceptedGaps{}, err
	}
	if row == nil || row.StepName != types.StepTest || row.Status != types.StepStatusAwaitingApproval && row.Status != types.StepStatusFixReview {
		return AcceptedGaps{}, fmt.Errorf("record accepted gaps: parked Test step not found")
	}
	if refusal := row.AcceptedGapRefusal(strict, gaps); refusal != "" {
		return AcceptedGaps{}, fmt.Errorf("%s", refusal)
	}
	findings, err := types.ParseFindingsJSON(*row.FindingsJSON)
	if err != nil {
		return AcceptedGaps{}, err
	}
	record := AcceptedGaps{
		Reason:     gaps.Reason,
		CoveredBy:  gaps.CoveredBy,
		Verdict:    findings.Verdict,
		AcceptedAt: time.Now().UTC().Format(time.RFC3339),
	}
	seen := map[string]bool{}
	for _, raw := range gaps.FindingIDs {
		id := strings.TrimSpace(raw)
		if seen[id] {
			continue
		}
		seen[id] = true
		for _, item := range findings.Items {
			if item.ID == id {
				record.FindingIDs = append(record.FindingIDs, id)
				record.Findings = append(record.Findings, AcceptedGapFinding{ID: id, Category: item.Category, Description: item.Description})
			}
		}
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return AcceptedGaps{}, err
	}
	result, err := d.sql.Exec(`UPDATE step_results SET accepted_gaps_json = ? WHERE id = ? AND step_name = ? AND status IN (?, ?)`,
		string(raw), id, types.StepTest, types.StepStatusAwaitingApproval, types.StepStatusFixReview)
	if err != nil {
		return AcceptedGaps{}, fmt.Errorf("record accepted gaps: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return AcceptedGaps{}, fmt.Errorf("record accepted gaps: parked Test step not found")
	}
	return record, nil
}

// AcceptableGapIDs returns the finding IDs an accepted-gap approval must name
// when this parked Test step is eligible for one, otherwise nil. It probes
// AcceptedGapRefusal with a placeholder reason and covering tests, so gate
// help and refusal hints can only offer a command the daemon would admit.
func (s *StepResult) AcceptableGapIDs() []string {
	if s.StepName != types.StepTest || s.FindingsJSON == nil {
		return nil
	}
	findings, err := types.ParseFindingsJSON(*s.FindingsJSON)
	if err != nil {
		return nil
	}
	var ids []string
	for _, item := range findings.Items {
		if item.ActionOrDefault() != types.ActionNoOp {
			ids = append(ids, item.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	probe := AcceptedGaps{FindingIDs: ids, Reason: strings.Repeat("x", MinAcceptedGapReasonLen), CoveredBy: "probe"}
	if s.AcceptedGapRefusal(true, probe) != "" {
		return nil
	}
	return ids
}

// acceptedGapHint is appended to strict approval refusals of a Test step that
// an owner-approved accepted gap could complete.
func (s *StepResult) acceptedGapHint() string {
	ids := s.AcceptableGapIDs()
	if len(ids) == 0 {
		return ""
	}
	return "; if the owner has approved this specific gap, use `axi respond --action approve --accept-gap " + strings.Join(ids, ",") + " --reason \"<who approved, where, when>\" --covered-by \"<tests>\"`"
}
