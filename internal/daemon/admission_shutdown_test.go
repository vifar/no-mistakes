package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestControlledShutdown_DrainsInflightAdmissionBeforeStopHandoff(t *testing.T) {
	for _, stop := range []struct {
		name string
		call func(*paths.Paths) error
	}{{"stop", Stop}, {"managed refresh", stopCurrentDaemonBeforeManagedRestart}} {
		for _, stage := range []string{"executor construction", "setup failure"} {
			t.Run(stop.name+"/"+stage, func(t *testing.T) {
				root, err := os.MkdirTemp("", "admission")
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
				repo, head := setupTestGitRepo(t, p, d, "admission")
				t.Setenv("NM_DEMO", "1")
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
				if stage == "setup failure" {
					realGit, err := testgit.RealGit()
					if err != nil {
						t.Fatal(err)
					}
					bin := t.TempDir()
					marker := filepath.Join(bin, "entered")
					released := filepath.Join(bin, "release")
					script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = fetch ]; then\ntrap '' TERM\n: > %q\nwhile [ ! -f %q ]; do sleep 0.05; done\nexit 17\nfi\nexec %q \"$@\"\n", marker, released, realGit)
					if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
						t.Fatal(err)
					}
					t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
					unblock = func() { once.Do(func() { os.WriteFile(released, []byte("release"), 0600); close(release) }) }
					defer unblock()
					go func() {
						for {
							if _, err := os.Stat(marker); err == nil {
								close(entered)
								return
							}
							select {
							case <-release:
								return
							case <-time.After(10 * time.Millisecond):
							}
						}
					}()
				}
				startDone := make(chan error, 1)
				go func() {
					_, err := mgr.startRun(context.Background(), repo, "main", head, head, "test", nil, "intent", "", false, "", nil)
					startDone <- err
				}()
				select {
				case <-entered:
				case err := <-startDone:
					t.Fatalf("admission failed before barrier: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("setup did not reach barrier")
				}
				active, err := d.GetActiveRun(repo.ID, "main")
				if err != nil || active == nil || active.Status != types.RunPending || active.WorktreeDir == nil {
					t.Fatalf("barrier lacks durable pending setup: %+v,%v", active, err)
				}
				worktree := *active.WorktreeDir
				if _, err := os.Stat(worktree); err != nil {
					t.Fatal(err)
				}
				srv := ipc.NewServer()
				var handoff atomic.Bool
				registerHandlers(srv, mgr, d, func() { handoff.Store(true); _ = os.Remove(p.PIDFile()) })
				if err := srv.Listen(p.Socket()); err != nil {
					t.Fatal(err)
				}
				serving := make(chan error, 1)
				go func() { serving <- srv.ServeReady() }()
				defer func() { srv.Close(); <-serving }()
				writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: os.Getpid(), StartedAt: mgr.runtimeStartedAt, Instance: mgr.runtimeInstance})
				oldRunning, oldStart, oldKill := daemonProcessRunning, daemonProcessStartTime, daemonKillPID
				defer func() { daemonProcessRunning, daemonProcessStartTime, daemonKillPID = oldRunning, oldStart, oldKill }()
				daemonProcessRunning = func(int) (bool, error) { return !handoff.Load(), nil }
				daemonProcessStartTime = func(int) (time.Time, error) { return mgr.runtimeStartedAt, nil }
				daemonKillPID = func(int) error { t.Error("unverified kill attempted"); return fmt.Errorf("refused") }
				stopped := make(chan error, 1)
				go func() { stopped <- stop.call(p) }()
				deadline := time.After(10 * time.Second)
				for !mgr.shuttingDown.Load() {
					select {
					case <-deadline:
						t.Fatal("shutdown did not prepare")
					case <-time.After(time.Millisecond):
					}
				}
				select {
				case err := <-stopped:
					t.Errorf("stop returned before admitted setup settled: %v", err)
				case <-time.After(100 * time.Millisecond):
				}
				if handoff.Load() {
					t.Error("successful ACK handed off before admitted setup settled")
				}
				newID, newErr := mgr.startRun(context.Background(), repo, "other", head, head, "test", nil, "intent", "", false, "", nil)
				if newErr == nil || newID != "" {
					t.Errorf("new admission during shutdown = %q,%v", newID, newErr)
				}
				unblock()
				select {
				case err := <-startDone:
					if err == nil {
						t.Error("admitted setup was not rejected")
					}
				case <-time.After(10 * time.Second):
					t.Fatal("admitted setup did not settle")
				}
				select {
				case err := <-stopped:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("planned stop did not drain")
				}
				if _, err := os.Stat(worktree); !os.IsNotExist(err) {
					t.Errorf("setup worktree remains after ACK: %v", err)
				}
				reopened, err := db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				run, err := reopened.GetRun(active.ID)
				if err != nil || run.Status != types.RunFailed || run.Error == nil {
					t.Errorf("ACK left nonterminal setup: %+v,%v", run, err)
				}
			})
		}
	}
}
