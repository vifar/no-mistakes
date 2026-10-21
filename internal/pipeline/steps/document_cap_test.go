package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The Document commit cap is a BOUND on commits, not a verdict on the
// documentation. When the cap is reached the step must still find out whether
// real gaps remain: a run whose documentation is already accurate must be able
// to finish instead of parking forever on a finding no agent can resolve.
//
// These tests cover the two triggers that parked real runs (issue #123):
// an automatic rebase that renames the head without changing the content,
// and an ordinary reviewed code fix after the second Document commit.

// documentCapAgent answers the FIRST call as a normal Document pass that
// commits, and every later call as the read-only gap check.
type documentCapAgent struct {
	t         *testing.T
	dir       string
	commit    bool
	gapOutput string
	calls     int
	// checkPrompts records the prompts of the read-only gap checks.
	checkPrompts []string
	// editOnCheck makes the gap check edit the worktree, the way an agent
	// that ignores its read-only instruction would.
	editOnCheck bool
	// editCalls limits how many gap checks edit. Zero means every one of them
	// does.
	editCalls int
	// failOnCheck makes the gap check fail after it edited.
	failOnCheck bool
	// cancelDuringCheck cancels the run's context while the check runs, the
	// way a daemon shutdown does.
	cancelDuringCheck func()
	// hideOnCheck makes the gap check hide a file from Git status behind an
	// index flag.
	hideOnCheck bool
	// commitOnCheck makes the gap check commit its repair.
	commitOnCheck bool
	// firstOutput overrides the first pass's structured output.
	firstOutput string
	// checks counts the gap checks so far, INCLUDING the one running.
	checks int
}

func (a *documentCapAgent) Name() string { return "test" }

func (a *documentCapAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.calls++
	if a.calls == 1 && a.commit {
		if err := os.WriteFile(filepath.Join(a.dir, "README.md"), []byte("guide\n"), 0o644); err != nil {
			a.t.Fatal(err)
		}
		if a.firstOutput != "" {
			return &agent.Result{Output: json.RawMessage(a.firstOutput)}, nil
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update guide"}`)}, nil
	}
	a.checks++
	a.checkPrompts = append(a.checkPrompts, opts.Prompt)
	// a.checks counts the gap checks so far, INCLUDING this one.
	if a.editOnCheck && (a.editCalls == 0 || a.checks <= a.editCalls) {
		if err := os.WriteFile(filepath.Join(a.dir, "README.md"), []byte("edited by the gap check\n"), 0o644); err != nil {
			a.t.Fatal(err)
		}
	}
	if a.hideOnCheck {
		gitCmd(a.t, a.dir, "update-index", "--assume-unchanged", "README.md")
	}
	if a.commitOnCheck {
		if err := os.WriteFile(filepath.Join(a.dir, "README.md"), []byte("repaired by the gap check\n"), 0o644); err != nil {
			a.t.Fatal(err)
		}
		gitCmd(a.t, a.dir, "add", "README.md")
		gitCmd(a.t, a.dir, "commit", "-qm", "gap check repair")
	}
	if a.cancelDuringCheck != nil {
		a.cancelDuringCheck()
	}
	if a.failOnCheck {
		return nil, fmt.Errorf("the gap check agent failed")
	}
	return &agent.Result{Output: json.RawMessage(a.gapOutput)}, nil
}

func (a *documentCapAgent) Close() error { return nil }

// advanceHead commits a change and republishes it as the run's head, the way
// a rebase or an ordinary reviewed code fix does.
func advanceHead(t *testing.T, sc *pipelineStepContext, path, content string) string {
	t.Helper()
	writeAndCommit(path, content)(t, sc.dir)
	head := gitCmd(t, sc.dir, "rev-parse", "HEAD")
	sc.sctx.Run.HeadSHA = head
	if err := sc.sctx.DB.UpdateRunHeadSHA(sc.sctx.Run.ID, head); err != nil {
		t.Fatal(err)
	}
	return head
}

type pipelineStepContext struct {
	dir  string
	sctx *pipeline.StepContext
}

// An automatic rebase renames the head without changing the content. Once the
// commit budget is spent that rename used to send the step straight to the
// cap, which parked the run on a finding no agent could resolve. The cap
// must ask instead: the documentation is still accurate, so the step
// completes.
func TestDocumentCapRebaseRenamesTheHeadAndStillCompletes(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, commit: true, gapOutput: `{"findings":[],"summary":"no gap"}`}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1

	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if out.NeedsApproval {
		t.Fatalf("initial pass parked: %+v", out)
	}
	accepted := sc.Run.HeadSHA

	// An automatic rebase replays the same commits onto a new base: the head
	// identity changes and the tree does not.
	gitCmd(t, dir, "commit", "--allow-empty", "-qm", "rebase onto new base")
	rebased := gitCmd(t, dir, "rev-parse", "HEAD")
	if rebased == accepted {
		t.Fatal("fixture did not rename the head")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD^{tree}"); got != gitCmd(t, dir, "rev-parse", accepted+"^{tree}") {
		t.Fatal("fixture changed the content")
	}
	sc.Run.HeadSHA = rebased
	if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, rebased); err != nil {
		t.Fatal(err)
	}

	out, err = (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if out.NeedsApproval {
		t.Fatalf("a rebase parked the Document step: %s", out.Findings)
	}
	if ag.calls != 2 {
		t.Fatalf("the cap did not run the read-only gap check: %d agent calls", ag.calls)
	}
	if len(ag.checkPrompts) != 1 {
		t.Fatalf("want exactly one gap check, got %d", len(ag.checkPrompts))
	}
	// The rebase renamed the head, not the content, so the gap check finds
	// nothing to report.
	if got := gitCmd(t, dir, "rev-list", "--count", rebased+"..HEAD"); got != "0" {
		t.Fatalf("the cap allowed %s commits", got)
	}
}

// An ordinary reviewed code fix after the second Document commit changes the
// head the same way a rebase does. The documentation is still accurate, so the
// step must finish: the cap blocks further COMMITS, not the run.
func TestDocumentCapCodeFixAfterTheBudgetStillCompletesWhenNoGapRemains(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, commit: true, gapOutput: `{"findings":[],"summary":"no gap"}`}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1

	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if out.NeedsApproval {
		t.Fatalf("initial pass parked: %+v", out)
	}
	// Spend the whole budget: the cap is now reached.
	passes, err := sc.DB.GetDocumentPasses(sc.Run.ID)
	if err != nil || len(passes) != 1 || passes[0].CommitCount != 1 {
		t.Fatalf("pass records: %+v, %v", passes, err)
	}

	// The reviewed code fix moves the head and changes the content.
	holder := &pipelineStepContext{dir: dir, sctx: sc}
	advanceHead(t, holder, "feature.txt", "reviewed repair\n")

	out, err = (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if out.NeedsApproval {
		t.Fatalf("the commit cap parked the step with no real gap: %s", out.Findings)
	}
	if ag.calls != 2 {
		t.Fatalf("the read-only gap check did not run: %d agent calls", ag.calls)
	}
	if len(ag.checkPrompts) != 1 {
		t.Fatalf("want exactly one gap check, got %d", len(ag.checkPrompts))
	}
	if !strings.Contains(ag.checkPrompts[0], "gap") {
		t.Fatalf("the gap check prompt does not ask for the remaining gaps: %s", ag.checkPrompts[0])
	}
	// The check is read-only: the cap still blocks a commit.
	if got := gitCmd(t, dir, "rev-list", "--count", sc.Run.HeadSHA+"..HEAD"); got != "0" {
		t.Fatalf("the cap allowed %s commits", got)
	}
}

// A REAL gap at the cap must still park: the cap does not excuse an
// unresolved documentation gap.
func TestDocumentCapRealGapStillParks(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, commit: true, gapOutput: `{"findings":[{"severity":"warning","description":"the rollout guide is still stale","action":"ask-user"}],"summary":"gap remains"}`}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1

	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	holder := &pipelineStepContext{dir: dir, sctx: sc}
	advanceHead(t, holder, "feature.txt", "reviewed repair\n")

	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if !out.NeedsApproval {
		t.Fatalf("a real gap at the cap did not park: %+v", out)
	}
	if !strings.Contains(out.Findings, "the rollout guide is still stale") {
		t.Fatalf("the real gap was lost: %s", out.Findings)
	}
	// The cap notice is informational: it must not be an actionable finding.
	for _, finding := range parseDocumentFindings(t, out.Findings) {
		if strings.Contains(finding.Description, "commit limit") && finding.Action != types.ActionNoOp {
			t.Fatalf("the cap notice is actionable and would deadlock strict shipping: %+v", finding)
		}
	}
}

// The cap notice must never be an unapprovable actionable finding under
// strict shipping: strict shipping refuses an approval while any actionable
// finding is unresolved, so an actionable cap notice is a deadlock.
func TestDocumentCapNoticeIsNeverActionableUnderStrictShipping(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, commit: true, gapOutput: `{"findings":[],"summary":"no gap"}`}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1
	sc.Run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull}

	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	holder := &pipelineStepContext{dir: dir, sctx: sc}
	advanceHead(t, holder, "feature.txt", "reviewed repair\n")

	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range parseDocumentFindings(t, out.Findings) {
		if strings.Contains(finding.Description, "commit limit") && types.HasActionableFindings(types.Findings{Items: []types.Finding{finding}}) {
			t.Fatalf("the cap notice is an actionable finding: %+v", finding)
		}
	}
	// A strict run's parked row must be approvable: the refusal is what
	// closed every exit in issue #123.
	row := parkDocumentStepRow(t, sc, out)
	if refusal := row.StrictApprovalRefusal(""); refusal != "" {
		t.Fatalf("strict shipping refuses the Document gate: %s", refusal)
	}
}

// The zero budget is the same cap: Document still checks for gaps and
// completes when there are none.
func TestDocumentCapZeroBudgetStillChecksForGaps(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, gapOutput: `{"findings":[],"summary":"no gap"}`}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 0

	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if out.NeedsApproval {
		t.Fatalf("a zero budget parked a run with no real gap: %s", out.Findings)
	}
	if ag.calls != 1 {
		t.Fatalf("the zero budget did not run the gap check: %d agent calls", ag.calls)
	}
}

// The gap check's OWN verdict is the authority on the CURRENT gaps.
//
// An earlier pass's finding is not re-appended: the check reads the current
// state, so a gap it does not re-report is either gone (a later repair
// removed what needed the documentation) or never was. Carrying the old one
// forward would park a run whose documentation is accurate - the very
// deadlock this change removes.
func TestDocumentCapGapCheckIsTheAuthorityOnCurrentGaps(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, commit: true, gapOutput: `{"findings":[],"summary":"no gap"}`}
	ag.firstOutput = `{"findings":[{"severity":"warning","description":"earlier gap still open","action":"ask-user"}],"summary":"gap remains"}`
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1

	// The first pass reports a gap AND commits, so the budget is spent and a
	// gap is outstanding.
	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if !out.NeedsApproval || !strings.Contains(out.Findings, "earlier gap still open") {
		t.Fatalf("the first pass did not park with its gap: %+v", out)
	}
	holder := &pipelineStepContext{dir: dir, sctx: sc}
	advanceHead(t, holder, "feature.txt", "reviewed repair\n")

	// The check now reports no gap: the earlier one does not survive it.
	out, err = (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if out.NeedsApproval {
		t.Fatalf("an obsolete earlier gap was carried forward: %+v", out)
	}
	if strings.Contains(out.Findings, "earlier gap still open") {
		t.Fatalf("the obsolete gap was re-appended: %s", out.Findings)
	}
}

// The gap check is a DOCUMENT review: it must not drop a finding because of
// its category. The combined pass's lint half belongs to the lint step, but a
// finding the CHECK reported is a documentation gap, and dropping it would
// park nothing and report nothing.
func TestDocumentCapGapCheckKeepsEveryReportedFinding(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, commit: true, gapOutput: `{"findings":[{"severity":"warning","description":"a categorized gap","action":"ask-user","category":"lint"}],"summary":"gap remains"}`}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1

	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	holder := &pipelineStepContext{dir: dir, sctx: sc}
	advanceHead(t, holder, "feature.txt", "reviewed repair\n")

	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if !out.NeedsApproval {
		t.Fatalf("the gap check dropped a reported finding: %+v", out)
	}
	if !strings.Contains(out.Findings, "a categorized gap") {
		t.Fatalf("the reported finding was filtered out: %s", out.Findings)
	}
}

// A gap the check DOES re-report stays visible and decidable.
func TestDocumentCapGapCheckReReportedGapStillParks(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, commit: true, gapOutput: `{"findings":[{"severity":"warning","description":"still stale","action":"ask-user"}],"summary":"gap remains"}`}
	ag.firstOutput = `{"findings":[{"severity":"warning","description":"still stale","action":"ask-user"}],"summary":"gap remains"}`
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1

	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	holder := &pipelineStepContext{dir: dir, sctx: sc}
	advanceHead(t, holder, "feature.txt", "reviewed repair\n")

	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if !out.NeedsApproval {
		t.Fatalf("a re-reported gap did not park: %+v", out)
	}
	if !strings.Contains(out.Findings, "still stale") {
		t.Fatalf("the re-reported gap was lost: %s", out.Findings)
	}
}

// A gap check that EDITS is not honored: the cap still bounds the commits, and
// the check's own proposal is rolled back.
//
// A check that edits and then reports no gap produced its verdict about bytes
// that no longer exist, so the step must NOT complete on it: it would finish
// with the ORIGINAL stale documentation still in place. The restored bytes
// are reassessed instead.
func TestDocumentCapDiscardsAGapCheckThatEdits(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, commit: true, gapOutput: `{"findings":[],"summary":"no gap"}`}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1

	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	holder := &pipelineStepContext{dir: dir, sctx: sc}
	advanceHead(t, holder, "feature.txt", "reviewed repair\n")
	accepted := sc.Run.HeadSHA

	// The gap check ignores its instructions and edits anyway. It edits on
	// EVERY call, so the bounded reassessment fails the step closed rather
	// than looping.
	ag.editOnCheck = true
	_, err := (&DocumentStep{}).Execute(sc)
	if err == nil {
		t.Fatal("a gap check that edited twice completed the step")
	}
	if !strings.Contains(err.Error(), "edited the worktree twice") {
		t.Fatalf("unexpected refusal: %v", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != accepted {
		t.Fatalf("the gap check advanced the head: %s", got)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("the gap check left edits behind: %s", got)
	}
	if got := gitCmd(t, dir, "show", "HEAD:README.md"); got != "guide" {
		t.Fatalf("the gap check changed the accepted documentation: %q", got)
	}
}

// A gap check that edits and then FAILS must not leave its proposal in the
// worktree: the budget is exhausted, and a surviving commit would also break
// the head continuity of every later attempt.
func TestDocumentCapRollsBackAGapCheckThatFails(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, commit: true, gapOutput: `{"findings":[],"summary":"no gap"}`}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1

	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	holder := &pipelineStepContext{dir: dir, sctx: sc}
	advanceHead(t, holder, "feature.txt", "reviewed repair\n")
	accepted := sc.Run.HeadSHA

	// The check edits and then fails.
	ag.editOnCheck = true
	ag.failOnCheck = true
	_, err := (&DocumentStep{}).Execute(sc)
	if err == nil {
		t.Fatal("a failed gap check completed the step")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != accepted {
		t.Fatalf("the failed gap check advanced the head: %s", got)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("the failed gap check left edits behind: %s", got)
	}
	if got := gitCmd(t, dir, "show", "HEAD:README.md"); got != "guide" {
		t.Fatalf("the failed gap check changed the accepted documentation: %q", got)
	}
}

// A gap check that COMMITS and then fails must not leave its commit behind:
// the budget is exhausted, and a surviving commit would also break the head
// continuity of every later attempt.
func TestDocumentCapRollsBackAGapCheckThatCommitsAndFails(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, commit: true, gapOutput: `{"findings":[],"summary":"no gap"}`}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1

	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	holder := &pipelineStepContext{dir: dir, sctx: sc}
	advanceHead(t, holder, "feature.txt", "reviewed repair\n")
	accepted := sc.Run.HeadSHA

	// The check commits its repair and then fails.
	ag.commitOnCheck = true
	ag.failOnCheck = true
	_, err := (&DocumentStep{}).Execute(sc)
	if err == nil {
		t.Fatal("a failed gap check completed the step")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != accepted {
		t.Fatalf("the failed gap check left its commit behind: %s", got)
	}
	if got := gitCmd(t, dir, "rev-list", "--count", accepted+"..HEAD"); got != "0" {
		t.Fatalf("the failed gap check left %s commits", got)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("the failed gap check left edits behind: %s", got)
	}
	if got := gitCmd(t, dir, "show", "HEAD:README.md"); got != "guide" {
		t.Fatalf("the failed gap check changed the accepted documentation: %q", got)
	}
}

// The rollback must survive a CANCELLED run context: a daemon shutdown
// cancels the run, and a cancelled context would fail every Git call of the
// rollback - leaving the over-budget commit behind and breaking the
// recorded-head continuity of every later attempt.
func TestDocumentCapRollbackSurvivesACancelledRunContext(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, commit: true, gapOutput: `{"findings":[],"summary":"no gap"}`}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1

	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	holder := &pipelineStepContext{dir: dir, sctx: sc}
	advanceHead(t, holder, "feature.txt", "reviewed repair\n")
	accepted := sc.Run.HeadSHA

	// The check commits its repair, and the run is cancelled WHILE it runs -
	// the daemon shutdown timing the review described.
	ag.commitOnCheck = true
	ag.failOnCheck = true
	cancelled, cancel := context.WithCancel(sc.Ctx)
	sc.Ctx = cancelled
	ag.cancelDuringCheck = cancel
	_, err := (&DocumentStep{}).Execute(sc)
	if err == nil {
		t.Fatal("a failed gap check completed the step")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != accepted {
		t.Fatalf("the cancelled rollback left the commit behind: %s", got)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("the cancelled rollback left edits behind: %s", got)
	}
}

// A gap check that HIDES a file behind an index flag and then edits it must
// not have its verdict accepted: Git status cannot see a --assume-unchanged
// or --skip-worktree entry, so the candidate discovery would find nothing
// and the step would complete with the committed documentation still stale.
func TestDocumentCapRefusesAGapCheckThatHidesTheIndex(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, commit: true, gapOutput: `{"findings":[],"summary":"no gap"}`}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1

	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	holder := &pipelineStepContext{dir: dir, sctx: sc}
	advanceHead(t, holder, "feature.txt", "reviewed repair\n")

	// The check hides README.md from status and then edits it.
	ag.hideOnCheck = true
	ag.editOnCheck = true
	_, err := (&DocumentStep{}).Execute(sc)
	if err == nil {
		t.Fatal("a gap check that hid the index completed the step")
	}
	if !strings.Contains(err.Error(), "hidden from status by index flags") {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

// A check that edits ONCE and then reports the gap on the restored bytes is
// honored: the second verdict describes the current state.
func TestDocumentCapReassessesTheRestoredBytesAfterAnEdit(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, commit: true, gapOutput: `{"findings":[{"severity":"warning","description":"the restored guide is still stale","action":"ask-user"}],"summary":"gap remains"}`}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1

	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	holder := &pipelineStepContext{dir: dir, sctx: sc}
	advanceHead(t, holder, "feature.txt", "reviewed repair\n")
	accepted := sc.Run.HeadSHA

	// The check edits on its FIRST call only.
	ag.editOnCheck = true
	ag.editCalls = 1
	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if !out.NeedsApproval {
		t.Fatalf("the reassessed gap did not park: %+v", out)
	}
	if !strings.Contains(out.Findings, "the restored guide is still stale") {
		t.Fatalf("the reassessed gap was lost: %s", out.Findings)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != accepted {
		t.Fatalf("the gap check advanced the head: %s", got)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("the gap check left edits behind: %s", got)
	}
	if ag.calls != 3 {
		t.Fatalf("the check ran %d times, want 3 (one pass, one check, one reassessment)", ag.calls)
	}
}

// The gap check's prompt must be internally consistent: a read-only check
// that still tells the agent to FIX things will fix them, and the pipeline
// will have to roll the edits back.
func TestDocumentCapGapCheckPromptIsReadOnly(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentCapAgent{t: t, dir: dir, gapOutput: `{"findings":[],"summary":"no gap"}`}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 0
	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	if len(ag.checkPrompts) != 1 {
		t.Fatalf("want one gap check, got %d", len(ag.checkPrompts))
	}
	prompt := ag.checkPrompts[0]
	for _, want := range []string{
		"READ-ONLY check",
		"Do not edit anything. This is a read-only check.",
		"you may NOT create any commit and you may NOT edit any file",
		"Do not fix anything - this pass only decides whether a gap remains",
		"Report ONLY the documentation gaps that remain unresolved",
		"return an empty findings array",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the gap check prompt is missing %q", want)
		}
	}
	// The repair instructions of the normal pass must be GONE, not merely
	// contradicted by an addition.
	for _, unwanted := range []string{
		"Leave edits uncommitted",
		"3. Fix in the authoritative location",
		"Update each altered fact in its owner document",
		"Remove stale duplicates or reduce them to a short pointer to the owner",
		"Re-read what you changed to verify it now reflects the code",
	} {
		if strings.Contains(prompt, unwanted) {
			t.Errorf("the gap check prompt still carries the repair instruction %q", unwanted)
		}
	}
}

func parseDocumentFindings(t *testing.T, raw string) []types.Finding {
	t.Helper()
	var findings Findings
	if err := json.Unmarshal([]byte(raw), &findings); err != nil {
		t.Fatal(err)
	}
	return findings.Items
}

// parkDocumentStepRow persists the outcome the way the executor does, so the
// strict admission check sees the same row a real run would.
func parkDocumentStepRow(t *testing.T, sc *pipeline.StepContext, out *pipeline.StepOutcome) *db.StepResult {
	t.Helper()
	row, err := sc.DB.InsertStepResult(sc.Run.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.DB.ParkStepForApproval(sc.Run.ID, row.ID, types.StepStatusFixReview, 0, 0, &out.Findings, ""); err != nil {
		t.Fatal(err)
	}
	persisted, err := sc.DB.GetStepResult(row.ID)
	if err != nil || persisted == nil {
		t.Fatalf("parked step row: %+v, %v", persisted, err)
	}
	return persisted
}
