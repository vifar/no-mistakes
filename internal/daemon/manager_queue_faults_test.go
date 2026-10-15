package daemon

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// faultFixture drives a RunManager directly (no IPC daemon) so a test can
// inject faults between its internal steps.
type faultFixture struct {
	t    *testing.T
	mgr  *RunManager
	d    *db.DB
	p    *paths.Paths
	step *schedStep
	head string
}

func newFaultFixture(t *testing.T, maxRuns string) *faultFixture {
	t.Helper()
	tmp, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	p := paths.WithRoot(tmp)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	cfg := "agent: claude\nagent_path_override:\n  claude: " + mockClaude + "\ndaemon:\n  max_concurrent_runs: " + maxRuns + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	_, head := setupTestGitRepo(t, p, d, "fault-repo")
	step := newSchedStep()
	mgr := NewRunManager(d, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(mgr.Shutdown)
	return &faultFixture{t: t, mgr: mgr, d: d, p: p, step: step, head: head}
}

func (f *faultFixture) push(branch string) string {
	f.t.Helper()
	id, err := f.mgr.HandlePushReceived(context.Background(), &ipc.PushReceivedParams{
		Gate: f.p.RepoDir("fault-repo"),
		Ref:  "refs/heads/" + branch,
		Old:  "0000000000000000000000000000000000000000",
		New:  f.head,
	})
	if err != nil {
		f.t.Fatalf("push %s: %v", branch, err)
	}
	return id
}

func (f *faultFixture) status(id string) types.RunStatus {
	f.t.Helper()
	run, err := f.d.GetRun(id)
	if err != nil || run == nil {
		f.t.Fatalf("get run %s: %v", id, err)
	}
	return run.Status
}

func (f *faultFixture) waitFor(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("timed out waiting for %s", what)
}

func (f *faultFixture) waitStarted(id string) {
	f.t.Helper()
	f.waitFor("run "+id+" to start executing", func() bool {
		for _, s := range f.step.startedIDs() {
			if s == id {
				return true
			}
		}
		return false
	})
}

func (f *faultFixture) started(id string) bool {
	for _, s := range f.step.startedIDs() {
		if s == id {
			return true
		}
	}
	return false
}

// A claim that fails transiently after dispatch took the run off the queue
// must not strand it: it returns to the head of the queue, ahead of later
// runs, and then starts.
func TestQueueClaimFailureReturnsRunToItsFIFOPosition(t *testing.T) {
	old := claimRetryDelay
	claimRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() { claimRetryDelay = old })
	f := newFaultFixture(t, "1")

	var failures atomic.Int32
	realClaim := f.mgr.claimQueued
	var first string
	f.mgr.claimQueued = func(id string) (bool, error) {
		if id == first && failures.Add(1) <= 2 {
			return false, errors.New("injected transient claim error")
		}
		return realClaim(id)
	}

	running := f.push("c0")
	f.waitStarted(running)
	first = f.push("c1")
	second := f.push("c2")

	f.step.release(running)
	// The failing claim for c1 must not let c2 jump it.
	f.waitStarted(first)
	if f.started(second) {
		t.Fatalf("a later queued run started before the run whose claim failed (started = %v)", f.step.startedIDs())
	}
	if got := failures.Load(); got < 3 {
		t.Fatalf("claim attempts for the first queued run = %d, want the two injected failures plus a success", got)
	}
	f.step.release(first)
	f.waitStarted(second)
	f.step.release(second)
	f.waitFor("all runs to complete", func() bool {
		return f.status(running) == types.RunCompleted && f.status(first) == types.RunCompleted && f.status(second) == types.RunCompleted
	})
}

// While its claim keeps failing the run is still abortable, and an abort ends
// it without it ever starting.
func TestQueueClaimFailureRunStaysAbortable(t *testing.T) {
	old := claimRetryDelay
	claimRetryDelay = 30 * time.Millisecond
	t.Cleanup(func() { claimRetryDelay = old })
	f := newFaultFixture(t, "1")

	var attempts atomic.Int32
	var stuck string
	realClaim := f.mgr.claimQueued
	f.mgr.claimQueued = func(id string) (bool, error) {
		if id == stuck {
			attempts.Add(1)
			return false, errors.New("injected persistent claim error")
		}
		return realClaim(id)
	}

	running := f.push("a0")
	f.waitStarted(running)
	stuck = f.push("a1")
	f.step.release(running)
	f.waitFor("the failing claim to be retried", func() bool { return attempts.Load() >= 2 })
	if got := f.status(stuck); got != types.RunQueued {
		t.Fatalf("run with a failing claim status = %s, want it still queued", got)
	}

	if err := f.mgr.HandleCancel(stuck); err != nil {
		t.Fatalf("abort during claim retries: %v", err)
	}
	f.waitFor("the abort to terminalize the run", func() bool { return f.status(stuck) == types.RunCancelled })
	n := attempts.Load()
	time.Sleep(150 * time.Millisecond)
	if after := attempts.Load(); after > n+1 {
		t.Fatalf("claims kept being retried after the abort (%d -> %d)", n, after)
	}
	if f.started(stuck) {
		t.Fatal("an aborted run started")
	}
	// The slot it held was released: the manager is free to run new work.
	next := f.push("a2")
	f.waitStarted(next)
	f.step.release(next)
}

// Shutdown racing the launch of a run the queue just dispatched: the shutdown
// check and handle registration are one atomic step, so the launch either never
// starts or is cancelled by Shutdown. Here the pause sits between the early
// check and registration.
func TestQueueShutdownRacingQueuedLaunchNeverStartsIt(t *testing.T) {
	f := newFaultFixture(t, "1")
	running := f.push("s0")
	f.waitStarted(running)
	queued := f.push("s1")

	reached := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	f.mgr.beforeQueuedRegister = func() {
		once.Do(func() {
			close(reached)
			<-resume
		})
	}

	// Freeing the slot dispatches s1 into the paused launch.
	f.step.release(running)
	<-reached

	shutdownDone := make(chan struct{})
	go func() {
		f.mgr.Shutdown()
		close(shutdownDone)
	}()
	f.waitFor("shutdown to begin", func() bool { return f.mgr.shuttingDown.Load() })
	close(resume)
	select {
	case <-shutdownDone:
	case <-time.After(45 * time.Second):
		t.Fatal("Shutdown did not finish")
	}

	if f.started(queued) {
		t.Fatalf("a queued run started during shutdown (started = %v)", f.step.startedIDs())
	}
	f.mgr.mu.Lock()
	leaked := len(f.mgr.cancels)
	f.mgr.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("%d run handles still registered after shutdown", leaked)
	}
	// The durable row stays queued so the next daemon start drains it.
	if got := f.status(queued); got != types.RunQueued {
		t.Fatalf("queued run status after shutdown = %s, want queued for the next start", got)
	}
	if st := f.mgr.sched.stats(); st.Active != 0 {
		t.Fatalf("slots still in use after shutdown: %+v", st)
	}
}
