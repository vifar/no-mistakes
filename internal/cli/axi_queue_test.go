package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunObjectRendersQueuePosition(t *testing.T) {
	rv := runView{
		ID: "run-q", Branch: "feature/x", Status: string(types.RunQueued),
		HeadSHA: "abcdef1234567890", QueuePosition: 2, QueueTotal: 3,
	}
	out := axiDoc(runObjectField(rv))
	if !strings.Contains(out, "status: queued\n  queue_position: 2\n  queue_length: 3\n") {
		t.Errorf("queued run should render status then queue_position in:\n%s", out)
	}

	// A resumed run waiting for a slot keeps status running and says so.
	rv.Status = string(types.RunRunning)
	rv.WaitingForSlot = true
	out = axiDoc(runObjectField(rv))
	if !strings.Contains(out, "status: running\n  queue_position: 2\n") || !strings.Contains(out, "waiting_for_slot: true") {
		t.Errorf("waiting-for-slot run rendered:\n%s", out)
	}

	// A run that is not waiting renders none of it, nor does a terminal run
	// that carries a stale position.
	rv.QueuePosition, rv.QueueTotal, rv.WaitingForSlot = 0, 0, false
	if out := axiDoc(runObjectField(rv)); strings.Contains(out, "queue_") {
		t.Errorf("unqueued run rendered queue fields:\n%s", out)
	}
	rv.QueuePosition, rv.Status = 1, string(types.RunCancelled)
	if out := axiDoc(runObjectField(rv)); strings.Contains(out, "queue_") {
		t.Errorf("terminal run rendered queue fields:\n%s", out)
	}
}

func TestQueuedLine(t *testing.T) {
	for _, tc := range []struct {
		position, total int
		want            string
	}{
		{1, 3, "queued (position 1 of 3)"},
		{2, 2, "queued (position 2 of 2)"},
		{4, 0, "queued (position 4 of 4)"},
		{0, 0, "queued"},
	} {
		if got := queuedLine(tc.position, tc.total); got != tc.want {
			t.Errorf("queuedLine(%d, %d) = %q, want %q", tc.position, tc.total, got, tc.want)
		}
	}
}

func TestRenderQueuedRunSucceedsWithRunIDAndQueuePosition(t *testing.T) {
	var out, errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := renderQueuedRun(cmd, &ipc.RunInfo{
		ID: "run-q", Branch: "feature/x", HeadSHA: "abcdef1234567890", Status: types.RunQueued,
		QueuePosition: 2, QueueTotal: 4,
	})
	if err != nil {
		t.Fatalf("renderQueuedRun must succeed, got %v", err)
	}
	for _, want := range []string{"id: run-q", "status: queued", "queue_position: 2", "outcome: queued", "queued (position 2 of 4)", "axi abort --run run-q"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("queued result missing %q in:\n%s", want, out.String())
		}
	}
	if !strings.Contains(errOut.String(), "queued (position 2 of 4)") {
		t.Errorf("progress line missing from stderr: %q", errOut.String())
	}
}

func TestProgressPrinterReportsQueuePositionChangesOnly(t *testing.T) {
	var buf bytes.Buffer
	pp := &progressPrinter{w: &buf, seen: map[string]string{}}
	run := &ipc.RunInfo{ID: "r", Status: types.RunQueued, QueuePosition: 3, QueueTotal: 3}
	pp.update(run)
	pp.update(run)
	run.QueuePosition, run.QueueTotal = 2, 2
	pp.update(run)
	got := buf.String()
	if strings.Count(got, "queued (position 3 of 3)") != 1 || strings.Count(got, "queued (position 2 of 2)") != 1 {
		t.Errorf("progress output = %q, want each distinct position once", got)
	}
}

// A queued run is its branch's active run, so axi status reports it as the
// worktree's own run with its place in line.
func TestAxiStatusShowsQueuedRunAndPosition(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/mine")
	chdir(t, repoDir)

	ahead, err := database.InsertRun(repo.ID, "feature/ahead", "head-ahead", "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkRunQueued(ahead.ID, ""); err != nil {
		t.Fatal(err)
	}
	mine, err := database.InsertRun(repo.ID, "feature/mine", "head-mine", "base")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkRunQueued(mine.ID, ""); err != nil {
		t.Fatal(err)
	}

	out := axiStatusOutput(t, "")
	doc := decodeStatusDoc(t, out)
	if doc.Run.ID != mine.ID || doc.Run.Status != "queued" {
		t.Fatalf("status run = %+v, want queued run %s:\n%s", doc.Run, mine.ID, out)
	}
	if !strings.Contains(out, "queue_position: 2") {
		t.Fatalf("status did not report queue_position: 2:\n%s", out)
	}
}

func TestSlotsSummary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     ipc.SlotsResult
		always bool
		want   string
	}{
		{"capped", ipc.SlotsResult{MaxConcurrentRuns: 3, InUse: 2, Queued: 4}, false, "2/3 in use, 4 queued"},
		{"unlimited", ipc.SlotsResult{InUse: 5}, false, "5 in use (no cap), 0 queued"},
		{"parked shown when present", ipc.SlotsResult{MaxConcurrentRuns: 3, InUse: 1, ParkedReleased: 2}, false, "1/3 in use, 0 queued, 2 parked (slot released)"},
		{"parked always for doctor", ipc.SlotsResult{MaxConcurrentRuns: 3}, true, "0/3 in use, 0 queued, 0 parked (slot released)"},
	} {
		if got := slotsSummary(tc.in, tc.always); got != tc.want {
			t.Errorf("%s: slotsSummary = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func stubSlots(t *testing.T, result *ipc.SlotsResult) {
	t.Helper()
	old := fetchSlots
	fetchSlots = func(*paths.Paths) *ipc.SlotsResult { return result }
	t.Cleanup(func() { fetchSlots = old })
}

func TestAxiStatusAndHomeShowSlotUsage(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/mine")
	chdir(t, repoDir)
	mine, err := database.InsertRun(repo.ID, "feature/mine", "head-mine", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(mine.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	stubSlots(t, &ipc.SlotsResult{MaxConcurrentRuns: 3, InUse: 2, Queued: 4})
	if out := axiStatusOutput(t, ""); !strings.Contains(out, `slots: "2/3 in use, 4 queued"`) {
		t.Errorf("axi status missing the slots line:\n%s", out)
	}
	var home bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&home)
	if err := runAxiHome(cmd); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(home.String(), `slots: "2/3 in use, 4 queued"`) {
		t.Errorf("axi home missing the slots line:\n%s", home.String())
	}

	// No daemon, no live usage to report: the line is simply absent.
	stubSlots(t, nil)
	if out := axiStatusOutput(t, ""); strings.Contains(out, "slots:") {
		t.Errorf("axi status printed slots without a daemon:\n%s", out)
	}
}

func TestDoctorReportsRunSlots(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	oldRunning := daemonIsRunningFn
	daemonIsRunningFn = func(*paths.Paths) (bool, error) { return true, nil }
	t.Cleanup(func() { daemonIsRunningFn = oldRunning })
	stubSlots(t, &ipc.SlotsResult{MaxConcurrentRuns: 3, InUse: 2, Queued: 4, ParkedReleased: 1})
	out, err := executeCmd("doctor")
	if err != nil {
		t.Logf("doctor reported failures (unrelated to slots): %v", err)
	}
	if !strings.Contains(out, "run slots") || !strings.Contains(out, "2/3 in use, 4 queued, 1 parked (slot released)") {
		t.Errorf("doctor missing the run slots line:\n%s", out)
	}
}

// `daemon set max-concurrent-runs N` writes the global config and has the
// running daemon apply it over IPC, with no restart.
func TestDaemonSetMaxConcurrentRunsWritesConfigAndAppliesWithoutRestart(t *testing.T) {
	root := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", root)
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	oldRunning := daemonIsRunningFn
	daemonIsRunningFn = func(*paths.Paths) (bool, error) { return true, nil }
	t.Cleanup(func() { daemonIsRunningFn = oldRunning })

	// The fake daemon applies whatever the config file says when asked, which
	// proves the file was written before the daemon was told.
	srv := ipc.NewServer()
	var reloads atomic.Int32
	srv.Handle(ipc.MethodReloadSlots, func(context.Context, json.RawMessage) (interface{}, error) {
		reloads.Add(1)
		cfg, err := config.LoadGlobal(p.ConfigFile())
		if err != nil {
			return nil, err
		}
		return &ipc.SlotsResult{MaxConcurrentRuns: cfg.Daemon.MaxConcurrentRuns, InUse: 2, Queued: 1}, nil
	})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(p.Socket()) }()
	t.Cleanup(func() {
		srv.Close()
		select {
		case <-errCh:
		case <-time.After(time.Second):
			t.Error("IPC server did not stop")
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := ipc.Dial(p.Socket()); err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	out, err := executeCmd("daemon", "set", "max-concurrent-runs", "5")
	if err != nil {
		t.Fatalf("daemon set: %v\n%s", err, out)
	}
	if reloads.Load() != 1 {
		t.Fatalf("daemon was told to reload %d times, want 1", reloads.Load())
	}
	if !strings.Contains(out, "applied: slots 2/5 in use, 1 queued") {
		t.Errorf("output missing the applied usage:\n%s", out)
	}
	cfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil || cfg.Daemon.MaxConcurrentRuns != 5 {
		t.Fatalf("persisted cap = %v, %v; want 5", cfg.Daemon, err)
	}

	for _, bad := range []string{"-1", "many", "2.5"} {
		if out, err := executeCmd("daemon", "set", "max-concurrent-runs", bad); err == nil {
			t.Errorf("max-concurrent-runs %q accepted:\n%s", bad, out)
		}
	}
	cfg, _ = config.LoadGlobal(p.ConfigFile())
	if cfg.Daemon.MaxConcurrentRuns != 5 {
		t.Fatalf("a rejected value changed the config to %d", cfg.Daemon.MaxConcurrentRuns)
	}
}

func TestDaemonSetMaxConcurrentRunsWithStoppedDaemonOnlyWritesConfig(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	oldRunning := daemonIsRunningFn
	daemonIsRunningFn = func(*paths.Paths) (bool, error) { return false, nil }
	t.Cleanup(func() { daemonIsRunningFn = oldRunning })
	out, err := executeCmd("daemon", "set", "max-concurrent-runs", "0")
	if err != nil {
		t.Fatalf("daemon set: %v\n%s", err, out)
	}
	if !strings.Contains(out, "daemon not running") {
		t.Errorf("output should say the value applies at next start:\n%s", out)
	}
	p, _ := paths.New()
	cfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil || cfg.Daemon.MaxConcurrentRuns != 0 {
		t.Fatalf("persisted cap = %v, %v; want 0 (unlimited)", cfg.Daemon, err)
	}
}
