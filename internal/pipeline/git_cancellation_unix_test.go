//go:build unix

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Explicitly stopping Test must record cancellation, while an independent
// command failure or joined failure must still record failure.
func TestExecutor_CancelDuringGitRead(t *testing.T) {
	for _, mode := range []string{"interrupted", "independent exit", "joined failure"} {
		t.Run(mode, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := completionFixture(t, database, run)
			realGit, err := testgit.RealGit()
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			entered := filepath.Join(bin, "entered")
			trap := ""
			if mode == "independent exit" {
				trap = "trap 'exit 128' TERM\n"
			}
			script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = nm-cancel-test ]; then\n%s: > %q\nwhile :; do sleep 1; done\nfi\nexec %q \"$@\"\n", trap, entered, realGit)
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
				_, err := git.Run(sctx.Ctx, sctx.WorkDir, "nm-cancel-test")
				if mode == "joined failure" {
					err = errors.Join(err, errors.New("independent failure"))
				}
				return nil, err
			}}
			executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			done := make(chan error, 1)
			go func() { done <- executor.Execute(ctx, run, repo, workDir) }()
			deadline := time.After(cancellationTestWait)
			for {
				if _, err := os.Stat(entered); err == nil {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("execution ended before Git started: %v", err)
				case <-deadline:
					t.Fatal("Git did not start")
				case <-time.After(10 * time.Millisecond):
				}
			}
			cancel(errors.New(types.RunCancelReasonAbortedByUser))
			select {
			case err := <-done:
				var exit *exec.ExitError
				if !errors.As(err, &exit) || !errors.Is(err, context.Canceled) {
					t.Fatalf("command/cancellation evidence lost: %v", err)
				}
			case <-time.After(cancellationTestWait):
				t.Fatal("cancelled execution did not stop")
			}
			stored, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := types.RunFailed
			if mode == "interrupted" {
				want = types.RunCancelled
				if stored.Error == nil || *stored.Error != types.RunCancelReasonAbortedByUser {
					t.Fatalf("operator cancellation reason lost: %+v", stored)
				}
			}
			if stored.Status != want {
				t.Fatalf("durable status = %s, want %s", stored.Status, want)
			}
		})
	}
}
