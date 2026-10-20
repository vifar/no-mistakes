//go:build !windows

package steps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/shipping"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestWatcherTargetRefReadCancellation(t *testing.T) {
	for _, mode := range []string{"ambiguous certified signal", "missing proof", "joined failure", "independent Git failure", "independent signal", "independent SIGTERM", "context cancellation before ref launch", "context cancellation before ref launch missing proof"} {
		t.Run(mode, func(t *testing.T) {
			sc, databasePath, publication := strictStatusContext(t)
			comparison, err := sc.DB.CaptureReviewComparison(sc.Run.ID, db.ReviewComparison{SourceRepo: "test/repo", SourceBranch: "feature", TargetBranch: "main", TargetSHA: sc.Run.BaseSHA, HeadSHA: sc.Run.HeadSHA, MergeBaseSHA: sc.Run.BaseSHA, DiffDigest: strings.Repeat("b", 64)})
			if err != nil {
				t.Fatal(err)
			}
			records, err := sc.DB.GetStepsByRun(sc.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := sc.DB.CompleteReviewStep(records[0].ID, sc.Run.ID, sc.Run.HeadSHA, 0, 1, "", comparison.Generation); err != nil {
				t.Fatal(err)
			}
			control, err := sql.Open("sqlite", databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			if _, err := control.Exec(`UPDATE review_comparisons SET approved=1 WHERE run_id=?`, sc.Run.ID); err != nil {
				t.Fatal(err)
			}
			prURL := "https://github.com/test/repo/pull/42"
			if err := sc.DB.UpdateRunPRURL(sc.Run.ID, prURL); err != nil {
				t.Fatal(err)
			}
			sc.Run.PRURL = &prURL
			sc.Run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull, TargetBranch: "main"}
			var mu sync.Mutex
			var states []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var p struct{ State string }
				json.NewDecoder(r.Body).Decode(&p)
				mu.Lock()
				states = append(states, p.State)
				mu.Unlock()
				w.WriteHeader(201)
			}))
			defer server.Close()
			env, _ := fakeGHWithBase(t, prURL, "main")
			env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL, "FAKE_CLI_PR_STATE=OPEN")
			for key, value := range environmentEntries(t, env) {
				t.Setenv(key, value)
			}
			(&PushStep{}).ReportPublishedHeadStatus(sc, "success", "")
			if mode == "missing proof" || mode == "context cancellation before ref launch missing proof" {
				if err := sc.DB.InvalidateStatusValidation(sc.Run.ID); err != nil {
					t.Fatal(err)
				}
			}
			realGit, err := testgit.RealGit()
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			entered := filepath.Join(bin, "entered")
			script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = ls-remote ] && [ \"$2\" = --exit-code ]; then\n: > %q\nwhile :; do sleep 1; done\nfi\nexec %q \"$@\"\n", entered, realGit)
			if mode == "independent Git failure" {
				script = fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = ls-remote ] && [ \"$2\" = --exit-code ]; then\n(trap '' TERM; sleep 30) &\nprintf '%%s' \"$$\" > %q\nexec %q ls-remote --exit-code %q \"$4\"\nfi\nexec %q \"$@\"\n", entered, realGit, t.TempDir(), realGit)
			}
			if mode == "independent signal" || mode == "independent SIGTERM" {
				script = fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = ls-remote ] && [ \"$2\" = --exit-code ]; then\n(trap '' TERM; sleep 30) &\nprintf '%%s' \"$$\" > %q\nwhile :; do sleep 1; done\nfi\nexec %q \"$@\"\n", entered, realGit)
			}
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			ci := (&CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return sc.Run.BaseSHA, true })
			var readErr error
			wrapper := &transitionStep{name: types.StepCI, execute: func(s *pipeline.StepContext) (*pipeline.StepOutcome, error) {
				var out *pipeline.StepOutcome
				var err error
				if strings.HasPrefix(mode, "context cancellation before ref launch") {
					cancel(errors.New(types.RunCancelReasonAbortedByUser))
					_, err = shipping.RemoteHead(s.Ctx, s.WorkDir, s.Repo.UpstreamURL, "main")
				} else {
					out, err = ci.Execute(s)
				}
				readErr = err
				if mode == "joined failure" {
					err = errors.Join(err, errors.New("independent failure https://user:private-token@example.com/repo"))
				}
				return out, err
			}}
			app := paths.WithRoot(t.TempDir())
			if err := app.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			executor := pipeline.NewExecutor(sc.DB, app, sc.Config, sc.Agent, []pipeline.Step{&watcherReporterStep{}, wrapper}, nil)
			executor.SetForgeContext(&forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"})
			done := make(chan error, 1)
			go func() { done <- executor.Execute(ctx, sc.Run, sc.Repo, sc.WorkDir) }()
			if !strings.HasPrefix(mode, "context cancellation before ref launch") {
				deadline := time.After(20 * time.Second)
				for {
					if raw, err := os.ReadFile(entered); err == nil {
						if mode != "independent Git failure" {
							break
						}
						pid, err := strconv.Atoi(string(raw))
						if err == nil && errors.Is(watcherProcessProbe(pid), syscall.ESRCH) {
							break
						}
					}
					select {
					case err := <-done:
						t.Fatalf("watcher exited before target read: %v", err)
					case <-deadline:
						t.Fatal("target Git read did not start")
					case <-time.After(10 * time.Millisecond):
					}
				}
				if mode == "independent signal" || mode == "independent SIGTERM" {
					raw, err := os.ReadFile(entered)
					if err != nil {
						t.Fatal(err)
					}
					pid, err := strconv.Atoi(string(raw))
					if err != nil {
						t.Fatal(err)
					}
					process, err := os.FindProcess(pid)
					if err != nil {
						t.Fatal(err)
					}
					if mode == "independent SIGTERM" {
						if err := process.Signal(syscall.SIGSTOP); err != nil {
							t.Fatal(err)
						}
						if err := process.Signal(syscall.SIGTERM); err != nil {
							t.Fatal(err)
						}
					} else if err := process.Kill(); err != nil {
						t.Fatal(err)
					}
				}
				cancel(errors.New(types.RunCancelReasonAbortedByUser))
				if mode == "independent SIGTERM" {
					raw, _ := os.ReadFile(entered)
					pid, _ := strconv.Atoi(string(raw))
					process, _ := os.FindProcess(pid)
					if err := process.Signal(syscall.SIGCONT); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
						t.Fatal(err)
					}
				}
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("watcher did not cancel")
				}
			case <-time.After(20 * time.Second):
				t.Fatal("watcher did not settle")
			}
			if mode == "independent Git failure" {
				var exit *exec.ExitError
				if !errors.As(readErr, &exit) || exit.ExitCode() != 128 || !errors.Is(readErr, context.Canceled) {
					t.Errorf("independent Git exit lost identity: %v", readErr)
				}
			} else if mode == "independent signal" || mode == "independent SIGTERM" {
				var exit *exec.ExitError
				if !errors.As(readErr, &exit) || !errors.Is(readErr, context.Canceled) {
					t.Errorf("independent signal lost failure identity: %v", readErr)
				}
				if exit != nil {
					status, ok := exit.Sys().(syscall.WaitStatus)
					wanted := syscall.SIGKILL
					if mode == "independent SIGTERM" {
						wanted = syscall.SIGTERM
					}
					if !ok || !status.Signaled() || status.Signal() != wanted {
						t.Errorf("independent leader exit = %v", exit)
					}
				}
			} else if !errors.Is(readErr, context.Canceled) {
				t.Errorf("target read cancellation provenance = %v", readErr)
			}
			if strings.HasPrefix(mode, "context cancellation before ref launch") {
				if _, err := os.Stat(entered); !os.IsNotExist(err) {
					t.Errorf("cancelled context launched a Git process: %v", err)
				}
			}
			if err := sc.DB.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := db.Open(databasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			run, err := reopened.GetRun(sc.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := types.RunFailed
			if strings.HasPrefix(mode, "context cancellation before ref launch") {
				wantStatus = types.RunCancelled
			}
			if run.Status != wantStatus {
				t.Errorf("durable status = %s, want %s", run.Status, wantStatus)
			}
			if run.Error == nil || strings.Contains(*run.Error, "private-token") {
				t.Errorf("terminal reason leaked or missing: %v", run.Error)
			}
			sc.DB, sc.Run = reopened, run
			for _, requested := range []string{"pending", "success", "failure"} {
				(&PushStep{}).ReportPublishedHeadStatus(sc, requested, "")
			}
			executor.ReportRecoveredPublishedHeadStatus(context.Background(), run, sc.Repo)
			stored, err := reopened.GetCommitStatusPublication(context.Background(), run.ID, publication)
			if err != nil {
				t.Fatal(err)
			}
			eligible := mode == "context cancellation before ref launch"
			if (stored.PreserveSuccess != nil && *stored.PreserveSuccess) != eligible {
				t.Errorf("preserved status = %+v", stored)
			}
			mu.Lock()
			defer mu.Unlock()
			want := "[success failure]"
			if eligible {
				want = "[success]"
			}
			if fmt.Sprint(states) != want {
				t.Errorf("HTTP statuses = %v, want %s", states, want)
			}
		})
	}
}

func TestWatcherImmutableRefErrorsRetainIdentity(t *testing.T) {
	for _, check := range []string{"source", "target"} {
		for _, mode := range []string{"ambiguous signal", "independent signal", "cancelled context"} {
			t.Run(check+"/"+mode, func(t *testing.T) {
				sc, _, _ := strictStatusContext(t)
				sc.Run.Shipping = &types.ShippingOptions{Mode: types.ShippingImmutableSource, TargetBranch: "main", TargetSHA: sc.Run.BaseSHA}
				realGit, err := testgit.RealGit()
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				entered := filepath.Join(bin, "entered")
				script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = ls-remote ]; then\n: > %q\nwhile :; do sleep 1; done\nfi\nexec %q \"$@\"\n", entered, realGit)
				if mode == "independent signal" {
					script = fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = ls-remote ]; then\n(trap '' TERM; sleep 30) &\nprintf '%%s' \"$$\" > %q\nwhile :; do sleep 1; done\nfi\nexec %q \"$@\"\n", entered, realGit)
				}
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				if mode == "cancelled context" {
					cancel()
				}
				go func() {
					if check == "source" {
						done <- shipping.CheckImmutableSource(ctx, sc.Run, sc.Repo, sc.WorkDir)
					} else {
						done <- shipping.CheckImmutableTarget(ctx, sc.Run, sc.Repo, sc.WorkDir, shipping.RemoteHead)
					}
				}()
				if mode != "cancelled context" {
					deadline := time.After(10 * time.Second)
					for {
						if _, err := os.Stat(entered); err == nil {
							break
						}
						select {
						case err := <-done:
							t.Fatalf("ref check exited early: %v", err)
						case <-deadline:
							t.Fatal("ref read never started")
						case <-time.After(10 * time.Millisecond):
						}
					}
					if mode == "independent signal" {
						raw, err := os.ReadFile(entered)
						if err != nil {
							t.Fatal(err)
						}
						pid, err := strconv.Atoi(string(raw))
						if err != nil {
							t.Fatal(err)
						}
						process, err := os.FindProcess(pid)
						if err != nil {
							t.Fatal(err)
						}
						if err := process.Kill(); err != nil {
							t.Fatal(err)
						}
					}
					cancel()
				}
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Errorf("%s ref error lost cancellation identity: %v", check, err)
					}
					var exit *exec.ExitError
					if mode != "cancelled context" && !errors.As(err, &exit) {
						t.Errorf("%s ref error lost independent command evidence: %v", check, err)
					}
					if mode != "cancelled context" && err.Error() != "observe shipping ref: remote observation failed" {
						t.Errorf("ref error changed redacted rendering: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("ref check did not cancel")
				}
			})
		}
	}
}

func watcherProcessProbe(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	err = process.Signal(syscall.Signal(0))
	if errors.Is(err, os.ErrProcessDone) {
		return syscall.ESRCH
	}
	return err
}
