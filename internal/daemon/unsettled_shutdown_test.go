package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestPlannedStop_UnsettledAdmissionDoesNotStopSupervisor(t *testing.T) {
	for _, stop := range []struct {
		name string
		call func(*paths.Paths) error
	}{{"stop", Stop}, {"managed refresh", stopCurrentDaemonBeforeManagedRestart}} {
		t.Run(stop.name, func(t *testing.T) {
			root, err := os.MkdirTemp("", "unsettled")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			p := paths.WithRoot(root)
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			repo, head := setupTestGitRepo(t, p, d, "unsettled")
			t.Setenv("NM_DEMO", "1")
			t.Setenv("NM_TEST_DAEMON_STOP_TIMEOUT", "20ms")
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			mgr := NewRunManager(d, p, func() []pipeline.Step {
				close(entered)
				<-release
				return []pipeline.Step{&mockPassStep{name: types.StepReview}}
			})
			mgr.runtimeStartedAt = time.Now().UTC()
			startDone := make(chan error, 1)
			go func() {
				_, err := mgr.startRun(context.Background(), repo, "main", head, head, "test", nil, "intent", "", false, "", "", nil)
				startDone <- err
			}()
			select {
			case <-entered:
			case err := <-startDone:
				t.Fatalf("startup failed before barrier: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("setup did not start")
			}
			run, err := d.GetActiveRun(repo.ID, "main")
			if err != nil || run == nil || run.Status != types.RunPending {
				t.Fatalf("durable pending row missing: %+v,%v", run, err)
			}
			cleanup := stubServiceRuntime(t)
			defer cleanup()
			runtimeGOOS = "linux"
			home := t.TempDir()
			serviceUserHomeDir = func() (string, error) { return home, nil }
			unit := filepath.Join(home, ".config", "systemd", "user", systemdServiceName(p))
			if err := os.MkdirAll(filepath.Dir(unit), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(unit, []byte("WorkingDirectory="+p.Root()+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var supervisorStops atomic.Int32
			serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), "stop ") {
					supervisorStops.Add(1)
				}
				return nil, nil
			}
			oldRunning, oldStart, oldKill := daemonProcessRunning, daemonProcessStartTime, daemonKillPID
			defer func() { daemonProcessRunning, daemonProcessStartTime, daemonKillPID = oldRunning, oldStart, oldKill }()
			daemonProcessRunning = func(int) (bool, error) { return true, nil }
			daemonProcessStartTime = func(int) (time.Time, error) { return mgr.runtimeStartedAt, nil }
			daemonKillPID = func(int) error { t.Error("unsettled live daemon reached PID fallback"); return fmt.Errorf("refused") }
			writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: os.Getpid(), StartedAt: mgr.runtimeStartedAt, Instance: mgr.runtimeInstance})
			srv := ipc.NewServer()
			if err := srv.Listen(p.Socket()); err != nil {
				t.Fatal(err)
			}
			var latestACK ipc.ShutdownResult
			var ackMu sync.Mutex
			srv.Handle(ipc.MethodShutdown, func(ctx context.Context, raw json.RawMessage) (interface{}, error) {
				var request ipc.ShutdownParams
				if err := json.Unmarshal(raw, &request); err != nil {
					return nil, err
				}
				bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
				ack, err := mgr.prepareControlledShutdown(bounded, request)
				if err == nil {
					ackMu.Lock()
					latestACK = *ack
					ackMu.Unlock()
					_ = os.Remove(p.PIDFile())
				}
				return ack, err
			})
			serving := make(chan error, 1)
			go func() { serving <- srv.ServeReady() }()
			defer func() { srv.Close(); <-serving }()
			if err := stop.call(p); err == nil || !strings.Contains(err.Error(), "did not drain") {
				t.Errorf("unsettled planned stop = %v", err)
			}
			if supervisorStops.Load() != 0 {
				t.Errorf("unsettled setup triggered %d supervisor stops", supervisorStops.Load())
			}
			stored, err := d.GetRun(run.ID)
			if err != nil || stored.Status != types.RunPending {
				t.Errorf("blocked setup was not still pending: %+v,%v", stored, err)
			}
			if _, err := os.Stat(*run.WorktreeDir); err != nil {
				t.Error("resource disappeared before settlement")
			}
			unblock()
			select {
			case err := <-startDone:
				if err == nil {
					t.Error("startup was not rejected")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("setup did not settle")
			}
			mgr.wg.Wait()
			reopened, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			stored, err = reopened.GetRun(run.ID)
			if err != nil || stored.Status != types.RunCancelled {
				t.Errorf("setup failed to settle durably: %+v,%v", stored, err)
			}
			if _, err := os.Stat(*run.WorktreeDir); !os.IsNotExist(err) {
				t.Errorf("setup resources remain after settlement: %v", err)
			}
			if mgr.shutdownState.Load() == shutdownIdle {
				t.Error("failed preparation reopened admission")
			}
			if id, err := mgr.startRun(context.Background(), repo, "other", head, head, "test", nil, "intent", "", false, "", "", nil); err == nil || id != "" {
				t.Errorf("fenced admission = %q,%v", id, err)
			}
			if err := stop.call(p); err != nil {
				t.Errorf("settled matching retry failed: %v", err)
			}
			if supervisorStops.Load() != 1 {
				t.Errorf("settled retry supervisor count = %d", supervisorStops.Load())
			}
			ackMu.Lock()
			ack := latestACK
			ackMu.Unlock()
			if !ack.OK || ack.Instance != mgr.runtimeInstance || ack.Lifecycle == "" {
				t.Errorf("retry lacks complete ACK: %+v", ack)
			}
			matches, err := reopened.ControlledShutdownMatches(run.ID, ack.Instance, ack.Lifecycle, "cancelled: planned daemon shutdown")
			if err != nil || !matches {
				t.Errorf("retry changed durable lifecycle: %v,%v", matches, err)
			}

		})
	}
}

func TestPlannedStop_VerifiedSettlementAllowsCapturedPIDFallback(t *testing.T) {
	for _, stop := range []struct {
		name string
		call func(*paths.Paths) error
	}{{"stop", Stop}, {"managed refresh", stopCurrentDaemonBeforeManagedRestart}} {
		for _, serviceFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/service_failure_%v", stop.name, serviceFails), func(t *testing.T) {
				root, err := os.MkdirTemp("", "settled")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(root)
				p := paths.WithRoot(root)
				if err := p.EnsureDirs(); err != nil {
					t.Fatal(err)
				}
				cleanup := stubServiceRuntime(t)
				defer cleanup()
				runtimeGOOS = "linux"
				home := t.TempDir()
				serviceUserHomeDir = func() (string, error) { return home, nil }
				unit := filepath.Join(home, ".config", "systemd", "user", systemdServiceName(p))
				if err := os.MkdirAll(filepath.Dir(unit), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(unit, []byte("WorkingDirectory="+p.Root()+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("NM_TEST_DAEMON_STOP_TIMEOUT", "20ms")
				started := time.Now().UTC()
				record := daemonPIDFile{PID: 424242, StartedAt: started, Instance: "captured-runtime"}
				writeDaemonPIDRecord(t, p.PIDFile(), record)
				var killed atomic.Bool
				oldRunning, oldStart, oldKill := daemonProcessRunning, daemonProcessStartTime, daemonKillPID
				defer func() { daemonProcessRunning, daemonProcessStartTime, daemonKillPID = oldRunning, oldStart, oldKill }()
				daemonProcessRunning = func(pid int) (bool, error) {
					if pid != record.PID {
						t.Errorf("followed another pid: %d", pid)
					}
					return !killed.Load(), nil
				}
				daemonProcessStartTime = func(int) (time.Time, error) { return started, nil }
				daemonKillPID = func(pid int) error {
					if pid != record.PID {
						t.Errorf("killed successor: %d", pid)
					}
					killed.Store(true)
					return nil
				}
				var acknowledged atomic.Bool
				var supervisorStops atomic.Int32
				serviceCommandRunner = func(_ string, args ...string) ([]byte, error) {
					if !acknowledged.Load() {
						t.Error("supervisor cleanup preceded verified ACK")
					}
					if strings.Contains(strings.Join(args, " "), "stop ") {
						supervisorStops.Add(1)
					}
					if serviceFails {
						return nil, fmt.Errorf("supervisor cleanup failed")
					}
					return nil, nil
				}
				srv := ipc.NewServer()
				srv.Handle(ipc.MethodShutdown, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
					var request ipc.ShutdownParams
					if err := json.Unmarshal(raw, &request); err != nil {
						return nil, err
					}
					if request.ExpectedPID != record.PID || request.ExpectedStartedAt != started.Format(time.RFC3339Nano) || request.ExpectedInstance != record.Instance {
						t.Error("captured request tuple changed")
					}
					acknowledged.Store(true)
					return &ipc.ShutdownResult{OK: true, PID: record.PID, StartedAt: request.ExpectedStartedAt, Instance: record.Instance, Lifecycle: "settled-lifecycle"}, nil
				})
				if err := srv.Listen(p.Socket()); err != nil {
					t.Fatal(err)
				}
				serving := make(chan error, 1)
				go func() { serving <- srv.ServeReady() }()
				defer func() { srv.Close(); <-serving }()
				err = stop.call(p)
				if serviceFails {
					if err == nil || !strings.Contains(err.Error(), "supervisor cleanup failed") {
						t.Errorf("cleanup failure lost: %v", err)
					}
				} else if err != nil {
					t.Error(err)
				}
				if !killed.Load() || supervisorStops.Load() != 1 {
					t.Errorf("settled capture cleanup = killed:%v stops:%d", killed.Load(), supervisorStops.Load())
				}
			})
		}
	}
}
