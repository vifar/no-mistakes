package github_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
)

func TestCancellationOfRunningGitHubCommandsIsAttributedToCancellation(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("requires POSIX termination status")
	}
	asset := filepath.Join(t.TempDir(), "proof.png")
	if err := os.WriteFile(asset, []byte("png bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, invoke := range map[string]func(context.Context, *github.Host) error{
		"auth": func(ctx context.Context, h *github.Host) error { return h.Available(ctx) },
		"stdout": func(ctx context.Context, h *github.Host) error {
			_, err := h.GetPRBaseBranch(ctx, &scm.PR{Number: "7"})
			return err
		},
		"combined": func(ctx context.Context, h *github.Host) error {
			_, err := h.UpdatePR(ctx, &scm.PR{Number: "7"}, scm.PRContent{Body: "b"})
			return err
		},
		"create": func(ctx context.Context, h *github.Host) error {
			_, err := h.CreatePR(ctx, "feature", "main", scm.PRContent{Title: "t", Body: "b"})
			return err
		},
		"status": func(ctx context.Context, h *github.Host) error {
			return h.PostCommitStatus(ctx, "abc123", "pending", "running", "")
		},
		"rerun": func(ctx context.Context, h *github.Host) error {
			return h.RerunCheck(ctx, nil, scm.Check{Name: "build", RunAttempt: 1, State: "CANCELLED", Link: "https://github.com/test/repo/actions/runs/9/job/10"})
		},
		"attachment repository": func(ctx context.Context, h *github.Host) error { _, err := h.UploadUserAsset(ctx, asset); return err },
		"attachment token":      func(ctx context.Context, h *github.Host) error { _, err := h.UploadUserAsset(ctx, asset); return err },
	} {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "running")
			calls := 0
			host := github.New(func(ctx context.Context, cmdName string, args ...string) *exec.Cmd {
				calls++
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGitHubHelperProcess$")
				cmd.Env = append(os.Environ(), "GITHUB_TEST_HELPER=1")
				if name == "attachment repository" && len(args) > 1 && args[0] == "auth" && args[1] == "token" {
					cmd.Env = append(cmd.Env, "GITHUB_TEST_STDOUT=ghp_test")
				} else {
					cmd.Env = append(cmd.Env, "GITHUB_TEST_RUNNING="+marker)
				}
				return cmd
			}, nil, "", "test/repo")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- invoke(ctx, host) }()
			deadline := time.NewTimer(15 * time.Second)
			defer deadline.Stop()
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("command exited before cancellation: %v", err)
				case <-deadline.C:
					t.Fatal("command never announced its start")
				case <-time.After(10 * time.Millisecond):
				}
			}
			cancel()
			select {
			case err := <-done:
				wantCalls := 1
				if name == "attachment repository" {
					wantCalls = 2
				}
				if !errors.Is(err, context.Canceled) || !pipeline.CancellationOnly(err, context.Canceled) || !pipeline.CancellationAttributable(err, context.Canceled) || calls != wantCalls {
					t.Fatalf("err=%v calls=%d, want cancellation attribution without retry", err, calls)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("cancelled command did not return promptly")
			}
		})
	}
}

func TestCancellationRacePreservesIndependentGitHubFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host := github.New(func(_ context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestGitHubHelperProcess$")
		cmd.Env = append(os.Environ(), "GITHUB_TEST_HELPER=1", "GITHUB_TEST_STDERR=HTTP 403: Permission denied", "GITHUB_TEST_EXIT_CODE=1")
		cancel()
		return cmd
	}, nil, "", "test/repo")
	_, err := host.GetPRBaseBranch(ctx, &scm.PR{Number: "7"})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || pipeline.CancellationOnly(err, context.Canceled) || pipeline.CancellationAttributable(err, context.Canceled) {
		t.Fatalf("independent failure was lost: %v", err)
	}
}
