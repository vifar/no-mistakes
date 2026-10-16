package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func queuedRunForTest(t *testing.T, d *DB, repoID, branch string) *Run {
	t.Helper()
	run, err := d.InsertRun(repoID, branch, "head-"+branch, "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRunQueued(run.ID, `{"trigger":"push"}`); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestQueuedRunsOrderByQueuedAtThenID(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	a := queuedRunForTest(t, d, repo.ID, "a")
	b := queuedRunForTest(t, d, repo.ID, "b")
	c := queuedRunForTest(t, d, repo.ID, "c")

	ids, err := d.QueuedRunIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != a.ID || ids[1] != b.ID || ids[2] != c.ID {
		t.Fatalf("queue order = %v, want [%s %s %s]", ids, a.ID, b.ID, c.ID)
	}
	// Equal timestamps fall back to id order (ids are monotonic).
	if _, err := d.sql.Exec(`UPDATE runs SET queued_at = 7`); err != nil {
		t.Fatal(err)
	}
	ids, _ = d.QueuedRunIDs()
	if ids[0] != a.ID || ids[1] != b.ID || ids[2] != c.ID {
		t.Fatalf("tie-break order = %v, want id order", ids)
	}
	if pos, total, err := d.QueuePosition(b.ID); err != nil || pos != 2 || total != 3 {
		t.Fatalf("QueuePosition(b) = %d of %d, %v; want 2 of 3", pos, total, err)
	}
	if pos, total, _ := d.QueuePosition("missing"); pos != 0 || total != 3 {
		t.Fatalf("QueuePosition(missing) = %d of %d, want 0 of 3", pos, total)
	}
}

func TestMarkRunQueuedOnlyAppliesToPendingRun(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := d.MarkRunQueued(run.ID, ""); err == nil {
		t.Fatal("a running run must not be re-queued")
	}
}

func TestQueuedRunClaimAndCancelAreExclusive(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run := queuedRunForTest(t, d, repo.ID, "feature")

	if spec, err := d.GetRunQueueSpec(run.ID); err != nil || spec != `{"trigger":"push"}` {
		t.Fatalf("queue spec = %q, %v", spec, err)
	}
	if ok, err := d.ClaimQueuedRun(run.ID); err != nil || !ok {
		t.Fatalf("claim = %v, %v; want true", ok, err)
	}
	if ok, _ := d.ClaimQueuedRun(run.ID); ok {
		t.Fatal("a run was claimed twice")
	}
	if ok, _ := d.CancelQueuedRun(run.ID, "x"); ok {
		t.Fatal("a claimed run was cancelled as queued")
	}
	got, _ := d.GetRun(run.ID)
	if got.Status != types.RunPending {
		t.Fatalf("claimed status = %s, want pending", got.Status)
	}

	other := queuedRunForTest(t, d, repo.ID, "other")
	if ok, err := d.CancelQueuedRun(other.ID, types.RunCancelReasonAbortedByUser); err != nil || !ok {
		t.Fatalf("cancel = %v, %v; want true", ok, err)
	}
	if ok, _ := d.ClaimQueuedRun(other.ID); ok {
		t.Fatal("a cancelled run was claimed")
	}
	got, _ = d.GetRun(other.ID)
	if got.Status != types.RunCancelled || got.Error == nil || *got.Error != types.RunCancelReasonAbortedByUser {
		t.Fatalf("cancelled run = %s %v", got.Status, got.Error)
	}
}

func TestQueuedRunIsTheBranchActiveRunButNotAStaleRun(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run := queuedRunForTest(t, d, repo.ID, "feature")

	for _, branch := range []string{"feature", ""} {
		active, err := d.GetActiveRun(repo.ID, branch)
		if err != nil || active == nil || active.ID != run.ID || active.Status != types.RunQueued {
			t.Fatalf("GetActiveRun(%q) = %#v, %v; want the queued run", branch, active, err)
		}
	}
	// Crash recovery must leave queued runs queued: nothing was executing.
	if n, err := d.RecoverStaleRuns("daemon crashed"); err != nil || n != 0 {
		t.Fatalf("RecoverStaleRuns = %d, %v; want 0", n, err)
	}
	got, _ := d.GetRun(run.ID)
	if got.Status != types.RunQueued {
		t.Fatalf("status after stale recovery = %s, want queued", got.Status)
	}
	if running, _ := d.GetActiveRuns(); len(running) != 0 {
		t.Fatalf("GetActiveRuns (executing runs) = %d, want 0", len(running))
	}
}

func TestQueuedRunPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	a := queuedRunForTest(t, d, repo.ID, "a")
	b := queuedRunForTest(t, d, repo.ID, "b")
	d.Close()

	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ids, _ := d.QueuedRunIDs()
	if len(ids) != 2 || ids[0] != a.ID || ids[1] != b.ID {
		t.Fatalf("reopened queue = %v, want [%s %s]", ids, a.ID, b.ID)
	}
}

func TestTerminateActiveRunNeverOverwritesATerminalRun(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	for _, status := range []types.RunStatus{types.RunPending, types.RunRunning} {
		run, _ := d.InsertRun(repo.ID, "active-"+string(status), "head", "base")
		if err := d.UpdateRunStatus(run.ID, status); err != nil {
			t.Fatal(err)
		}
		if ok, err := d.TerminateActiveRun(run.ID, "cancelled: aborted by user", types.RunCancelled); err != nil || !ok {
			t.Fatalf("terminate %s run = %v, %v; want it applied", status, ok, err)
		}
	}
	for _, status := range []types.RunStatus{types.RunCompleted, types.RunFailed, types.RunCancelled, types.RunQueued} {
		run, _ := d.InsertRun(repo.ID, "settled-"+string(status), "head", "base")
		if err := d.UpdateRunStatus(run.ID, status); err != nil {
			t.Fatal(err)
		}
		if ok, err := d.TerminateActiveRun(run.ID, "late cancellation", types.RunCancelled); err != nil || ok {
			t.Fatalf("terminate %s run = %v, %v; want it left alone", status, ok, err)
		}
		got, _ := d.GetRun(run.ID)
		if got.Status != status || got.Error != nil {
			t.Fatalf("%s run became %s / %v", status, got.Status, got.Error)
		}
	}
}

// The stored queue order follows admission order even when the clock steps
// backwards between admissions, so a restart restores it unchanged.
func TestQueuedAtIsMonotonicWhateverTheClockDoes(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	times := []int64{5_000, 1_000, 1_000, 9_000_000, 2}
	d.queueClock = func() int64 {
		ts := times[0]
		times = times[1:]
		return ts
	}
	var want []string
	var last int64
	for i := range []int{0, 1, 2, 3, 4} {
		run, _ := d.InsertRun(repo.ID, "b"+string(rune('a'+i)), "head", "base")
		ts, err := d.MarkRunQueued(run.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if ts <= last {
			t.Fatalf("admission %d stamped %d after %d: queued_at went backwards", i, ts, last)
		}
		last = ts
		want = append(want, run.ID)
	}
	got, err := d.QueuedRunIDs()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("restored queue order = %v, want admission order %v", got, want)
	}
}

func TestPendingGateResponseLifecycle(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	if err := d.SetStepPendingResponse(step.ID, `{"action":"approve"}`, "x", databaseGateSnapshot(t, d, step.ID)); err == nil {
		t.Fatal("a response was recorded for a step that is not parked")
	}
	if _, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.ParkStepForApproval(run.ID, step.ID, types.StepStatusFixReview, 0, 1, nil); err != nil {
		t.Fatal(err)
	}
	current := databaseGateSnapshot(t, d, step.ID)
	for _, operation := range []func() error{
		func() error {
			return d.SetStepPendingResponse(step.ID, `{"action":"approve"}`, "answered", types.GateIdentity{})
		},
		func() error {
			return d.ResumeStepFromApproval(run.ID, step.ID, "gate resumed", 5, types.GateIdentity{})
		},
	} {
		err := operation()
		refusal, ok := err.(*types.GateSupersededError)
		if !ok || refusal.Current != current {
			t.Fatalf("unbound storage operation: %v", err)
		}
		row, err := d.GetStepResult(step.ID)
		if err != nil || row.Status != types.StepStatusFixReview || row.PendingResponse != nil {
			t.Fatalf("unbound operation changed gate: %+v, %v", row, err)
		}
	}
	if err := d.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.SetStepPendingResponse(step.ID, `{"action":"approve"}`, "answered", databaseGateSnapshot(t, d, step.ID)); err != nil {
		t.Fatal(err)
	}
	got, _ := d.GetStepResult(step.ID)
	if got.Status != types.StepStatusFixReview || !got.AnsweredWaitingForSlot() || *got.PendingResponse != `{"action":"approve"}` {
		t.Fatalf("recorded step = %+v", got)
	}
	// A re-park (a new round) starts with no stale answer.
	if err := d.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 1, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = d.GetStepResult(step.ID); got.PendingResponse != nil {
		t.Fatalf("a new park kept the old pending response: %v", *got.PendingResponse)
	}
	// Dispatch clears the response, the parked status and the marker together.
	if err := d.SetStepPendingResponse(step.ID, `{"action":"skip"}`, "answered", databaseGateSnapshot(t, d, step.ID)); err != nil {
		t.Fatal(err)
	}
	if err := d.ResumeStepFromApproval(run.ID, step.ID, "gate resumed", 5, databaseGateSnapshot(t, d, step.ID)); err != nil {
		t.Fatal(err)
	}
	got, _ = d.GetStepResult(step.ID)
	r, _ := d.GetRun(run.ID)
	if got.Status != types.StepStatusRunning || got.PendingResponse != nil || r.AwaitingAgentSince != nil {
		t.Fatalf("after dispatch: step %+v run awaiting %v", got, r.AwaitingAgentSince)
	}
}
