package github

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

func TestWindowsGitHubCancellationKeepsAnAlreadyCompletedFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	factory, _ := sequencedGHFactory(t, []ghScriptStep{{stderr: "independent failure", code: 1}})
	cmd := factory(ctx, "gh")
	err := runGHCommand(ctx, cmd, func() error {
		if err := shellenv.StartShellCommand(cmd); err != nil {
			return err
		}
		defer shellenv.TerminateShellCommandGroup(cmd)
		err := cmd.Wait()
		cancel()
		if stopErr := cmd.Cancel(); stopErr != nil {
			t.Fatalf("stop completed job: %v", stopErr)
		}
		return err
	})
	var exit *exec.ExitError
	var cancellation *ghCancellationError
	if !errors.As(err, &exit) || errors.As(err, &cancellation) || exit.ExitCode() != 1 {
		t.Fatalf("completed failure changed to cancellation: %v", err)
	}
}
