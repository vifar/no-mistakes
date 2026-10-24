package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #188: a spent Document commit budget plus a REAL documentation gap
// was a total deadlock in strict shipping. The read-only gap check parked
// the gap; strict shipping refused `approve` ("cannot approve unresolved
// actionable findings") and `skip` ("forbids skipped steps"); and `fix` only
// re-ran the read-only check, which could not land the fix and re-reported
// the same gap. Every disposition was refused, so the run had to be aborted.
//
// The way forward: a fix response that a human (or `axi --yes`, once per
// step) sends to that gate may land ONE Document commit for the findings it
// selected. The automatic pass stays capped; only an explicit fix response
// earns the commit, and each response earns at most one.

// capGapScenario spends a one-commit budget, moves the head with a reviewed
// code fix, and parks the Document gate on the gap the read-only check
// reports. fix is what the agent does on the fix round that follows.
type capGapScenario struct {
	gap    string // the gap check's finding JSON
	fix    func(t *testing.T, dir string) string
	limit  int
	spend  bool
	parked *pipeline.StepOutcome
	sc     *pipeline.StepContext
	dir    string
	calls  []agent.RunOpts
}

func (s *capGapScenario) run(t *testing.T) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	s.dir = dir
	phase := "spend"
	if !s.spend {
		phase = "check"
	}
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		s.calls = append(s.calls, opts)
		switch phase {
		case "spend":
			phase = "check"
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("guide\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update guide"}`)}, nil
		case "check":
			return &agent.Result{Output: json.RawMessage(`{"findings":[` + s.gap + `],"summary":"gap remains"}`)}, nil
		default:
			return &agent.Result{Output: json.RawMessage(s.fix(t, dir))}, nil
		}
	}}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = s.limit
	sc.Run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull}
	s.sc = sc

	if s.spend {
		if out, err := (&DocumentStep{}).Execute(sc); err != nil {
			t.Fatal(err)
		} else if out.NeedsApproval {
			t.Fatalf("the first pass parked: %+v", out)
		}
		writeAndCommit("feature.txt", "reviewed repair\n")(t, dir)
		moved := gitCmd(t, dir, "rev-parse", "HEAD")
		sc.Run.HeadSHA = moved
		if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, moved); err != nil {
			t.Fatal(err)
		}
	}

	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if !out.NeedsApproval {
		t.Fatalf("a real gap at the cap did not park: %+v", out)
	}
	if refusal := strictRefusalFor(t, sc, out.Findings); !strings.Contains(refusal, "unresolved actionable findings") {
		t.Fatalf("strict shipping did not refuse the real gap: %q", refusal)
	}
	s.parked = out
	phase = "fix"
}

// respondFix sends a fix response selecting every actionable finding of the
// parked gate, the way the executor hands a human fix round to the step.
func (s *capGapScenario) respondFix(t *testing.T, auto bool) (*pipeline.StepOutcome, error) {
	t.Helper()
	parsed, err := types.ParseFindingsJSON(s.parked.Findings)
	if err != nil {
		t.Fatal(err)
	}
	var selected types.Findings
	for _, item := range parsed.Items {
		if item.Action != types.ActionNoOp {
			selected.Items = append(selected.Items, item)
		}
	}
	raw, err := types.MarshalFindingsJSON(selected)
	if err != nil {
		t.Fatal(err)
	}
	s.sc.Fixing = true
	s.sc.AutoFixRound = auto
	s.sc.PreviousFindings = raw
	return (&DocumentStep{}).Execute(s.sc)
}

func strictRefusalFor(t *testing.T, sc *pipeline.StepContext, findings string) string {
	t.Helper()
	row, err := sc.DB.InsertStepResult(sc.Run.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.DB.ParkStepForApproval(sc.Run.ID, row.ID, types.StepStatusAwaitingApproval, 0, 0, &findings); err != nil {
		t.Fatal(err)
	}
	persisted, err := sc.DB.GetStepResult(row.ID)
	if err != nil || persisted == nil {
		t.Fatalf("parked row: %+v, %v", persisted, err)
	}
	return persisted.StrictApprovalRefusal("")
}

func fixReadme(content string) func(t *testing.T, dir string) string {
	return func(t *testing.T, dir string) string {
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return `{"findings":[],"summary":"fix the remaining documentation gap"}`
	}
}

func (s *capGapScenario) assertGraceCommitLanded(t *testing.T, out *pipeline.StepOutcome, headBefore string) {
	t.Helper()
	if out.NeedsApproval {
		t.Fatalf("the fix round did not clear the gap: %s", out.Findings)
	}
	if got := gitCmd(t, s.dir, "rev-list", "--count", headBefore+".."+s.sc.Run.HeadSHA); got != "1" {
		t.Fatalf("the fix round landed %s commits, want exactly 1", got)
	}
	if refusal := strictRefusalFor(t, s.sc, out.Findings); refusal != "" {
		t.Fatalf("strict shipping still refuses after the fix landed: %s", refusal)
	}
	if got := gitStatusPorcelain(t, s.dir); got != "" {
		t.Fatalf("the fix round left a dirty worktree: %s", got)
	}
	history, err := readDocumentHistory(s.sc)
	if err != nil {
		t.Fatal(err)
	}
	if history.commits != s.limit+1 {
		t.Fatalf("the grace commit was not recorded in the Document history: %d commits, want %d", history.commits, s.limit+1)
	}
}

// Spent budget + a real INFO auto-fix finding (controller run
// 01M5MVMCTQH2Q48B7WCVCBJ2M9): the fix response lands one commit and the
// strict gate clears without an abort.
func TestDocumentCapFixResponseLandsOneCommitForAnInfoGap(t *testing.T) {
	s := &capGapScenario{
		limit: 1, spend: true,
		gap: `{"id":"doc-lane-telemetry-labels","severity":"info","file":"README.md","description":"two comments omit two label values","action":"auto-fix"}`,
		fix: fixReadme("guide with every label value\n"),
	}
	s.run(t)
	headBefore := s.sc.Run.HeadSHA
	out, err := s.respondFix(t, false)
	if err != nil {
		t.Fatal(err)
	}
	s.assertGraceCommitLanded(t, out, headBefore)
	// The fix round ran the REPAIR prompt, not the read-only check: the
	// read-only check cannot land a fix, which was the deadlock.
	last := s.calls[len(s.calls)-1]
	if strings.Contains(last.Prompt, "read-only gap check") || last.Purpose == "document-gap-check" {
		t.Fatalf("the fix round ran the read-only gap check again: %s", last.Purpose)
	}
}

// Spent budget + a real WARNING ask-user finding (controller run
// 01M5KZ3P5RM9A15C9KCKEQM1MR): the same way forward.
func TestDocumentCapFixResponseLandsOneCommitForAWarningGap(t *testing.T) {
	s := &capGapScenario{
		limit: 1, spend: true,
		gap: `{"id":"document-1","severity":"warning","file":"README.md","description":"the rollout guide is still stale","action":"ask-user"}`,
		fix: fixReadme("current rollout guide\n"),
	}
	s.run(t)
	headBefore := s.sc.Run.HeadSHA
	out, err := s.respondFix(t, false)
	if err != nil {
		t.Fatal(err)
	}
	s.assertGraceCommitLanded(t, out, headBefore)
}

// A zero cap disables the Document step's own commits, and a real gap under
// it deadlocked a strict run the same way. An explicit fix response is the
// owner opting in, so it earns the same single commit.
func TestDocumentCapFixResponseLandsOneCommitUnderAZeroCap(t *testing.T) {
	s := &capGapScenario{
		limit: 0, spend: false,
		gap: `{"id":"document-1","severity":"warning","file":"README.md","description":"the rollout guide is still stale","action":"ask-user"}`,
		fix: fixReadme("current rollout guide\n"),
	}
	s.run(t)
	headBefore := s.sc.Run.HeadSHA
	out, err := s.respondFix(t, false)
	if err != nil {
		t.Fatal(err)
	}
	s.assertGraceCommitLanded(t, out, headBefore)
}

// The commit is bounded: a fix round that proposes two commits is discarded
// like any other over-budget proposal, and the gap stays visible.
func TestDocumentCapFixResponseIsBoundedToOneCommit(t *testing.T) {
	s := &capGapScenario{
		limit: 1, spend: true,
		gap: `{"id":"document-1","severity":"warning","file":"README.md","description":"the rollout guide is still stale","action":"ask-user"}`,
		fix: func(t *testing.T, dir string) string {
			writeAndCommit("README.md", "first\n")(t, dir)
			writeAndCommit("README.md", "second\n")(t, dir)
			return `{"findings":[],"summary":"two commits"}`
		},
	}
	s.run(t)
	headBefore := s.sc.Run.HeadSHA
	out, err := s.respondFix(t, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, s.dir, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("an over-budget fix round moved the head: %s -> %s", headBefore, got)
	}
	if !out.NeedsApproval || !strings.Contains(out.Findings, "the rollout guide is still stale") {
		t.Fatalf("the discarded fix round lost the gap: %s", out.Findings)
	}
}

// The automatic fix loop never earns the commit: only an explicit fix
// response does, so the cap still bounds what the pipeline does on its own.
func TestDocumentCapAutomaticFixRoundStaysReadOnly(t *testing.T) {
	s := &capGapScenario{
		limit: 1, spend: true,
		gap: `{"id":"document-1","severity":"info","file":"README.md","description":"two comments omit two label values","action":"auto-fix"}`,
		fix: func(t *testing.T, dir string) string {
			return `{"findings":[{"id":"document-1","severity":"info","file":"README.md","description":"two comments omit two label values","action":"auto-fix"}],"summary":"gap remains"}`
		},
	}
	s.run(t)
	headBefore := s.sc.Run.HeadSHA
	out, err := s.respondFix(t, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, s.dir, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("the automatic fix round committed past the cap: %s -> %s", headBefore, got)
	}
	if last := s.calls[len(s.calls)-1]; last.Purpose != "document-gap-check" {
		t.Fatalf("the automatic fix round did not run the read-only check: %s", last.Purpose)
	}
	if !out.NeedsApproval {
		t.Fatalf("the gap vanished without a fix: %s", out.Findings)
	}
}

// The cap notice and the strict refusals name the way forward that works -
// a fix response - and no longer send the operator to abort the run.
func TestDocumentCapWayForwardNamesTheFixResponse(t *testing.T) {
	if strings.Contains(pipeline.DocumentCapWayForward, "Abort") {
		t.Fatalf("the way forward still says to abort the run: %s", pipeline.DocumentCapWayForward)
	}
	if !strings.Contains(pipeline.DocumentCapWayForward, "--action fix") {
		t.Fatalf("the way forward does not name the fix response: %s", pipeline.DocumentCapWayForward)
	}
}
