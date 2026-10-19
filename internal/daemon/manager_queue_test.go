package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// schedStep is a controllable pipeline step. Its behaviour follows the run's
// branch name:
//
//	park*  asks for approval, so the run parks at a gate
//	fail*  waits for release(), then fails
//	panic* waits for release(), then panics
//	other  waits for release(), then passes
//
// started records the order in which runs began executing.
type schedStep struct {
	mu      sync.Mutex
	started []string
	gates   map[string]chan struct{}
}

func TestQueuedSetupAbortCancelsStalledFetchAndReleasesSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX git shim")
	}
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, head := setupTestGitRepo(t, p, database, "queued-setup")
	run, err := database.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkRunQueued(run.ID, ""); err != nil {
		t.Fatal(err)
	}
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "fetch-started")
	// Keep the stalled operation longer than the completion budget. Waiting
	// for the launch also includes real Git worktree cleanup, which can take
	// several seconds on a loaded host even after fetch was cancelled.
	script := fmt.Sprintf("#!/bin/sh\nfor arg in \"$@\"; do\n if [ \"$arg\" = fetch ]; then\n  touch '%s'\n  exec sleep 60\n fi\ndone\nexec '%s' \"$@\"\n", marker, realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	m := NewRunManager(database, p, nil)
	m.sched = newSlotScheduler(func() (int, bool) { return 1, false }, &m.mu)
	m.sched.start = m.startQueuedRun
	defer m.Shutdown()
	m.runs[run.ID] = &managedRun{run: run, state: ownershipQueued}
	m.sched.enqueue(run.ID)
	// Setup is outside the cancellation assertion below: a busy host may
	// need time to create the worktree before the fetch shim starts.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued launch never reached trusted fetch")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := m.HandleCancel(run.ID); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() { m.wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("abort did not stop queued setup")
	}
	if usage := m.sched.stats(); usage.Active != 0 {
		t.Fatalf("cancelled setup retained its slot: %+v", usage)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status.Active() {
		t.Fatalf("cancelled setup left an active run: %+v", stored)
	}
}

func newSchedStep() *schedStep { return &schedStep{gates: map[string]chan struct{}{}} }

func (s *schedStep) Name() types.StepName { return types.StepReview }

func (s *schedStep) gate(runID string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.gates[runID]
	if !ok {
		g = make(chan struct{})
		s.gates[runID] = g
	}
	return g
}

func (s *schedStep) release(runID string) { close(s.gate(runID)) }

func (s *schedStep) startedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.started...)
}

func (s *schedStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.mu.Lock()
	s.started = append(s.started, sctx.Run.ID)
	s.mu.Unlock()
	branch := sctx.Run.Branch
	if strings.HasPrefix(branch, "park") {
		if err := bindMockRunComparison(sctx); err != nil {
			return nil, err
		}
		return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[],"summary":"needs review"}`}, nil
	}
	select {
	case <-s.gate(sctx.Run.ID):
	case <-sctx.Ctx.Done():
		return nil, context.Cause(sctx.Ctx)
	}
	switch {
	case strings.HasPrefix(branch, "fail"):
		return nil, fmt.Errorf("induced failure")
	case strings.HasPrefix(branch, "panic"):
		panic("induced panic")
	}
	if err := bindMockRunComparison(sctx); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{}, nil
}

type schedFixture struct {
	t      *testing.T
	p      *paths.Paths
	d      *db.DB
	client *ipc.Client
	step   *schedStep
	repoID string
	head   string
}

func newSchedFixture(t *testing.T, daemonConfig string) *schedFixture {
	t.Helper()
	step := newSchedStep()
	p, d := startTestDaemonWithStepsConfig(t, func() []pipeline.Step { return []pipeline.Step{step} }, daemonConfig)
	f := &schedFixture{t: t, p: p, d: d, step: step, repoID: "sched-repo"}
	_, f.head = setupTestGitRepo(t, p, d, f.repoID)
	f.dial()
	return f
}

func (f *schedFixture) dial() {
	f.t.Helper()
	if f.client != nil {
		f.client.Close()
	}
	client, err := ipc.Dial(f.p.Socket())
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { client.Close() })
	f.client = client
}

// push launches a run for branch and returns its id and how long the launch
// RPC took.
func (f *schedFixture) push(branch string) (string, time.Duration) {
	f.t.Helper()
	// This fixture reports branch creation (Old=zero), including deliberate
	// recreation on a second push. Model the accepted Git change before the
	// post-receive RPC so live-tip validation sees the ref it describes.
	ref := "refs/heads/" + branch
	gitCmd(f.t, f.p.RepoDir(f.repoID), "update-ref", "-d", ref)
	gitCmd(f.t, f.p.RepoDir(f.repoID), "update-ref", ref, f.head)
	begin := time.Now()
	var result ipc.PushReceivedResult
	err := f.client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: f.p.RepoDir(f.repoID),
		Ref:  "refs/heads/" + branch,
		Old:  "0000000000000000000000000000000000000000",
		New:  f.head,
	}, &result)
	if err != nil {
		f.t.Fatalf("push %s: %v", branch, err)
	}
	return result.RunID, time.Since(begin)
}

func (f *schedFixture) info(runID string) *ipc.RunInfo {
	f.t.Helper()
	var result ipc.GetRunResult
	if err := f.client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: runID}, &result); err != nil {
		f.t.Fatalf("get run %s: %v", runID, err)
	}
	return result.Run
}

func (f *schedFixture) status(runID string) types.RunStatus {
	f.t.Helper()
	run, err := f.d.GetRun(runID)
	if err != nil || run == nil {
		f.t.Fatalf("get run %s: %v", runID, err)
	}
	return run.Status
}

// Read persisted causes while the fixture DB and step logs still exist.
func (f *schedFixture) runDiagnostics(runID string) string {
	f.t.Helper()
	var detail strings.Builder
	run, err := f.d.GetRun(runID)
	if err != nil || run == nil {
		fmt.Fprintf(&detail, "read run: %v (found=%t)\n", err, run != nil)
	} else {
		cause := "<nil>"
		if run.Error != nil {
			cause = *run.Error
		}
		fmt.Fprintf(&detail, "run status=%s error=%q\n", run.Status, cause)
	}
	steps, err := f.d.GetStepsByRun(runID)
	if err != nil {
		fmt.Fprintf(&detail, "read steps: %v\n", err)
	}
	for _, step := range steps {
		cause := "<nil>"
		if step.Error != nil {
			cause = *step.Error
		}
		fmt.Fprintf(&detail, "step=%s status=%s error=%q\n", step.StepName, step.Status, cause)
	}
	return detail.String()
}

func (f *schedFixture) waitStatus(runID string, want ...types.RunStatus) types.RunStatus {
	f.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var got types.RunStatus
	for time.Now().Before(deadline) {
		got = f.status(runID)
		for _, w := range want {
			if got == w {
				return got
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatalf("run %s status = %s, want one of %v\n%s", runID, got, want, f.runDiagnostics(runID))
	return got
}

func (f *schedFixture) waitStarted(runID string) {
	f.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, id := range f.step.startedIDs() {
			if id == runID {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("run %s never started executing; started = %v\n%s", runID, f.step.startedIDs(), f.runDiagnostics(runID))
}

// assertNotStarted settles the scheduler (a no-op round trip) and then checks
// that none of ids began executing.
func (f *schedFixture) assertNotStarted(ids ...string) {
	f.t.Helper()
	time.Sleep(150 * time.Millisecond)
	started := map[string]bool{}
	for _, id := range f.step.startedIDs() {
		started[id] = true
	}
	for _, id := range ids {
		if started[id] {
			f.t.Fatalf("run %s started executing but should be queued; started = %v", id, f.step.startedIDs())
		}
		// Execution begins well after the queue releases a run (worktree and
		// agent setup come first), so also require the row to still be queued.
		if got := f.status(id); got != types.RunQueued {
			f.t.Fatalf("run %s status = %s, want it still queued", id, got)
		}
	}
}

func (f *schedFixture) assertQueued(runID string, position, total int) {
	f.t.Helper()
	if got := f.status(runID); got != types.RunQueued {
		f.t.Fatalf("run %s status = %s, want queued", runID, got)
	}
	info := f.info(runID)
	if info.Status != types.RunQueued || info.QueuePosition != position || info.QueueTotal != total {
		f.t.Fatalf("run %s info = status %s position %d of %d, want queued %d of %d", runID, info.Status, info.QueuePosition, info.QueueTotal, position, total)
	}
}

// assertNoWorktree checks a queued run did no launch setup: no worktree on disk.
func (f *schedFixture) assertNoWorktree(runID string) {
	f.t.Helper()
	if _, err := os.Stat(f.p.WorktreeDir(f.repoID, runID)); !os.IsNotExist(err) {
		f.t.Fatalf("queued run %s has a worktree (stat err %v); a queued launch must not run setup", runID, err)
	}
}

func (f *schedFixture) slots() ipc.SlotsResult {
	f.t.Helper()
	var result ipc.SlotsResult
	if err := f.client.Call(ipc.MethodGetSlots, nil, &result); err != nil {
		f.t.Fatalf("get_slots: %v", err)
	}
	return result
}

func (f *schedFixture) setCap(n int) {
	f.t.Helper()
	if err := config.SetGlobalDaemonSetting(f.p.ConfigFile(), "max_concurrent_runs", strconv.Itoa(n)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *schedFixture) reloadSlots() ipc.SlotsResult {
	f.t.Helper()
	var result ipc.SlotsResult
	if err := f.client.Call(ipc.MethodReloadSlots, nil, &result); err != nil {
		f.t.Fatalf("reload_slots: %v", err)
	}
	return result
}

// slowWatcher keeps the config watcher out of a test that must observe the
// effect of an explicit reload on its own.
func slowWatcher(t *testing.T, interval time.Duration) {
	t.Helper()
	old := limitWatchInterval
	limitWatchInterval = interval
	t.Cleanup(func() { limitWatchInterval = old })
}

func (f *schedFixture) respondApprove(runID string) {
	f.t.Helper()
	var result ipc.RespondResult
	err := f.client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: runID, Step: types.StepReview, Action: types.ActionApprove, ExpectedGate: ipcGateSnapshot(f.t, f.client, runID, types.StepReview)}, &result)
	if err != nil || !result.OK {
		f.t.Fatalf("approve %s: result=%+v, error=%v", runID, result, err)
	}
}

func (f *schedFixture) waitParked(runID string) {
	f.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		run, err := f.d.GetRun(runID)
		if err == nil && run != nil && run.AwaitingAgentSince != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("run %s never parked\n%s", runID, f.runDiagnostics(runID))
}

// launchReplyBudget is deliberately loose: it only has to tell "replied" from
// "blocked until a slot freed", which in these tests means never. The strict
// check is structural (assertQueued, assertNoWorktree): a queued launch does no
// setup work at all.
const launchReplyBudget = 10 * time.Second

// The captured test failure is the public diagnostic contract: an unexpected
// terminal run must explain both persisted causes before its fixture is removed.
// Removing runDiagnostics from waitStatus must fail this test, even if the daemon
// independently logs the same cause.
func TestQueueWaitStatusReportsTerminalCausesBeforeCleanup(t *testing.T) {
	const childEnv = "NM_TEST_QUEUE_WAIT_DIAGNOSTICS"
	const cleanupMessage = "scheduler diagnostic fixture cleanup complete"
	if os.Getenv(childEnv) == "1" {
		var root string
		// Registered first, so this runs after the fixture's daemon, DB and root
		// cleanups, including when waitStatus calls Fatalf.
		t.Cleanup(func() {
			if root == "" {
				return
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Errorf("scheduler diagnostic fixture was not removed: %v", err)
				return
			}
			t.Log(cleanupMessage)
		})
		f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
		root = f.p.Root()
		id, _ := f.push("fail-diagnostics")
		f.waitStarted(id)
		f.step.release(id)
		f.waitStatus(id, types.RunFailed)
		// Keep the ordinary completed expectation: the deliberately failed run
		// must reach the real wait failure path, not just its string formatter.
		f.waitStatus(id, types.RunCompleted)
		t.Fatal("failed run unexpectedly satisfied the completed expectation")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestQueueWaitStatusReportsTerminalCausesBeforeCleanup$", "-test.timeout=3m")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("scheduler diagnostic child did not finish: %v\n%s", ctx.Err(), output)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected an ordinary test failure, got %v\n%s", err, output)
	}
	// Ignore testing's indentation, but require the causes in the wait's fatal
	// message itself. These literals come from schedStep's induced failure and
	// the executor's persisted run/step error contract, not runDiagnostics.
	lines := strings.Split(string(output), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	got := strings.Join(lines, "\n")
	const want = "status = failed, want one of [completed]\n" +
		"run status=failed error=\"step review failed: induced failure\"\n" +
		"step=review status=failed error=\"induced failure\""
	diagnosticAt := strings.Index(got, want)
	if diagnosticAt < 0 {
		t.Fatalf("wait failure omitted persisted terminal causes:\n%s", output)
	}
	if cleanupAt := strings.Index(got, cleanupMessage); cleanupAt <= diagnosticAt {
		t.Fatalf("wait diagnostic did not precede completed fixture cleanup:\n%s", output)
	}
}

func TestQueueCapTwoWithFourLaunchesRunsTwoAndQueuesTwo(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 2\n")
	r1, _ := f.push("b1")
	r2, _ := f.push("b2")
	f.waitStarted(r1)
	f.waitStarted(r2)
	r3, took3 := f.push("b3")
	r4, took4 := f.push("b4")
	if took3 > launchReplyBudget || took4 > launchReplyBudget {
		t.Fatalf("queued launches took %s and %s; they must reply immediately", took3, took4)
	}
	f.assertQueued(r3, 1, 2)
	f.assertQueued(r4, 2, 2)
	f.assertNotStarted(r3, r4)
	f.assertNoWorktree(r3)
	f.assertNoWorktree(r4)
	if got := f.step.startedIDs(); len(got) != 2 {
		t.Fatalf("executing runs = %v, want exactly 2", got)
	}
	// A queued run is its branch's active run: reattach and abort find it.
	var active ipc.GetActiveRunResult
	if err := f.client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: f.repoID, Branch: "b3"}, &active); err != nil {
		t.Fatal(err)
	}
	if active.Run == nil || active.Run.ID != r3 || active.Run.QueuePosition != 1 {
		t.Fatalf("active run for queued branch = %#v, want %s at position 1", active.Run, r3)
	}
	for _, id := range []string{r1, r2} {
		f.step.release(id)
	}
	f.step.release(r3)
	f.step.release(r4)
	for _, id := range []string{r1, r2, r3, r4} {
		f.waitStatus(id, types.RunCompleted)
	}
}

func TestQueueStartsInFIFOOrderAsSlotsFree(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	first, _ := f.push("fifo0")
	f.waitStarted(first)
	var queued []string
	for i := 1; i <= 3; i++ {
		id, _ := f.push(fmt.Sprintf("fifo%d", i))
		queued = append(queued, id)
	}
	for i, id := range queued {
		f.assertQueued(id, i+1, 3)
	}
	prev := first
	for i, id := range queued {
		f.step.release(prev)
		f.waitStarted(id)
		f.assertNotStarted(queued[i+1:]...)
		prev = id
	}
	f.step.release(prev)
	f.waitStatus(prev, types.RunCompleted)
	want := append([]string{first}, queued...)
	if got := f.step.startedIDs(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("start order = %v, want %v", got, want)
	}
}

func TestQueueParkedRunReleasesSlotAndResumedRunBeatsQueuedNewRun(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	parked, _ := f.push("park1")
	f.waitParked(parked)

	// The parked run gave its slot back, so a new launch starts at once.
	blocker, _ := f.push("block1")
	f.waitStarted(blocker)
	waiting, _ := f.push("block2")
	f.assertQueued(waiting, 1, 1)
	if got := f.slots(); got.MaxConcurrentRuns != 1 || got.InUse != 1 || got.Queued != 1 || got.ParkedReleased != 1 {
		t.Fatalf("slots with one running, one queued and one parked = %+v, want cap 1, in use 1, queued 1, parked 1", got)
	}

	// The operator responds: the parked run needs a slot again and goes
	// ahead of the queued new run, and says it is waiting for one.
	f.respondApprove(parked)
	// The acknowledgement itself promises the priority reservation (R2).
	// Polling here would hide a reservation registered too late.
	info := f.info(parked)
	if !info.WaitingForSlot || info.QueuePosition != 1 || info.QueueTotal != 2 || info.Status != types.RunRunning {
		t.Fatalf("acknowledged response did not reserve priority: %#v", info)
	}
	f.assertQueued(waiting, 2, 2)
	t.Logf("immediately after response acknowledgement: resumed position=%d/%d, new run position=%d/%d; slots=%+v", info.QueuePosition, info.QueueTotal, f.info(waiting).QueuePosition, f.info(waiting).QueueTotal, f.slots())

	f.step.release(blocker)
	f.waitStarted(waiting)
	// By the time the queued new run starts, the resumed run already finished.
	if got := f.status(parked); got != types.RunCompleted {
		t.Fatalf("resumed run status = %s when the queued run started, want completed first", got)
	}
	t.Logf("after blocker released: resumed run=%s before queued new run started", f.status(parked))
	f.step.release(waiting)
	f.waitStatus(waiting, types.RunCompleted)
}

func TestQueueHoldSlotWhileParkedKeepsTheSlot(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n  hold_slot_while_parked: true\n")
	parked, _ := f.push("park1")
	f.waitParked(parked)
	next, _ := f.push("block1")
	f.assertQueued(next, 1, 1)
	f.assertNotStarted(next)

	f.respondApprove(parked)
	f.waitStatus(parked, types.RunCompleted)
	f.waitStarted(next)
	f.step.release(next)
	f.waitStatus(next, types.RunCompleted)
}

func TestQueueAbortQueuedRunCancelsWithoutStartingAndShiftsPositions(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	running, _ := f.push("a0")
	f.waitStarted(running)
	q1, _ := f.push("a1")
	q2, _ := f.push("a2")
	q3, _ := f.push("a3")
	f.assertQueued(q3, 3, 3)

	if err := f.client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: q2}, nil); err != nil {
		t.Fatalf("abort queued run: %v", err)
	}
	if got := f.status(q2); got != types.RunCancelled {
		t.Fatalf("aborted queued run status = %s, want cancelled", got)
	}
	run, _ := f.d.GetRun(q2)
	if run.Error == nil || *run.Error != types.RunCancelReasonAbortedByUser {
		t.Fatalf("aborted queued run error = %v", run.Error)
	}
	f.assertQueued(q1, 1, 2)
	f.assertQueued(q3, 2, 2)

	f.step.release(running)
	f.waitStarted(q1)
	f.step.release(q1)
	f.waitStarted(q3)
	f.step.release(q3)
	f.waitStatus(q3, types.RunCompleted)
	for _, id := range f.step.startedIDs() {
		if id == q2 {
			t.Fatal("an aborted queued run was started")
		}
	}
}

func TestQueueNewPushSupersedesQueuedRunOnSameBranch(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	running, _ := f.push("s0")
	f.waitStarted(running)
	old, _ := f.push("s1")
	f.assertQueued(old, 1, 1)
	replacement, _ := f.push("s1")
	if got := f.status(old); got != types.RunCancelled {
		t.Fatalf("superseded queued run status = %s, want cancelled", got)
	}
	f.assertQueued(replacement, 1, 1)
	f.step.release(running)
	f.waitStarted(replacement)
	f.step.release(replacement)
	f.waitStatus(replacement, types.RunCompleted)
}

func TestQueueSlotIsReleasedOnFailureAbortAndPanic(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	for _, kind := range []string{"fail", "panic", "abort"} {
		holder, _ := f.push(kind + "-holder")
		f.waitStarted(holder)
		follower, _ := f.push("follower-" + kind)
		f.assertQueued(follower, 1, 1)

		switch kind {
		case "abort":
			if err := f.client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: holder}, nil); err != nil {
				t.Fatal(err)
			}
			f.waitStatus(holder, types.RunCancelled, types.RunFailed)
		default:
			f.step.release(holder)
			f.waitStatus(holder, types.RunFailed)
		}
		f.waitStarted(follower)
		f.step.release(follower)
		f.waitStatus(follower, types.RunCompleted)
	}
}

func TestQueueCapZeroIsUnlimited(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 0\n")
	var ids []string
	for i := 0; i < 6; i++ {
		id, _ := f.push(fmt.Sprintf("u%d", i))
		ids = append(ids, id)
	}
	for _, id := range ids {
		f.waitStarted(id)
		if got := f.status(id); got == types.RunQueued {
			t.Fatalf("run %s queued with max_concurrent_runs 0", id)
		}
	}
	for _, id := range ids {
		f.step.release(id)
	}
	for _, id := range ids {
		f.waitStatus(id, types.RunCompleted)
	}
}

func TestQueueDefaultCapIsThree(t *testing.T) {
	f := newSchedFixture(t, "")
	var ids []string
	for i := 0; i < 4; i++ {
		id, _ := f.push(fmt.Sprintf("d%d", i))
		ids = append(ids, id)
	}
	f.waitStarted(ids[2])
	f.assertQueued(ids[3], 1, 1)
	for _, id := range ids {
		f.step.release(id)
	}
	for _, id := range ids {
		f.waitStatus(id, types.RunCompleted)
	}
}

func TestQueueControlPlaneAnswersPromptlyWhileSlotsAreFull(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	running, _ := f.push("c0")
	f.waitStarted(running)
	queued, _ := f.push("c1")

	within := func(name string, call func() error) {
		t.Helper()
		begin := time.Now()
		if err := call(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if took := time.Since(begin); took > 2*time.Second {
			t.Fatalf("%s took %s with every slot full", name, took)
		}
	}
	within("get_run", func() error {
		return f.client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: queued}, &ipc.GetRunResult{})
	})
	within("get_runs", func() error {
		return f.client.Call(ipc.MethodGetRuns, &ipc.GetRunsParams{RepoID: f.repoID}, &ipc.GetRunsResult{})
	})
	within("get_active_run", func() error {
		return f.client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: f.repoID, Branch: "c1"}, &ipc.GetActiveRunResult{})
	})
	within("gate_context", func() error {
		return f.client.Call(ipc.MethodGateContext, &ipc.GateContextParams{CWD: t.TempDir()}, &ipc.GateContextResult{})
	})
	within("admit_push", func() error {
		// A refusal is a valid answer; only an unanswered call is a failure.
		_ = f.client.Call(ipc.MethodAdmitPush, &ipc.AdmitPushParams{Gate: f.p.RepoDir(f.repoID)}, &ipc.AdmitPushResult{})
		return nil
	})
	within("respond", func() error {
		// There is no gate on a running or queued run: an error, but fast.
		_ = f.client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: queued, Step: types.StepReview, Action: types.ActionApprove, ExpectedGate: ipcGateSnapshot(t, f.client, queued, types.StepReview)}, nil)
		return nil
	})
	f.step.release(running)
	f.step.release(queued)
	f.waitStatus(queued, types.RunCompleted)
}

func TestQueueSurvivesDaemonRestartAndDrains(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	running, _ := f.push("r0")
	f.waitStarted(running)
	q1, _ := f.push("r1")
	q2, _ := f.push("r2")
	f.assertQueued(q1, 1, 2)
	f.assertQueued(q2, 2, 2)

	// The server drains open connections before it exits and releases its
	// singleton lock, so close this client first.
	f.client.Close()
	shutdownTestDaemonAndWaitForCleanup(t, f.p)

	// The running run died with the daemon; the queued ones are still queued.
	if got := f.status(running); !got.Terminal() {
		t.Fatalf("run that was executing at shutdown status = %s, want terminal", got)
	}
	for _, id := range []string{q1, q2} {
		if got := f.status(id); got != types.RunQueued {
			t.Fatalf("queued run %s status after shutdown = %s, want queued", id, got)
		}
	}
	startedBefore := len(f.step.startedIDs())

	f.restartDaemon()

	// FIFO survives the restart: q1 starts first and q2 stays queued behind it.
	f.waitStarted(q1)
	f.assertQueued(q2, 1, 1)
	f.assertNotStarted(q2)
	f.step.release(q1)
	f.waitStarted(q2)
	f.step.release(q2)
	f.waitStatus(q2, types.RunCompleted)
	if got := f.step.startedIDs()[startedBefore:]; strings.Join(got, ",") != q1+","+q2 {
		t.Fatalf("post-restart start order = %v, want [%s %s]", got, q1, q2)
	}
}

// restartDaemon starts a second daemon on the same NM_HOME and database once
// the first has released the singleton lock (it does so just after removing
// its socket).
func (f *schedFixture) restartDaemon() {
	f.t.Helper()
	sf := func() []pipeline.Step { return []pipeline.Step{f.step} }
	deadline := time.Now().Add(30 * time.Second)
	for {
		exited := make(chan error, 1)
		stopped := make(chan struct{})
		go func() {
			exited <- RunWithOptions(f.p, f.d, sf)
			close(stopped)
		}()
		select {
		case err := <-exited:
			if !errors.Is(err, ErrSingletonLockHeld) || time.Now().After(deadline) {
				f.t.Fatalf("restart daemon: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
			continue
		case <-time.After(500 * time.Millisecond):
		}
		f.t.Cleanup(func() {
			if client, err := ipc.Dial(f.p.Socket()); err == nil {
				_ = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
				client.Close()
			}
			select {
			case <-stopped:
			case <-time.After(30 * time.Second):
				f.t.Error("restarted daemon did not stop")
			}
		})
		waitForDaemonReadyOrExit(f.t, f.p, exited)
		f.dial()
		return
	}
}

// Editing daemon.max_concurrent_runs in the global config applies without a
// restart: the watcher notices the new mtime and a raised cap starts queued runs.
func TestQueueHotReloadFromConfigFileRaisingCapStartsQueuedRuns(t *testing.T) {
	slowWatcher(t, 100*time.Millisecond)
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	running, _ := f.push("h0")
	f.waitStarted(running)
	q1, _ := f.push("h1")
	q2, _ := f.push("h2")
	f.assertQueued(q2, 2, 2)

	f.setCap(3)
	f.waitStarted(q1)
	f.waitStarted(q2)
	if got := f.slots(); got.MaxConcurrentRuns != 3 || got.InUse != 3 || got.Queued != 0 {
		t.Fatalf("slots after raising the cap = %+v, want cap 3, 3 in use, none queued", got)
	}
	for _, id := range []string{running, q1, q2} {
		f.step.release(id)
	}
	for _, id := range []string{running, q1, q2} {
		f.waitStatus(id, types.RunCompleted)
	}
}

// The reload RPC (what `daemon set` calls) applies a raised cap before it
// replies, with no help from the file watcher.
func TestQueueReloadSlotsRPCAppliesRaisedCapImmediately(t *testing.T) {
	slowWatcher(t, time.Hour)
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	running, _ := f.push("p0")
	f.waitStarted(running)
	q1, _ := f.push("p1")
	f.assertQueued(q1, 1, 1)

	f.setCap(2)
	usage := f.reloadSlots()
	if usage.MaxConcurrentRuns != 2 || usage.InUse != 2 || usage.Queued != 0 {
		t.Fatalf("reload_slots = %+v, want cap 2, 2 in use, none queued", usage)
	}
	f.waitStarted(q1)
	f.step.release(running)
	f.step.release(q1)
	f.waitStatus(q1, types.RunCompleted)
}

// Lowering the cap never touches a running run; it only stops new admissions
// until usage drops below the new cap.
func TestQueueLoweringCapLeavesRunningRunsAndBlocksNewAdmissions(t *testing.T) {
	slowWatcher(t, time.Hour)
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 3\n")
	a, _ := f.push("l0")
	b, _ := f.push("l1")
	f.waitStarted(a)
	f.waitStarted(b)

	f.setCap(1)
	if usage := f.reloadSlots(); usage.MaxConcurrentRuns != 1 || usage.InUse != 2 {
		t.Fatalf("reload_slots = %+v, want cap 1 with both runs still holding slots", usage)
	}
	for _, id := range []string{a, b} {
		if got := f.status(id); got != types.RunRunning {
			t.Fatalf("run %s status after lowering the cap = %s, want it left running", id, got)
		}
	}
	waiting, _ := f.push("l2")
	f.assertQueued(waiting, 1, 1)

	// One run finishing leaves usage (1) at the new cap, so the queue holds.
	f.step.release(a)
	f.waitStatus(a, types.RunCompleted)
	f.assertNotStarted(waiting)
	// Dropping below the cap releases the queued run.
	f.step.release(b)
	f.waitStarted(waiting)
	f.step.release(waiting)
	f.waitStatus(waiting, types.RunCompleted)
}
