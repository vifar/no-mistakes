package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestSupervisorCleanupExcludesSuccessors(t *testing.T) {
	for _, action := range []string{"stop", "managed refresh", "uninstall"} {
		for _, mode := range []string{"acknowledged exit", "stale artifacts", "no artifacts", "successor before cleanup", "successor starts during cleanup", "invalid acknowledgement", "failed request"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				defer stubServiceRuntime(t)()
				runtimeGOOS = "darwin"
				home := t.TempDir()
				serviceUserHomeDir = func() (string, error) { return home, nil }
				serviceCurrentUser = func() (*user.User, error) { return &user.User{Uid: "501"}, nil }
				root, err := os.MkdirTemp("", "sce")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(root)
				p := paths.WithRoot(root)
				if err := p.EnsureDirs(); err != nil {
					t.Fatal(err)
				}
				definition := launchAgentPath(p)
				if err := os.MkdirAll(filepath.Dir(definition), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(definition, []byte("service"), 0600); err != nil {
					t.Fatal(err)
				}
				record := daemonPIDFile{PID: 424242, StartedAt: time.Now().UTC(), Instance: "captured"}
				var exited atomic.Bool
				exited.Store(mode == "stale artifacts" || mode == "no artifacts")
				oldRunning, oldStart, oldKill := daemonProcessRunning, daemonProcessStartTime, daemonKillPID
				defer func() { daemonProcessRunning, daemonProcessStartTime, daemonKillPID = oldRunning, oldStart, oldKill }()
				daemonProcessRunning = func(int) (bool, error) { return !exited.Load(), nil }
				daemonProcessStartTime = func(int) (time.Time, error) { return record.StartedAt, nil }
				daemonKillPID = func(pid int) error {
					t.Errorf("supervisor cleanup attempted to kill PID %d", pid)
					return errors.New("unexpected kill")
				}
				if mode != "no artifacts" {
					writeDaemonPIDRecord(t, p.PIDFile(), record)
				}
				var commands atomic.Int32
				serviceCommandRunner = func(string, ...string) ([]byte, error) {
					commands.Add(1)
					if !exited.Load() {
						t.Error("supervisor stop preceded captured exit")
					}
					successorLock, err := acquireSingletonLock(p)
					if err == nil {
						successorLock.Release()
						t.Error("successor acquired singleton ownership during supervisor cleanup")
					} else if !errors.Is(err, ErrSingletonLockHeld) {
						t.Errorf("unexpected lock refusal: %v", err)
					}
					if mode == "successor starts during cleanup" {
						writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: 424243, StartedAt: record.StartedAt, Instance: "successor"})
					}
					return nil, nil
				}
				if mode != "stale artifacts" && mode != "no artifacts" {
					srv := ipc.NewServer()
					srv.Handle(ipc.MethodShutdown, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
						var request ipc.ShutdownParams
						if err := json.Unmarshal(raw, &request); err != nil {
							return nil, err
						}
						if request.ExpectedPID != record.PID || request.ExpectedInstance != record.Instance || request.ExpectedStartedAt != record.StartedAt.Format(time.RFC3339Nano) {
							return nil, errors.New("captured identity lost")
						}
						exited.Store(true)
						if mode == "failed request" {
							return nil, errors.New("settlement unavailable")
						}
						if mode == "successor before cleanup" {
							lock, err := acquireSingletonLock(p)
							if err != nil {
								return nil, err
							}
							t.Cleanup(func() { lock.Release() })
							writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: 424243, StartedAt: record.StartedAt, Instance: "successor"})
						}
						ack := &ipc.ShutdownResult{OK: true, PID: record.PID, StartedAt: request.ExpectedStartedAt, Instance: record.Instance, Lifecycle: "settled"}
						if mode == "invalid acknowledgement" {
							ack.Instance = "wrong"
						}
						return ack, nil
					})
					if err := srv.Listen(p.Socket()); err != nil {
						t.Fatal(err)
					}
					done := make(chan error, 1)
					go func() { done <- srv.ServeReady() }()
					defer func() { srv.Close(); <-done }()
				} else if mode == "stale artifacts" {
					if err := os.WriteFile(p.Socket(), []byte("stale socket"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				var result error
				switch action {
				case "stop":
					result = Stop(p)
				case "managed refresh":
					result = stopCurrentDaemonBeforeManagedRestart(p)
				case "uninstall":
					_, result = Uninstall(p)
				}
				fail := mode == "successor before cleanup" || mode == "successor starts during cleanup" || mode == "invalid acknowledgement" || mode == "failed request"
				if (result != nil) != fail {
					t.Fatalf("cleanup result: %v; failure expected %v", result, fail)
				}
				wantCommands := int32(1)
				if mode == "successor before cleanup" || mode == "invalid acknowledgement" || mode == "failed request" {
					wantCommands = 0
				}
				if commands.Load() != wantCommands {
					t.Fatalf("supervisor calls %d, want %d", commands.Load(), wantCommands)
				}
				_, definitionErr := os.Stat(definition)
				if action == "uninstall" && !fail {
					if !os.IsNotExist(definitionErr) {
						t.Fatalf("settled uninstall retained definition: %v", definitionErr)
					}
				} else if definitionErr != nil {
					t.Fatalf("cleanup removed retained service: %v", definitionErr)
				}
				if fail {
					saved, err := readDaemonPIDFile(p.PIDFile())
					if err != nil {
						t.Fatal(err)
					}
					wanted := record.Instance
					if mode == "successor before cleanup" || mode == "successor starts during cleanup" {
						wanted = "successor"
					}
					if saved.Instance != wanted {
						t.Fatalf("failed cleanup changed identity: %+v, want %s", saved, wanted)
					}
				}
				if !fail {
					lock, err := acquireSingletonLock(p)
					if err != nil {
						t.Fatal(fmt.Errorf("cleanup leaked exclusion: %w", err))
					}
					lock.Release()
				}
			})
		}
	}
}
