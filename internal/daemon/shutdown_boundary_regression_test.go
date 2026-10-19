package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// An independent registration failure must remain failed even when raw
// shutdown cancels its context before the background handler runs.
func TestRecoveredRegistrationPanicSurvivesImmediateRawCancellation(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, head := setupTestGitRepo(t, p, d, "registration-panic")
	run, err := d.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepResult(run.ID, types.StepReview); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewRunManager(d, p, nil)
	cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
	// Keep the caller on the only P until it has cancelled the newly admitted
	// handle. No sleeps or racing raw-shutdown DB writes choose the ordering.
	workDir := p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), workDir, head); err != nil {
		t.Fatal(err)
	}
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	mgr.resumeRecoveredRun(recoveredRunPlan{run: run, repo: repo, workDir: workDir, gateDir: p.RepoDir(repo.ID), cfg: cfg, agent: recoveredRunTestAgent{}, steps: []pipeline.Step{recoveredNamePanicStep{}}})
	mgr.mu.Lock()
	handle := mgr.runs[run.ID].handle
	handle.cancel(pipeline.ErrDaemonShutdown)
	mgr.mu.Unlock()
	mgr.Shutdown()
	select {
	case <-handle.done:
	case <-time.After(3 * time.Second):
		t.Fatal("registration failure retained custody after healthy settlement")
	}
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || !strings.Contains(*stored.Error, "internal panic") {
		t.Fatalf("independent panic must remain failed: %+v, %v", stored, err)
	}
}

// The OS timestamp is a tolerant identity check; the shutdown wire tuple
// must retain the exact timestamp published by the runtime, including nanos.
func TestCaptureRunningDaemonRetainsRecordedTimestamp(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	recorded := time.Now().UTC()
	writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: 424242, StartedAt: recorded, Instance: "runtime"})
	old := daemonProcessStartTime
	defer func() { daemonProcessStartTime = old }()
	daemonProcessStartTime = func(int) (time.Time, error) { return recorded.Add(-500 * time.Millisecond), nil }
	got := captureRunningDaemon(p)
	if got.pid != 424242 || got.instance != "runtime" || !got.startedAt.Equal(recorded) {
		t.Fatalf("capture lost runtime tuple: %+v; recorded %v", got, recorded)
	}
}

// The controlled-shutdown acknowledgement is the durable settlement contract.
// A supervisor bootout before it would send raw shutdown to a certified watcher.
func TestUninstallSettlesBeforeBootout(t *testing.T) {
	for _, mode := range []string{"settled", "refused", "successor", "successor during bootout", "successor holds lock"} {
		t.Run(mode, func(t *testing.T) {
			defer stubServiceRuntime(t)()
			runtimeGOOS = "darwin"
			home := t.TempDir()
			serviceUserHomeDir = func() (string, error) { return home, nil }
			serviceCurrentUser = func() (*user.User, error) { return &user.User{Uid: "501"}, nil }
			// Keep the socket path below the OS Unix-domain length limit.
			root, err := os.MkdirTemp("", "usb")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			p := paths.WithRoot(root)
			definition := launchAgentPath(p)
			if err := os.MkdirAll(filepath.Dir(definition), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(definition, []byte("service"), 0600); err != nil {
				t.Fatal(err)
			}
			started := time.Now().UTC()
			record := daemonPIDFile{PID: 424242, StartedAt: started, Instance: "runtime"}
			writeDaemonPIDRecord(t, p.PIDFile(), record)
			oldRunning, oldStart, oldKill := daemonProcessRunning, daemonProcessStartTime, daemonKillPID
			defer func() { daemonProcessRunning, daemonProcessStartTime, daemonKillPID = oldRunning, oldStart, oldKill }()
			var settled, exited atomic.Bool
			daemonProcessRunning = func(int) (bool, error) { return !exited.Load(), nil }
			daemonProcessStartTime = func(int) (time.Time, error) { return started.Add(-500 * time.Millisecond), nil }
			daemonKillPID = func(int) error {
				t.Error("uninstall must not kill before settlement")
				return errors.New("unexpected kill")
			}
			daemonHealthCheck = func(*paths.Paths, ...time.Duration) (bool, error) { return !exited.Load(), nil }
			var bootouts atomic.Int32
			serviceCommandRunner = func(string, ...string) ([]byte, error) {
				bootouts.Add(1)
				if !settled.Load() {
					t.Error("bootout preceded durable shutdown settlement")
				}
				exited.Store(true)
				if mode == "successor during bootout" {
					writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: 424243, StartedAt: started, Instance: "successor"})
				}
				return nil, nil
			}
			srv := ipc.NewServer()
			srv.Handle(ipc.MethodShutdown, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				var request ipc.ShutdownParams
				if err := json.Unmarshal(raw, &request); err != nil {
					return nil, err
				}
				if request.ExpectedPID != record.PID || request.ExpectedInstance != record.Instance || request.ExpectedStartedAt != started.Format(time.RFC3339Nano) {
					return nil, errors.New("runtime tuple mismatch")
				}
				if mode == "refused" {
					return nil, errors.New("settlement failed")
				}
				if mode == "successor" {
					writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: 424243, StartedAt: started, Instance: "successor"})
				}
				settled.Store(true)
				exited.Store(true)
				if mode == "successor holds lock" {
					lock, err := acquireSingletonLock(p)
					if err != nil {
						return nil, err
					}
					t.Cleanup(func() { lock.Release() })
				}
				return &ipc.ShutdownResult{OK: true, PID: record.PID, StartedAt: request.ExpectedStartedAt, Instance: record.Instance, Lifecycle: "settled"}, nil
			})
			if err := srv.Listen(p.Socket()); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- srv.ServeReady() }()
			defer func() { srv.Close(); <-done }()
			got, err := Uninstall(p)
			if mode == "settled" {
				if err != nil || got != definition || bootouts.Load() != 1 {
					t.Fatalf("uninstall = %q, %v; bootouts %d", got, err, bootouts.Load())
				}
				if _, err := os.Stat(definition); !os.IsNotExist(err) {
					t.Fatalf("definition retained after settled uninstall: %v", err)
				}
			} else {
				wantBootouts := int32(0)
				if mode == "successor during bootout" {
					wantBootouts = 1
				}
				if err == nil || got != "" || bootouts.Load() != wantBootouts {
					t.Fatalf("unsafe uninstall = %q, %v; bootouts %d", got, err, bootouts.Load())
				}
				if _, err := os.Stat(definition); err != nil {
					t.Fatalf("unsuccessful settlement removed definition: %v", err)
				}
			}
		})
	}
}

// Pre-instance daemons support only the non-preserving shutdown RPC. Permit
// that protocol only for a verified PID/start/root and an unchanged PID record.
func TestStopVerifiedLegacyDaemon(t *testing.T) {
	for _, mode := range []string{"legacy", "wrong root", "unverifiable start", "successor before request", "current protocol"} {
		t.Run(mode, func(t *testing.T) {
			defer stubServiceRuntime(t)()
			runtimeGOOS = "freebsd"
			root, err := os.MkdirTemp("", "lsd")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			p := paths.WithRoot(root)
			started := time.Now().UTC()
			record := daemonPIDFile{PID: 424242, StartedAt: started}
			writeDaemonPIDRecord(t, p.PIDFile(), record)
			oldRunning, oldStart, oldKill, oldList, oldDial := daemonProcessRunning, daemonProcessStartTime, daemonKillPID, daemonListDaemonProcesses, daemonDial
			defer func() {
				daemonProcessRunning, daemonProcessStartTime, daemonKillPID, daemonListDaemonProcesses, daemonDial = oldRunning, oldStart, oldKill, oldList, oldDial
			}()
			var exited atomic.Bool
			daemonProcessRunning = func(int) (bool, error) { return !exited.Load(), nil }
			daemonProcessStartTime = func(int) (time.Time, error) {
				if mode == "unverifiable start" {
					return time.Time{}, errors.New("cannot inspect")
				}
				return started, nil
			}
			daemonKillPID = func(int) error {
				t.Error("unverified legacy fallback killed a process")
				return errors.New("unexpected kill")
			}
			daemonListDaemonProcesses = func() ([]daemonProcessInfo, error) {
				processRoot := root
				if mode == "wrong root" {
					processRoot = root + "-other"
				}
				return []daemonProcessInfo{{PID: record.PID, Root: processRoot}}, nil
			}
			if mode == "successor before request" {
				daemonDial = func(socket string) (*ipc.Client, error) {
					writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: 424243, StartedAt: started, Instance: "successor"})
					return oldDial(socket)
				}
			}
			var calls atomic.Int32
			srv := ipc.NewServer()
			srv.Handle(ipc.MethodShutdown, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
				calls.Add(1)
				var request ipc.ShutdownParams
				if err := json.Unmarshal(raw, &request); err != nil {
					return nil, err
				}
				if request.ExpectedPID != 0 || request.ExpectedInstance != "" || request.ExpectedStartedAt != "" {
					t.Error("legacy shutdown requested certified preservation")
				}
				if mode == "current protocol" {
					return nil, errors.New("captured runtime identity is required")
				}
				exited.Store(true)
				return &ipc.ShutdownResult{OK: true}, nil
			})
			if err := srv.Listen(p.Socket()); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- srv.ServeReady() }()
			defer func() { srv.Close(); <-done }()
			err = Stop(p)
			if mode == "legacy" {
				if err != nil || calls.Load() != 1 {
					t.Fatalf("verified legacy stop: %v; RPC calls %d", err, calls.Load())
				}
				if _, err := os.Stat(p.PIDFile()); !os.IsNotExist(err) {
					t.Fatalf("stopped legacy PID record retained: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("unverified legacy shutdown reported success")
				}
				wantCalls := int32(0)
				if mode == "current protocol" {
					wantCalls = 1
				}
				if calls.Load() != wantCalls {
					t.Fatalf("unverified shutdown RPC calls %d, want %d", calls.Load(), wantCalls)
				}
				got, err := readDaemonPIDFile(p.PIDFile())
				if err != nil || (mode == "successor before request" && got.Instance != "successor") {
					t.Fatalf("unverified stop changed artifacts: %+v, %v", got, err)
				}
			}
		})
	}
}
