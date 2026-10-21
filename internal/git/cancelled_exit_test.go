package git

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
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
	for _, args := range [][]string{
		{"-C", repo, "config", "user.email", "t@t.com"},
		{"-C", repo, "config", "user.name", "T"},
		{"-C", repo, "config", "alias.sleeper", "!sleep 30"},
		{"-C", repo, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a command this context stopped is marked", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := Run(ctx, repo, "sleeper")
			done <- err
		}()
		// Give the command a moment to start, then cancel it.
		time.Sleep(200 * time.Millisecond)
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
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := Run(ctx, repo, "sleeper")
			done <- err
		}()
		time.Sleep(200 * time.Millisecond)
		// Kill the process group from outside the command's context, then
		// cancel the context. The context never stopped this child, so the
		// exit is not the cancellation's doing.
		_ = exec.Command("pkill", "-TERM", "-f", "sleep 30").Run()
		err := <-done
		if err == nil {
			t.Fatal("killed command reported success")
		}
		var cancelled *CancelledExitError
		if errors.As(err, &cancelled) {
			t.Fatalf("an externally killed command was marked as a cancellation: %v", err)
		}
	})
}
