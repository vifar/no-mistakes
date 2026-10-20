package pipeline

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// A git command that this context cancelled records the provenance at the
// execution boundary, so the classifier never has to guess it from a signal.
//
// The provenance is the whole point: a child can die of a signal on its own
// and then race a shutdown that happened to observe the context afterwards.
// Only the execution boundary knows whether the cancellation stopped it.
func TestGitCommandRecordsWhetherItsContextCancelledIt(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := gitInit(repo); err != nil {
		t.Fatal(err)
	}

	t.Run("a cancelled command records the cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			// A git alias that sleeps runs long enough for the cancellation
			// to reach the command.
			_, err := git.Run(ctx, repo, "sleeper")
			done <- err
		}()
		// Give the command a moment to start, then cancel it.
		time.Sleep(200 * time.Millisecond)
		cancel()
		err := <-done
		if err == nil {
			t.Fatal("cancelled git command reported success")
		}
		if !git.CancelledByContext(err) {
			t.Fatalf("cancelled git command did not record the cancellation: %v", err)
		}
	})

	t.Run("a command that failed on its own does not record the cancellation", func(t *testing.T) {
		// A command that fails because the ref does not exist is an
		// independent failure, whatever the context is doing.
		_, err := git.Run(context.Background(), repo, "rev-parse", "refs/heads/does-not-exist")
		if err == nil {
			t.Fatal("git command on a missing ref reported success")
		}
		if git.CancelledByContext(err) {
			t.Fatalf("an independent failure recorded the cancellation: %v", err)
		}
	})
}

// gitInit creates a repository with one commit, so a command that runs
// against it has a HEAD to resolve.
func gitInit(dir string) error {
	if err := exec.Command("git", "init", dir).Run(); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"-C", dir, "config", "user.email", "t@t.com"},
		{"-C", dir, "config", "user.name", "T"},
		{"-C", dir, "config", "alias.sleeper", "!sleep 30"},
		{"-C", dir, "commit", "--allow-empty", "-m", "init"},
	} {
		if err := exec.Command("git", args...).Run(); err != nil {
			return err
		}
	}
	return nil
}
