package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestSetupFailurePreservesCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX git shim")
	}
	for _, mode := range []string{"queued", "inline"} {
		for _, action := range []string{"abort", "supersede", "error"} {
			t.Run(mode+"/"+action, func(t *testing.T) {
				f := newFaultFixture(t, "1")
				var id string
				var start func()
				launchDone := make(chan struct{})
				if mode == "queued" {
					holder := f.push("holder")
					f.waitStarted(holder)
					id = f.push("setup")
					start = func() {
						f.step.release(holder)
						go func() { f.mgr.wg.Wait(); close(launchDone) }()
					}
				} else {
					repo, err := f.d.GetRepo("fault-repo")
					if err != nil {
						t.Fatal(err)
					}
					run, err := f.d.InsertRun(repo.ID, "setup", f.head, f.head)
					if err != nil {
						t.Fatal(err)
					}
					id = run.ID
					cfg, err := config.LoadGlobal(f.p.ConfigFile())
					if err != nil {
						t.Fatal(err)
					}
					slot, admitted := f.mgr.sched.tryAdmit(id)
					if !admitted {
						t.Fatal("inline launch was not admitted")
					}
					start = func() {
						go func() {
							defer close(launchDone)
							_, _ = f.mgr.launchRun(context.Background(), repo, run, cfg, "test", nil, slot, nil)
						}()
					}
				}
				sub, err := f.mgr.Subscribe(id)
				if err != nil {
					t.Fatal(err)
				}
				defer sub.Close()
				initialCtx, initialCancel := context.WithTimeout(context.Background(), time.Second)
				initial, ok := sub.Next(initialCtx)
				initialCancel()
				if !ok || initial.Type != ipc.EventStreamGap {
					t.Fatalf("subscription did not open with reconciliation: %+v", initial)
				}
				realGit, err := testgit.RealGit()
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				marker := filepath.Join(bin, "worktree-started")
				operation := "exec sleep 60"
				if action == "error" {
					operation = "echo 'injected worktree failure' >&2; exit 1"
				}
				script := fmt.Sprintf("#!/bin/sh\nprevious=''\nfor arg in \"$@\"; do\n if [ \"$previous\" = worktree ] && [ \"$arg\" = add ]; then\n  touch '%s'\n  %s\n fi\n previous=\"$arg\"\ndone\nexec '%s' \"$@\"\n", marker, operation, realGit)
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				start()
				f.waitFor("worktree setup stub", func() bool { _, err := os.Stat(marker); return err == nil })
				wantStatus := types.RunCancelled
				wantReason := types.RunCancelReasonAbortedByUser
				switch action {
				case "abort":
					err = f.mgr.HandleCancel(id)
				case "supersede":
					wantReason = types.RunCancelReasonSuperseded
					err = f.mgr.cancelActiveRuns("fault-repo", "setup")
				case "error":
					wantStatus = types.RunFailed
					wantReason = "injected worktree failure"
				}
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-launchDone:
				case <-time.After(30 * time.Second):
					t.Fatal("setup did not stop")
				}
				stored, err := f.d.GetRun(id)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Status != wantStatus || stored.Error == nil || !strings.Contains(*stored.Error, wantReason) {
					t.Fatalf("setup outcome = %s, %v; want %s with %q", stored.Status, stored.Error, wantStatus, wantReason)
				}
				if f.started(id) {
					t.Fatal("failed setup reached the executor")
				}
				if stats := f.mgr.sched.stats(); stats.Active != 0 {
					t.Fatalf("setup retained a slot: %+v", stats)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				terminalEvents := 0
				for {
					event, ok := sub.Next(ctx)
					if !ok {
						break
					}
					if event.Type == ipc.EventRunCompleted {
						terminalEvents++
						if event.Status == nil || *event.Status != string(stored.Status) || event.Error == nil || *event.Error != *stored.Error {
							t.Fatalf("terminal event disagrees with saved outcome: %+v", event)
						}
					}
				}
				if terminalEvents != 1 {
					t.Fatalf("terminal events = %d, want 1", terminalEvents)
				}
			})
		}
	}
}

func TestQueuedSetupConfigFailureReleasesBranchCustody(t *testing.T) {
	f := newFaultFixture(t, "1")
	holder := f.push("holder")
	f.waitStarted(holder)
	id := f.push("queued")
	if f.status(id) != types.RunQueued {
		t.Fatal("run did not queue")
	}
	repo, err := f.d.GetRepo("fault-repo")
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "queued")
	if err := os.WriteFile(f.p.ConfigFile(), []byte("daemon: [invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.step.release(holder)
	f.waitFor("queued configuration failure", func() bool { return f.status(id) == types.RunFailed })
	run, err := f.d.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if run.WorktreeDir != nil || run.TerminalHeadVerifiedAt == nil || run.LastPushedSHA != nil || run.PushGeneration != nil || run.PushActive || f.started(id) {
		t.Fatalf("never-started setup failure lost its release proof: %+v", run)
	}
	svc := branchsync.Service{DB: f.d, Repo: repo, WorkDir: repo.WorkingPath, Paths: f.p, GateDir: f.p.RepoDir(repo.ID)}
	state := svc.InspectCached(context.Background())
	if state.State != branchsync.StateUserOwned || state.Safety != "user_owned" || state.NextAction != nil || state.Pipeline.RunID != id {
		t.Fatalf("failed queued branch was not released: %+v", state)
	}
}

func TestStartedSetupFailureRetainsBranchCustody(t *testing.T) {
	f := newFaultFixture(t, "1")
	repo, err := f.d.GetRepo("fault-repo")
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.d.InsertRun(repo.ID, "started", f.head, f.head)
	if err != nil {
		t.Fatal(err)
	}
	f.mgr.failRunSetup(context.Background(), run.ID, true, "failure after worktree creation")
	stored, err := f.d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != types.RunFailed || stored.TerminalHeadVerifiedAt != nil {
		t.Fatalf("started failure incorrectly released custody: %+v", stored)
	}
}
