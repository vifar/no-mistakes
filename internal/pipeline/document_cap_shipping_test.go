package pipeline

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The Document commit cap used to be an unapprovable actionable finding, so
// strict shipping refused the gate ("strict shipping cannot approve
// unresolved actionable findings") and the run could never finish
// (issue #123).
//
// These are the executor-level proofs. The step-level tests in
// internal/pipeline/steps own the Document step's own behavior; these own
// what the executor and the strict admission do with its outcome.
const documentCapNoticeFindings = `{"findings":[` +
	`{"id":"document-1","severity":"info","description":"Document commit limit reached; further documentation edits require a new run or an owner decision.","action":"no-op"}],` +
	`"summary":"Document commit limit reached; no remaining documentation gap"}`

// The cap notice alone must not park a gate and must not block an approval.
// The notice is INFORMATIONAL: there is nothing to decide about it, and an
// actionable notice would deadlock strict shipping forever.
func TestStrictShippingDocumentCapNoticeAloneNeverParks(t *testing.T) {
	database, p, baseRun, repo := setupTest(t)
	workDir := completionFixture(t, database, baseRun)
	run, err := database.InsertRunWithShipping(repo.ID, "feature", baseRun.HeadSHA, baseRun.HeadSHA, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull})
	if err != nil {
		t.Fatal(err)
	}
	bindCompletionFixture(t, database, run, workDir)

	document := &adaptiveCallStep{name: types.StepDocument, fn: func(*StepContext) (*StepOutcome, error) {
		return &StepOutcome{NeedsApproval: false, Findings: documentCapNoticeFindings}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{document}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)
	waitExecutorDone(t, done)
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunCompleted {
		t.Fatalf("the strict run did not finish: %+v", got)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepDocument && step.Status != types.StepStatusCompleted {
			t.Fatalf("the Document step did not complete: %+v", step)
		}
	}
}

// The cap notice must not be counted among the blocking findings the
// executor looks for when it decides whether a gate is real.
func TestDocumentCapNoticeIsNotABlockingFinding(t *testing.T) {
	if hasBlockingFindingsAtStepJSON(documentCapNoticeFindings, types.StepDocument) {
		t.Fatal("the informational cap notice counts as a blocking finding")
	}
	if hasAskUserFindingsJSON(documentCapNoticeFindings) {
		t.Fatal("the informational cap notice counts as an ask-user finding")
	}
}

// A REAL gap at the cap still parks, and strict shipping still refuses to
// approve it - but the REFUSAL must come from the real gap, never from the
// cap notice. The cap notice must not be counted among the actionable
// findings, or the gate would deadlock exactly like issue #123.
func TestStrictShippingDocumentCapRealGapStillParksAndTheCapNoticeIsNotTheBlocker(t *testing.T) {
	database, p, baseRun, repo := setupTest(t)
	workDir := completionFixture(t, database, baseRun)
	run, err := database.InsertRunWithShipping(repo.ID, "feature", baseRun.HeadSHA, baseRun.HeadSHA, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull})
	if err != nil {
		t.Fatal(err)
	}
	bindCompletionFixture(t, database, run, workDir)

	const findings = `{"findings":[` +
		`{"id":"document-1","severity":"info","description":"Document commit limit reached; further documentation edits require a new run or an owner decision.","action":"no-op"},` +
		`{"id":"document-2","severity":"warning","file":"README.md","description":"the rollout guide is still stale","action":"ask-user"}],` +
		`"summary":"Document commit limit reached; stopping further commits"}`
	document := &adaptiveCallStep{name: types.StepDocument, fn: func(*StepContext) (*StepOutcome, error) {
		return &StepOutcome{NeedsApproval: true, Findings: findings}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{document}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)

	waitForStepStatus(t, database, run.ID, types.StepDocument, types.StepStatusAwaitingApproval)
	exec.mu.Lock()
	gate := exec.waitingFindings
	exec.mu.Unlock()
	parsed, err := types.ParseFindingsJSON(gate)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 2 {
		t.Fatalf("the gate lost a finding: %s", gate)
	}
	for _, item := range parsed.Items {
		if strings.Contains(item.Description, "commit limit") && types.HasActionableFindings(types.Findings{Items: []types.Finding{item}}) {
			t.Fatalf("the cap notice is an actionable finding: %+v", item)
		}
	}
	// The real gap still blocks the approval - that is the point of a gap.
	if err := exec.Respond(types.StepDocument, types.ActionApprove, nil, gateSnapshot(exec)); err == nil || !strings.Contains(err.Error(), "unresolved actionable findings") {
		t.Fatalf("the real gap did not block the strict approval: %v", err)
	}
	if err := exec.Respond(types.StepDocument, types.ActionAbort, nil, nil); err != nil {
		t.Fatal(err)
	}
	<-done
}

// The strict refusal names the actionable findings, so a cap notice must not
// appear among them.
func TestDocumentCapNoticeIsNotCountedAsActionable(t *testing.T) {
	findings, err := types.ParseFindingsJSON(documentCapNoticeFindings)
	if err != nil {
		t.Fatal(err)
	}
	if types.HasActionableFindings(findings) {
		t.Fatal("the informational cap notice counts as actionable")
	}
	notice := documentCapNoticeFindings
	row := &db.StepResult{StepName: types.StepDocument, FindingsJSON: &notice}
	if refusal := row.StrictApprovalRefusal(""); refusal != "" {
		t.Fatalf("strict shipping refuses the informational cap notice: %s", refusal)
	}
}
