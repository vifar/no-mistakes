package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDaemonStop_CapturedExitIgnoresSuccessorHealth(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	oldHealth, oldRunning, oldStart, oldKill := daemonHealthCheck, daemonProcessRunning, daemonProcessStartTime, daemonKillPID
	t.Cleanup(func() {
		daemonHealthCheck, daemonProcessRunning, daemonProcessStartTime, daemonKillPID = oldHealth, oldRunning, oldStart, oldKill
	})
	t.Setenv("NM_TEST_DAEMON_STOP_TIMEOUT", "20ms")
	started := time.Now().UTC()
	successor := daemonPIDFile{PID: 9822, StartedAt: started.Add(time.Second)}
	writeDaemonPIDRecord(t, p.PIDFile(), successor)
	if err := os.WriteFile(p.Socket(), []byte("successor"), 0600); err != nil {
		t.Fatal(err)
	}
	daemonHealthCheck = func(*paths.Paths, ...time.Duration) (bool, error) { return true, nil }
	daemonProcessRunning = func(pid int) (bool, error) { return pid == successor.PID, nil }
	daemonProcessStartTime = func(pid int) (time.Time, error) {
		if pid == successor.PID {
			return successor.StartedAt, nil
		}
		return started, nil
	}
	daemonKillPID = func(pid int) error {
		t.Errorf("must not kill exited capture or successor: %d", pid)
		return fmt.Errorf("unexpected kill")
	}
	if err := waitForDaemonStop(p, daemonInstance{pid: 9811, startedAt: started}); err != nil {
		t.Fatal(err)
	}
	got, err := readDaemonPIDFile(p.PIDFile())
	if err != nil || got.PID != successor.PID {
		t.Fatalf("successor record removed: %+v,%v", got, err)
	}
	data, err := os.ReadFile(p.Socket())
	if err != nil || string(data) != "successor" {
		t.Fatalf("successor socket removed: %q,%v", data, err)
	}
}

func TestPlannedStop_RequiresCapturedIdentityAndMatchingAcknowledgement(t *testing.T) {
	for _, entry := range []struct {
		name string
		stop func(*paths.Paths) error
	}{{"stop", Stop}, {"managed refresh", stopCurrentDaemonBeforeManagedRestart}} {
		for _, mode := range []string{"missing capture", "missing start", "missing instance", "replacement before dial", "incomplete ack", "changed ack start", "changed ack instance", "preparation failed", "lost ack"} {
			t.Run(entry.name+"/"+mode, func(t *testing.T) {
				root, err := os.MkdirTemp("", "ack")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(root)
				p := paths.WithRoot(root)
				if err := p.EnsureDirs(); err != nil {
					t.Fatal(err)
				}
				started := time.Now().UTC()
				record := daemonPIDFile{PID: 424242, StartedAt: started, Instance: "captured-runtime"}
				if mode == "missing start" {
					record.StartedAt = time.Time{}
				}
				if mode == "missing instance" {
					record.Instance = ""
				}
				if mode != "missing capture" {
					writeDaemonPIDRecord(t, p.PIDFile(), record)
				}
				oldHealth, oldRunning, oldStart, oldKill, oldDial := daemonHealthCheck, daemonProcessRunning, daemonProcessStartTime, daemonKillPID, daemonDial
				t.Cleanup(func() {
					daemonHealthCheck, daemonProcessRunning, daemonProcessStartTime, daemonKillPID = oldHealth, oldRunning, oldStart, oldKill
					daemonDial = oldDial
				})
				var exited atomic.Bool
				var calls atomic.Int32
				daemonHealthCheck = func(*paths.Paths, ...time.Duration) (bool, error) { return true, nil }
				daemonProcessRunning = func(pid int) (bool, error) { return pid == 424243 || !exited.Load(), nil }
				if mode == "replacement before dial" {
					daemonDial = func(socket string) (*ipc.Client, error) {
						writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: 424243, StartedAt: started.Add(time.Second), Instance: "successor"})
						return oldDial(socket)
					}
				}
				daemonProcessStartTime = func(int) (time.Time, error) { return started, nil }
				daemonKillPID = func(pid int) error {
					t.Errorf("unverified shutdown must not kill pid %d", pid)
					return fmt.Errorf("unexpected kill")
				}
				srv := ipc.NewServer()
				srv.Handle(ipc.MethodShutdown, func(ctx context.Context, raw json.RawMessage) (interface{}, error) {
					calls.Add(1)
					var request ipc.ShutdownParams
					if err := json.Unmarshal(raw, &request); err != nil {
						return nil, err
					}
					if request.ExpectedPID != record.PID || request.ExpectedStartedAt != started.Format(time.RFC3339Nano) || request.ExpectedInstance != "captured-runtime" {
						t.Errorf("request lost captured tuple: %+v", request)
					}
					exited.Store(true)
					if mode == "replacement before dial" {
						return nil, fmt.Errorf("shutdown targets a different daemon instance")
					}
					if mode == "preparation failed" || mode == "lost ack" {
						return nil, fmt.Errorf("%s", mode)
					}
					ack := ipc.ShutdownResult{OK: true, PID: record.PID, StartedAt: started.Format(time.RFC3339Nano), Instance: record.Instance, Lifecycle: "prepared-lifecycle"}
					switch mode {
					case "incomplete ack":
						ack.Lifecycle = ""
					case "changed ack start":
						ack.StartedAt = started.Add(time.Second).Format(time.RFC3339Nano)
					case "changed ack instance":
						ack.Instance = "successor"
					}
					return ack, nil
				})
				if mode == "lost ack" {
					ln, err := net.Listen("unix", p.Socket())
					if err != nil {
						t.Fatal(err)
					}
					done := make(chan struct{})
					go func() {
						defer close(done)
						conn, err := ln.Accept()
						if err != nil {
							return
						}
						defer conn.Close()
						var request ipc.Request
						if err := json.NewDecoder(conn).Decode(&request); err != nil {
							t.Error(err)
							return
						}
						calls.Add(1)
						exited.Store(true)
						// The peer exits after preparation without writing an ACK.
					}()
					defer func() { ln.Close(); <-done }()
				} else {
					if err := srv.Listen(p.Socket()); err != nil {
						t.Fatal(err)
					}
					done := make(chan error, 1)
					go func() { done <- srv.ServeReady() }()
					defer func() { srv.Close(); <-done }()
				}
				err = entry.stop(p)
				if err == nil {
					t.Fatal("unverified preparation/acknowledgement reported success")
				}
				missing := strings.HasPrefix(mode, "missing")
				if missing && calls.Load() != 0 {
					t.Fatalf("missing capture sent preserving RPC (%d)", calls.Load())
				}
				if !missing && calls.Load() != 1 {
					t.Fatalf("captured identity was not prepared: %d", calls.Load())
				}
				if mode == "replacement before dial" {
					got, err := readDaemonPIDFile(p.PIDFile())
					if err != nil || got.Instance != "successor" {
						t.Fatalf("successor artifacts lost: %+v,%v", got, err)
					}
				}
			})
		}
	}
}

func TestControlledShutdown_ReasonIsDurableBeforeCancellationAndAcknowledgement(t *testing.T) {
	root, err := os.MkdirTemp("", "csd")
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
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	mgr := NewRunManager(d, p, nil)
	mgr.runtimeStartedAt = time.Now().UTC()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	mgr.cancels[run.ID] = cancel
	mgr.wg.Add(1)
	observed := make(chan string, 1)
	go func() {
		defer mgr.wg.Done()
		<-ctx.Done()
		cause := context.Cause(ctx)
		if planned, ok := cause.(*pipeline.ControlledShutdownCause); ok {
			matches, err := d.ControlledShutdownMatches(run.ID, planned.Instance, planned.Lifecycle, planned.Error())
			if err != nil || !matches {
				t.Errorf("cancelled before durable reason: %v,%v", matches, err)
			}
		}
		if err := d.UpdateRunErrorStatus(run.ID, cause.Error(), types.RunCancelled); err != nil {
			t.Error(err)
		}
		observed <- cause.Error()
	}()
	srv := ipc.NewServer()
	registerHandlers(srv, mgr, d, func() { mgr.Shutdown() })
	if err := srv.Listen(p.Socket()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.ServeReady() }()
	defer func() { srv.Close(); <-done }()
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	var result ipc.ShutdownResult
	err = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{ExpectedPID: os.Getpid(), ExpectedStartedAt: mgr.runtimeStartedAt.Format(time.RFC3339Nano), ExpectedInstance: mgr.runtimeInstance}, &result)
	client.Close()
	if err != nil {
		t.Fatal(err)
	}
	if result.Instance == "" || result.Lifecycle == "" {
		t.Fatalf("controlled shutdown acknowledged without durable identity: %+v", result)
	}
	select {
	case reason := <-observed:
		matches, err := d.ControlledShutdownMatches(run.ID, result.Instance, result.Lifecycle, reason)
		if err != nil || !matches {
			t.Fatalf("reason was not durable before cancellation/ack: %q,%v,%v", reason, matches, err)
		}
	default:
		t.Fatal("acknowledged before cancellation drained")
	}
}

func TestControlledShutdown_RejectsIncompleteOrChangedRuntimeIdentity(t *testing.T) {
	for _, change := range []string{"missing pid", "missing start", "missing instance", "changed start", "changed instance"} {
		t.Run(change, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			mgr := NewRunManager(d, p, nil)
			mgr.runtimeStartedAt = time.Now().UTC()
			request := ipc.ShutdownParams{ExpectedPID: os.Getpid(), ExpectedStartedAt: mgr.runtimeStartedAt.Format(time.RFC3339Nano), ExpectedInstance: mgr.runtimeInstance}
			switch change {
			case "missing pid":
				request.ExpectedPID = 0
			case "missing start":
				request.ExpectedStartedAt = ""
			case "missing instance":
				request.ExpectedInstance = ""
			case "changed start":
				request.ExpectedStartedAt = mgr.runtimeStartedAt.Add(time.Second).Format(time.RFC3339Nano)
			case "changed instance":
				request.ExpectedInstance = newShutdownToken()
			}
			result, err := mgr.prepareControlledShutdown(context.Background(), request)
			if err == nil || result != nil || mgr.shuttingDown.Load() {
				t.Fatalf("unverified identity prepared shutdown: result=%+v,err=%v,shutdown=%v", result, err, mgr.shuttingDown.Load())
			}
		})
	}
}
