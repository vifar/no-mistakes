package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The end-to-end proof for issue #123: a STRICT run whose Document step
// reaches its commit cap and finds no real gap must FINISH, and the gate it
// would have parked on must be approvable.
//
// The deadlock this replaces: the cap emitted an `ask-user` finding, strict
// shipping refused the approval ("strict shipping cannot approve unresolved
// actionable findings"), and the gate's other exits were closed too, so the
// run could never finish.
func TestDocumentCapStrictRunFinishesWhenNoGapRemains(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			// The first pass commits a documentation repair, spending the
			// whole budget.
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("guide\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update guide"}`)}, nil
		}
		// The read-only gap check finds nothing.
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"no gap"}`)}, nil
	}}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1
	sc.Run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull}

	// The first pass spends the budget.
	if out, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	} else if out.NeedsApproval {
		t.Fatalf("the first pass parked: %+v", out)
	}

	// A reviewed code fix moves the head.
	writeAndCommit("feature.txt", "reviewed repair\n")(t, dir)
	moved := gitCmd(t, dir, "rev-parse", "HEAD")
	sc.Run.HeadSHA = moved
	if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, moved); err != nil {
		t.Fatal(err)
	}

	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if out.NeedsApproval {
		t.Fatalf("the cap parked a strict run with no real gap: %s", out.Findings)
	}
	// The step's own row must be approvable by strict shipping.
	row, err := sc.DB.InsertStepResult(sc.Run.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}
	findings := out.Findings
	if err := sc.DB.ParkStepForApproval(sc.Run.ID, row.ID, types.StepStatusFixReview, 0, 0, &findings); err != nil {
		t.Fatal(err)
	}
	persisted, err := sc.DB.GetStepResult(row.ID)
	if err != nil || persisted == nil {
		t.Fatalf("parked row: %+v, %v", persisted, err)
	}
	if refusal := persisted.StrictApprovalRefusal(""); refusal != "" {
		t.Fatalf("strict shipping refuses the Document gate: %s", refusal)
	}
}

// The same journey through the EXECUTOR: a strict run reaches the Document
// cap, the step completes instead of parking, and the run finishes.
func TestDocumentCapStrictExecutorRunFinishes(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("guide\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update guide"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"no gap"}`)}, nil
	}}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1
	sc.Run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull}

	// Spend the budget before the executor runs, so the Document step starts
	// at the cap.
	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	// A reviewed code fix moves the head, so the exact-head reuse misses and
	// the step reaches the cap.
	writeAndCommit("feature.txt", "reviewed repair\n")(t, dir)
	moved := gitCmd(t, dir, "rev-parse", "HEAD")
	sc.Run.HeadSHA = moved
	if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, moved); err != nil {
		t.Fatal(err)
	}

	var documentCalls int
	steps := []pipeline.Step{
		&transitionStep{name: types.StepDocument, execute: func(c *pipeline.StepContext) (*pipeline.StepOutcome, error) {
			documentCalls++
			return (&DocumentStep{}).Execute(c)
		}},
	}
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	completed := false
	executor := pipeline.NewExecutor(sc.DB, p, sc.Config, ag, steps, func(event ipc.Event) {
		if event.Type == ipc.EventRunCompleted && event.Status != nil && *event.Status == string(types.RunCompleted) {
			completed = true
		}
	})
	executor.SetPRContextGuard(func(c *pipeline.StepContext, _ types.StepName) (pipeline.PRContextDecision, error) {
		candidate, err := testgit.RunComparisonCandidate(c.Run, dir, "main", base, "", "test/repo", "feature")
		if err != nil {
			return pipeline.PRContextDecision{}, err
		}
		_, err = c.DB.AdvanceRunPRContext(c.Run.ID, candidate)
		return pipeline.PRContextDecision{Target: pipeline.PRTargetSelection{SourceRepo: "test/repo", SourceBranch: "feature", TargetBranch: "main"}}, err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- executor.Execute(ctx, sc.Run, sc.Repo, dir) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the strict run failed: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the strict run did not finish")
	}
	if !completed {
		t.Fatal("the strict run did not complete")
	}
	if documentCalls != 1 {
		t.Fatalf("the Document step ran %d times, want 1", documentCalls)
	}
	run, err := sc.DB.GetRun(sc.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunCompleted {
		t.Fatalf("the strict run status: %s", run.Status)
	}
	// The step must have completed, never parked.
	rows, err := sc.DB.GetStepsByRun(sc.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.StepName != types.StepDocument {
			continue
		}
		if row.Status != types.StepStatusCompleted {
			t.Fatalf("the Document step parked in a strict run: %+v", row)
		}
		if row.FindingsJSON != nil {
			items := parseDocumentFindings(t, *row.FindingsJSON)
			if len(items) != 0 {
				t.Fatalf("a completing step recorded findings: %s", *row.FindingsJSON)
			}
		}
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("the run left a dirty worktree: %s", got)
	}
}

// The gap check must ASK the agent, not decide on its own: a real gap at the
// cap still parks, with the gap as the actionable finding and the cap notice
// as the informational one.
func TestDocumentCapStrictRunStillParksOnARealGap(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("guide\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update guide"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"the rollout guide is still stale","action":"ask-user"}],"summary":"gap remains"}`)}, nil
	}}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1
	sc.Run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull}

	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	writeAndCommit("feature.txt", "reviewed repair\n")(t, dir)
	moved := gitCmd(t, dir, "rev-parse", "HEAD")
	sc.Run.HeadSHA = moved
	if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, moved); err != nil {
		t.Fatal(err)
	}

	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if !out.NeedsApproval {
		t.Fatalf("a real gap at the cap did not park: %+v", out)
	}
	items := parseDocumentFindings(t, out.Findings)
	var gap, notice types.Finding
	for _, item := range items {
		switch {
		case strings.Contains(item.Description, "the rollout guide is still stale"):
			gap = item
		case strings.Contains(item.Description, "commit limit"):
			notice = item
		}
	}
	if gap.Description == "" {
		t.Fatalf("the real gap was lost: %s", out.Findings)
	}
	if gap.Action != types.ActionAskUser {
		t.Fatalf("the real gap is not actionable: %+v", gap)
	}
	if notice.Description == "" {
		t.Fatalf("the cap notice is missing: %s", out.Findings)
	}
	if notice.Action != types.ActionNoOp {
		t.Fatalf("the cap notice is actionable and would deadlock strict shipping: %+v", notice)
	}
	// The strict admission must still refuse - because of the REAL gap, not
	// because of the cap notice.
	row, err := sc.DB.InsertStepResult(sc.Run.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}
	findings := out.Findings
	if err := sc.DB.ParkStepForApproval(sc.Run.ID, row.ID, types.StepStatusFixReview, 0, 0, &findings); err != nil {
		t.Fatal(err)
	}
	persisted, err := sc.DB.GetStepResult(row.ID)
	if err != nil || persisted == nil {
		t.Fatalf("parked row: %+v, %v", persisted, err)
	}
	refusal := persisted.StrictApprovalRefusal("")
	if refusal == "" {
		t.Fatal("the real gap did not block the strict approval")
	}
	if !strings.Contains(refusal, "unresolved actionable findings") {
		t.Fatalf("the refusal names the wrong cause: %s", refusal)
	}
}
