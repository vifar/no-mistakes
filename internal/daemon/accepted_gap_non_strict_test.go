package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #158: the accepted Test gap used to be refused on every non-strict
// run, so a repository that declares no_ci - a repository with no external
// check to fall back on - could not finish a run whose Test verdict is
// inconclusive. These tests pin the non-strict path and prove that the four
// callers of the eligibility rule stay in step.

const (
	nonStrictGapReason    = "owner approved via first mate, 2026-10-21"
	nonStrictGapCoveredBy = "TestBrokerTimeoutMocked; internal/broker/timeout_test.go"
)

// parkedNonStrictTest is a non-strict run whose Test step is parked on an
// inconclusive verdict, with the manager and the executor wired to it.
type parkedNonStrictTest struct {
	database *db.DB
	manager  *RunManager
	executor *pipeline.Executor
	run      *db.Run
	parked   *db.StepResult
	cancel   context.CancelFunc
	done     chan error
	final    *ipc.Event
}

func parkNonStrictTest(t *testing.T) *parkedNonStrictTest {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, head := setupTestGitRepo(t, p, database, "accepted-gap-non-strict")
	run, err := database.InsertRunWithShipping(repo.ID, "feature", head, head, nil, "", "", "", "main", false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.Shipping != nil {
		t.Fatal("the run is not a plain run")
	}
	// A plain run completes against the PR comparison, so the fixture binds
	// one the way the pipeline does.
	target := gitOutput(t, repo.WorkingPath, "rev-parse", "--abbrev-ref", "HEAD")
	testgit.BindRunComparison(t, database, run, repo.WorkingPath, target, "https://github.com/test/repo/pull/1", "test/repo", "feature")
	raw := inconclusiveGapEvidence
	gateReady := make(chan struct{}, 1)
	holder := &parkedNonStrictTest{database: database, done: make(chan error, 1)}
	executor := pipeline.NewExecutor(database, p, nil, nil, []pipeline.Step{&noSurfaceApprovalStep{findings: raw}}, func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepTest && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			select {
			case gateReady <- struct{}{}:
			default:
			}
		}
		if event.Type == ipc.EventRunCompleted {
			final := event
			holder.final = &final
		}
	})
	manager := NewRunManager(database, p, nil)
	manager.runs[run.ID] = &managedRun{run: run, executor: executor}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	holder.cancel = cancel
	holder.manager = manager
	holder.executor = executor
	holder.run = run
	go func() {
		defer close(holder.done)
		holder.done <- executor.Execute(ctx, run, repo, repo.WorkingPath)
	}()
	t.Cleanup(func() {
		cancel()
		for range holder.done {
		}
	})
	select {
	case <-gateReady:
	case err := <-holder.done:
		t.Fatalf("executor exited before the gate: %v", err)
	case <-ctx.Done():
		t.Fatal("the Test step did not park")
	}
	rows, err := database.GetStepsByRun(run.ID)
	if err != nil || len(rows) != 1 || rows[0].Status != types.StepStatusAwaitingApproval {
		t.Fatalf("parked rows = %+v, %v", rows, err)
	}
	holder.parked = rows[0]
	return holder
}

// A non-strict run admits an owner-approved gap, the run finishes, and the
// audit is stored verbatim.
func TestNonStrictAcceptedGapCompletesTheRun(t *testing.T) {
	h := parkNonStrictTest(t)
	gaps := &db.AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: nonStrictGapReason, CoveredBy: nonStrictGapCoveredBy}
	if _, err := h.manager.HandleRespondWithAcceptedGaps(h.run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, nonStrictGapReason, gaps, managerGateSnapshot(t, h.manager, h.run.ID, types.StepTest)); err != nil {
		t.Fatalf("a non-strict run refused an owner-approved gap: %v", err)
	}
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
	row, err := h.database.GetStepResult(h.parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != types.StepStatusCompleted || row.ApprovalReason != nil || row.OverrideReason != nil || row.SkipReason != nil {
		t.Fatalf("accepted gap row = %+v", row)
	}
	recorded, err := row.RecordedAcceptedGaps()
	if err != nil || recorded == nil {
		t.Fatalf("recorded gaps = %+v, %v", recorded, err)
	}
	if recorded.Reason != nonStrictGapReason || recorded.CoveredBy != nonStrictGapCoveredBy {
		t.Fatalf("recorded gap = %+v, want the audit stored verbatim", recorded)
	}
	if strings.Join(recorded.FindingIDs, ",") != "test-verdict" || recorded.Verdict != types.TestVerdictInconclusive || recorded.AcceptedAt == "" {
		t.Fatalf("recorded gap = %+v", recorded)
	}
	if !strings.Contains(row.TestOverrideReason(), "Test gap accepted (test-verdict)") {
		t.Fatalf("TestOverrideReason = %q", row.TestOverrideReason())
	}
	if h.final == nil || h.final.Status == nil || *h.final.Status != string(types.RunCompleted) {
		t.Fatalf("completion event = %+v", h.final)
	}
}

// The callers of the eligibility rule agree: the manager's pre-check, the
// executor's live gate and the durable recording refuse exactly the same
// requests on a non-strict run, and they agree with the shared rule itself.
func TestNonStrictAcceptedGapCallersAgree(t *testing.T) {
	admissible := db.AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: nonStrictGapReason, CoveredBy: nonStrictGapCoveredBy}
	unknownID := db.AcceptedGaps{FindingIDs: []string{"missing"}, Reason: nonStrictGapReason, CoveredBy: nonStrictGapCoveredBy}
	shortReason := db.AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: "ok", CoveredBy: nonStrictGapCoveredBy}
	noCoveredBy := db.AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: nonStrictGapReason}
	cases := []struct {
		name    string
		gaps    db.AcceptedGaps
		refused bool
	}{
		{"admissible", admissible, false},
		{"unknown finding", unknownID, true},
		{"short reason", shortReason, true},
		{"no covered-by", noCoveredBy, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			// The shared rule, on a parked row.
			ruleRun := parkNonStrictTest(t)
			rule := ruleRun.parked.AcceptedGapRefusal(false, tt.gaps) != ""
			// The manager's pre-check, which then dispatches to the executor.
			_, managerErr := ruleRun.manager.HandleRespondWithAcceptedGaps(ruleRun.run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, tt.gaps.Reason, &tt.gaps, managerGateSnapshot(t, ruleRun.manager, ruleRun.run.ID, types.StepTest))
			managerRefused := managerErr != nil
			// The executor's live gate, on its own parked gate.
			liveRun := parkNonStrictTest(t)
			_, liveErr := liveRun.executor.RespondWithAcceptedGaps(types.StepTest, types.ActionApprove, nil, nil, nil, nil, tt.gaps.Reason, &tt.gaps, gateSnapshot(liveRun.executor))
			live := liveErr != nil
			// The durable recording, on its own parked gate.
			recordRun := parkNonStrictTest(t)
			_, recordErr := recordRun.database.RecordAcceptedGaps(recordRun.parked.ID, tt.gaps, false)
			recorded := recordErr != nil
			if rule != tt.refused || recorded != tt.refused || live != tt.refused || managerRefused != tt.refused {
				t.Fatalf("the callers disagree: rule=%v recorded=%v live=%v manager=%v, want refused=%v (record err %v, live err %v, manager err %v)", rule, recorded, live, managerRefused, tt.refused, recordErr, liveErr, managerErr)
			}
		})
	}
}

// Strict shipping's behaviour is unchanged: a plain approve, a Test exception
// reason and an unknown finding are all still refused, and the gap is still
// admitted.
func TestStrictAcceptedGapBehaviourIsUnchanged(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, head := setupTestGitRepo(t, p, database, "accepted-gap-strict-unchanged")
	run, err := database.InsertRunWithShipping(repo.ID, "feature", head, head, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull})
	if err != nil {
		t.Fatal(err)
	}
	raw := inconclusiveGapEvidence
	gateReady := make(chan struct{}, 1)
	var final ipc.Event
	executor := pipeline.NewExecutor(database, p, nil, nil, []pipeline.Step{&noSurfaceApprovalStep{findings: raw}}, func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepTest && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			gateReady <- struct{}{}
		}
		if event.Type == ipc.EventRunCompleted {
			final = event
		}
	})
	manager := NewRunManager(database, p, nil)
	manager.runs[run.ID] = &managedRun{run: run, executor: executor}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- executor.Execute(ctx, run, repo, repo.WorkingPath)
	}()
	defer func() {
		cancel()
		for range done {
		}
	}()
	select {
	case <-gateReady:
	case err := <-done:
		t.Fatalf("executor exited before the gate: %v", err)
	case <-ctx.Done():
		t.Fatal("the Test step did not park")
	}
	rows, err := database.GetStepsByRun(run.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("parked rows = %+v, %v", rows, err)
	}
	parked := rows[0]
	gaps := &db.AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: nonStrictGapReason, CoveredBy: nonStrictGapCoveredBy}
	refused := func(label string, call func() error) {
		t.Helper()
		if err := call(); err == nil {
			t.Fatalf("%s was admitted", label)
		}
		got, err := database.GetStepResult(parked.ID)
		if err != nil || got.Status != types.StepStatusAwaitingApproval || got.AcceptedGapsJSON != nil || got.ApprovalReason != nil {
			t.Fatalf("%s changed the parked gate: %+v, %v", label, got, err)
		}
	}
	refused("a plain strict approve", func() error {
		_, err := manager.HandleRespondWithOverrides(run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, "", managerGateSnapshot(t, manager, run.ID, types.StepTest))
		return err
	})
	refused("a strict approve with a Test exception reason", func() error {
		_, err := manager.HandleRespondWithOverrides(run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, nonStrictGapReason, managerGateSnapshot(t, manager, run.ID, types.StepTest))
		return err
	})
	refused("an unknown finding", func() error {
		_, err := manager.HandleRespondWithAcceptedGaps(run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, nonStrictGapReason, &db.AcceptedGaps{FindingIDs: []string{"missing"}, Reason: nonStrictGapReason, CoveredBy: nonStrictGapCoveredBy}, managerGateSnapshot(t, manager, run.ID, types.StepTest))
		return err
	})
	refused("a gap on a fix action", func() error {
		_, err := manager.HandleRespondWithAcceptedGaps(run.ID, types.StepTest, types.ActionFix, []string{"test-verdict"}, nil, nil, nil, nonStrictGapReason, gaps, managerGateSnapshot(t, manager, run.ID, types.StepTest))
		return err
	})
	// The strict gap is still admitted, and the strict admission rules still
	// hold: a plain approve of the same step is still refused.
	if _, err := manager.HandleRespondWithAcceptedGaps(run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, nonStrictGapReason, gaps, managerGateSnapshot(t, manager, run.ID, types.StepTest)); err != nil {
		t.Fatalf("a strict run refused an owner-approved gap: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	row, err := database.GetStepResult(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != types.StepStatusCompleted || row.ValidAcceptedGaps() == nil || row.ApprovalReason != nil || row.OverrideReason != nil {
		t.Fatalf("strict accepted gap row = %+v", row)
	}
	if final.Status == nil || *final.Status != string(types.RunCompleted) {
		t.Fatalf("completion event = %+v", final)
	}
}
