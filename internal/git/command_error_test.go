package git

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/testgit/gitfault"
)

func TestCommandErrorRetainsRedactedCapturedStderrAndFormat(t *testing.T) {
	gitfault.Install(t, "error")
	t.Setenv("NM_GIT_FAULT_STDERR", "fatal: cannot read https://alice:password@example.com/repo.git")
	_, err := Run(context.Background(), t.TempDir(), "rev-parse", "--git-common-dir")
	var captured *CommandError
	if !errors.As(err, &captured) {
		t.Fatalf("missing structured captured stderr: %v", err)
	}
	if got := captured.Stderr; got != "fatal: cannot read https://redacted@example.com/repo.git\n" {
		t.Fatalf("captured stderr = %q", got)
	}
	if got, want := err.Error(), "git rev-parse --git-common-dir: exit status 128: fatal: cannot read https://redacted@example.com/repo.git"; got != want {
		t.Fatalf("error text = %q, want %q", got, want)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 128 {
		t.Fatalf("underlying exit error lost: %v", err)
	}
	if strings.Contains(err.Error(), "password") || strings.Contains(captured.Stderr, "password") {
		t.Fatal("stderr leaked credentials")
	}
}

func TestCommandErrorPreservesExitAndIncompleteDrain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("escaped Unix process fixture")
	}
	gitfault.Install(t, "escaped")
	gitfault.EscapedHolder(t)
	_, err := Run(context.Background(), t.TempDir(), "rev-parse", "--git-common-dir")
	var exit *exec.ExitError
	if !errors.Is(err, exec.ErrWaitDelay) || !errors.As(err, &exit) || exit.ExitCode() != 128 {
		t.Fatalf("lost combined command/drain cause: %v", err)
	}
}

func TestCommandErrorPreservesContextAndIncompleteDrain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("escaped Unix process fixture")
	}
	gitfault.Install(t, "escaped")
	gitfault.EscapedHolder(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := Run(ctx, t.TempDir(), "rev-parse", "--git-common-dir")
	var exit *exec.ExitError
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, exec.ErrWaitDelay) || !errors.As(err, &exit) || exit.ExitCode() != 128 {
		t.Fatalf("lost context, command, or drain cause: %v", err)
	}
}
