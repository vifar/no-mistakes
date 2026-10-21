package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The provenance of a cancelled command is recorded where it is known: the
// context stops its own child, and only that stop is recorded. Observing the
// context afterwards would not prove it - a child can die of a signal on its
// own and then race a shutdown that happened to observe the context a moment
// later.
func TestCancelledExitRecordsOnlyTheContextsOwnStop(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := exec.Command("git", "init", repo).Run(); err != nil {
		t.Fatal(err)
	}
	// The sleeper announces that it is up, and writes its own PID, so the
	// test can cancel the context only once the child is running and can
	// signal that child alone. A fixed sleep would race the child's start on
	// a loaded runner, and a cancellation that arrives before the child is
	// up is a different case: the command never ran. Signalling by command
	// text would reach every unrelated process that happens to match it.
	started := filepath.Join(dir, "started")
	for _, args := range [][]string{
		{"-C", repo, "config", "user.email", "t@t.com"},
		{"-C", repo, "config", "user.name", "T"},
		{"-C", repo, "config", "alias.sleeper", "!echo $$ > '" + strings.ReplaceAll(started, "'", "'\\''") + "'; exec sleep 30"},
		{"-C", repo, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatal(err)
		}
	}
	waitForStart := func(t *testing.T) int {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			if data, err := os.ReadFile(started); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
					return pid
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("the child never started")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	resetStart := func(t *testing.T) {
		t.Helper()
		if err := os.Remove(started); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	t.Run("a command this context stopped is marked", func(t *testing.T) {
		resetStart(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := Run(ctx, repo, "sleeper")
			done <- err
		}()
		waitForStart(t)
		cancel()
		err := <-done
		if err == nil {
			t.Fatal("cancelled command reported success")
		}
		var cancelled *CancelledExitError
		if !errors.As(err, &cancelled) {
			t.Fatalf("a command this context stopped was not marked: %v", err)
		}
	})

	t.Run("a command that failed on its own is not marked", func(t *testing.T) {
		_, err := Run(context.Background(), repo, "rev-parse", "refs/heads/does-not-exist")
		if err == nil {
			t.Fatal("command on a missing ref reported success")
		}
		var cancelled *CancelledExitError
		if errors.As(err, &cancelled) {
			t.Fatalf("an independent failure was marked as a cancellation: %v", err)
		}
	})

	t.Run("a command killed by something else is not marked", func(t *testing.T) {
		resetStart(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := Run(ctx, repo, "sleeper")
			done <- err
		}()
		pid := waitForStart(t)
		child, err := os.FindProcess(pid)
		if err != nil {
			t.Fatal(err)
		}
		if err := child.Kill(); err != nil {
			t.Fatal(err)
		}
		err = <-done
		if err == nil {
			t.Fatal("killed command reported success")
		}
		var cancelled *CancelledExitError
		if errors.As(err, &cancelled) {
			t.Fatalf("an externally killed command was marked as a cancellation: %v", err)
		}
	})
}
