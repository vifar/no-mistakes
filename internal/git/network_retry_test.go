package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/netretry"
)

// stubGit puts a git that fails with the given stderr for its first failures
// calls on PATH, then reports success. It returns the call counter file.
func stubGit(t *testing.T, failures int, stderr string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell executable on PATH")
	}
	dir := t.TempDir()
	counter := filepath.Join(dir, "calls")
	script := fmt.Sprintf(`#!/bin/sh
n=0
[ -f %q ] && n=$(cat %q)
n=$((n+1))
printf '%%s' "$n" > %q
if [ "$n" -le %d ]; then
  printf '%%s\n' %q >&2
  exit 128
fi
printf 'ok\n'
`, counter, counter, counter, failures, stderr)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return counter
}

func calls(t *testing.T, counter string) int {
	t.Helper()
	raw, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read call counter: %v", err)
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &n); err != nil {
		t.Fatalf("parse call counter %q: %v", raw, err)
	}
	return n
}

func TestTransientNetworkFailureIsRetriedThenSucceeds(t *testing.T) {
	previousBase, previousMax := networkRetryBase, networkRetryMax
	networkRetryBase, networkRetryMax = time.Millisecond, time.Millisecond
	t.Cleanup(func() { networkRetryBase, networkRetryMax = previousBase, previousMax })

	counter := stubGit(t, 2, `fatal: unable to access 'https://github.com/vifar/controller.git/': Operation timed out`)
	if err := FetchRemoteBranch(context.Background(), t.TempDir(), "origin", "dev"); err != nil {
		t.Fatalf("fetch after two timeouts: %v", err)
	}
	if got := calls(t, counter); got != 3 {
		t.Fatalf("git calls = %d, want 3 (two transient failures then success)", got)
	}
}

func TestAuthenticationFailureIsNotRetried(t *testing.T) {
	previousBase, previousMax := networkRetryBase, networkRetryMax
	networkRetryBase, networkRetryMax = time.Millisecond, time.Millisecond
	t.Cleanup(func() { networkRetryBase, networkRetryMax = previousBase, previousMax })

	counter := stubGit(t, 9, `fatal: Authentication failed for 'https://github.com/vifar/controller.git/'`)
	err := FetchRemoteBranch(context.Background(), t.TempDir(), "origin", "dev")
	if err == nil || !strings.Contains(err.Error(), "Authentication failed") {
		t.Fatalf("err = %v, want the authentication failure", err)
	}
	if got := calls(t, counter); got != 1 {
		t.Fatalf("git calls = %d, want 1 (an auth failure is never retried)", got)
	}
}

func TestRejectedPushIsNotRetried(t *testing.T) {
	previousBase, previousMax := networkRetryBase, networkRetryMax
	networkRetryBase, networkRetryMax = time.Millisecond, time.Millisecond
	t.Cleanup(func() { networkRetryBase, networkRetryMax = previousBase, previousMax })

	counter := stubGit(t, 9, "! [rejected]        main -> main (non-fast-forward)")
	err := Push(context.Background(), t.TempDir(), "origin", "refs/heads/main", "", false)
	if err == nil || !strings.Contains(err.Error(), "non-fast-forward") {
		t.Fatalf("err = %v, want the rejected push", err)
	}
	if got := calls(t, counter); got != 1 {
		t.Fatalf("git calls = %d, want 1 (a rejected push is never retried)", got)
	}
}

func TestPushRetryDoesNotForceOverAMovedRemote(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	run(t, t.TempDir(), "git", "init", "--bare", remote)

	work := t.TempDir()
	run(t, work, "git", "init", "-b", "main")
	run(t, work, "git", "config", "user.name", "Test")
	run(t, work, "git", "config", "user.email", "test@example.com")
	run(t, work, "git", "remote", "add", "origin", remote)
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "git", "add", "file.txt")
	run(t, work, "git", "commit", "-m", "one")
	if err := Push(context.Background(), work, "origin", "refs/heads/main", "", false); err != nil {
		t.Fatalf("first push: %v", err)
	}
	landed, err := Run(context.Background(), work, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// The push that produced `landed` lost its response: the remote already
	// carries it, so the retry must report success instead of pushing again.
	ok, err := pushAlreadyLanded(context.Background(), work, "origin", "refs/heads/main", "HEAD", landed, false)
	if err != nil || !ok {
		t.Fatalf("already-landed check = %v, %v; want true", ok, err)
	}

	// Another writer advances the remote. A leased retry must refuse rather
	// than overwrite it.
	other := t.TempDir()
	run(t, other, "git", "clone", "-b", "main", remote, ".")
	run(t, other, "git", "config", "user.name", "Other")
	run(t, other, "git", "config", "user.email", "other@example.com")
	if err := os.WriteFile(filepath.Join(other, "other.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, other, "git", "add", "other.txt")
	run(t, other, "git", "commit", "-m", "two")
	run(t, other, "git", "push", "origin", "HEAD:refs/heads/main")

	if _, err := pushAlreadyLanded(context.Background(), work, "origin", "refs/heads/main", "HEAD", landed, true); err == nil {
		t.Fatal("leased retry accepted a remote that moved; want a refusal, never a force push")
	}
}

func TestNetworkRetryUsesDiagnosticsInsteadOfArguments(t *testing.T) {
	previousBase, previousMax := networkRetryBase, networkRetryMax
	networkRetryBase, networkRetryMax = time.Millisecond, time.Millisecond
	t.Cleanup(func() { networkRetryBase, networkRetryMax = previousBase, previousMax })
	for _, verb := range []string{"fetch", "ls-remote"} {
		t.Run(verb, func(t *testing.T) {
			counter := stubGit(t, 1, "fatal: unable to access 'https://github.com/test/conflict.git/': Operation timed out")
			args := []string{verb, "https://github.com/test/conflict.git", "refs/heads/fix/non-fast-forward-conflict-resolution"}
			_, err := RunNetwork(context.Background(), t.TempDir(), args...)
			if err != nil || calls(t, counter) < 2 {
				t.Fatalf("err=%v, want recovery despite argument text", err)
			}
		})
	}
}

func TestCommandRetryClassifierDoesNotReadTransientTextFromArguments(t *testing.T) {
	err := &CommandError{Command: "fetch origin refs/heads/i/o timeout", Stderr: "fatal: invalid refspec", Err: errors.New("exit status 128")}
	if class := netretry.Describe(err); class != "" {
		t.Fatalf("class=%q, want no retry for an invalid refspec", class)
	}
}
