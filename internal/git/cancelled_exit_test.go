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

// A stop that is REFUSED still interrupts the command, and the interruption
// is still the cancellation's doing. The standard library reports the
// refusal instead of the child's exit status, so there is no exit status to
// carry the provenance - the marker carries the refusal itself.
//
// This is the case a planned shutdown hits when the process group is already
// gone: the run was cancelled, but the failure it recorded was the refusal
// rather than the cancellation, so the run read as failed instead of
// cancelled.
//
// The oracle is the STANDARD LIBRARY's own report, obtained from a real
// exec.CommandContext whose Cancel hook refuses. The test never builds the
// message the recogniser looks for, so a recogniser that looked for the
// wrong text would fail here.
func TestCancelledExitRecordsARefusedStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The child finishes on its own, so the command's own outcome is a
	// success. The stop is refused, so the standard library reports the
	// refusal INSTEAD of the success - and that report is the shape this
	// failure takes: the command was interrupted, and the interruption
	// could not reach the child.
	cmd := exec.CommandContext(ctx, "true")
	cmd.Cancel = func() error { return errors.New("operation not permitted") }
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	err := cmd.Wait()
	if err == nil {
		t.Fatal("the refused stop reported success")
	}
	// The standard library's own report is the oracle: the test never
	// builds the text the recogniser looks for, so a recogniser that
	// looked for the wrong text would fail here.
	if !isCancelRefusal(err) {
		t.Fatalf("the standard library's refusal was not recognised: %v", err)
	}
	// The refusal is the standard library's, not the test's: its text
	// carries the refusal the hook returned.
	if !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("the refusal does not carry the hook's own error: %v", err)
	}
	// An error that is not the refusal is the command's own.
	if isCancelRefusal(errors.New("operation not permitted")) {
		t.Fatal("an unrelated error was read as a refusal")
	}
	if isCancelRefusal(nil) {
		t.Fatal("no error was read as a refusal")
	}
}

// The marker is attached on the refused-stop path, so a caller can tell a
// refused stop from a command that failed on its own. The marker is built by
// the PRODUCTION path: the git package's own command, whose stop was
// refused.
//
// The refusal reaches the caller only when it IS the error. When the child
// is still running, the standard library reports the refusal and the
// child is killed by the shell package's group termination, so the child
// produced an exit status as well - and the exit status is what the
// standard library prefers. The marker then carries the exit, and the
// refusal is what the stop's own hook returned. Either way the failure is
// the cancellation's, which is what the marker says.
func TestCancelledExitMarkerCarriesTheRefusedStop(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := exec.Command("git", "init", repo).Run(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", repo, "config", "user.email", "t@t.com"},
		{"-C", repo, "config", "user.name", "T"},
		{"-C", repo, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatal(err)
		}
	}
	// The refusal is injected by the package's own stop path, so the
	// marker is the production one.
	refuseStops.Store(true)
	defer refuseStops.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := filepath.Join(dir, "started")
	for _, args := range [][]string{
		{"-C", repo, "config", "alias.sleeper", "!echo $$ > '" + strings.ReplaceAll(started, "'", "'\\''") + "'; exec sleep 30"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, repo, "sleeper")
		done <- err
	}()
	// The cancel lands while the child is running, so the stop is the
	// context's doing.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	err := <-done
	if err == nil {
		t.Fatal("the refused stop reported success")
	}
	var cancelled *CancelledExitError
	if !errors.As(err, &cancelled) {
		t.Fatalf("a refused stop was not marked: %v", err)
	}
	if cancelled.Error() == "" {
		t.Fatal("the marker reported no reason")
	}
	// The marker says the failure is the cancellation's, and the run
	// reads it as the cancellation rather than as an independent failure.
	if cancelled.ExitError == nil && cancelled.Refused == nil {
		t.Fatal("the marker carried neither the exit nor the refusal")
	}
}
