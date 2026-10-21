//go:build !windows

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseProcessStartTimeUsesProvidedLocation(t *testing.T) {
	loc := time.FixedZone("UTC+9", 9*60*60)

	startedAt, err := parseProcessStartTime("Mon Jan 2 15:04:05 2006", loc)
	if err != nil {
		t.Fatalf("parseProcessStartTime returned error: %v", err)
	}

	if startedAt.Location() != loc {
		t.Fatalf("location = %v, want %v", startedAt.Location(), loc)
	}
	if startedAt.UTC() != time.Date(2006, time.January, 2, 6, 4, 5, 0, time.UTC) {
		t.Fatalf("utc time = %v, want %v", startedAt.UTC(), time.Date(2006, time.January, 2, 6, 4, 5, 0, time.UTC))
	}
}

func TestProcessStartTimeCommandForcesCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	t.Setenv("LANG", "fr_FR.UTF-8")

	cmd := processStartTimeCommand(123)

	if got := strings.Join(cmd.Args[1:], " "); got != "-p 123 -o lstart=" {
		t.Fatalf("args = %q, want %q", got, "-p 123 -o lstart=")
	}
	if filepath.Base(cmd.Path) != "ps" {
		t.Fatalf("path = %q, want ps executable", cmd.Path)
	}
	if !containsEnvEntry(cmd.Env, "LC_ALL=C") {
		t.Fatalf("expected LC_ALL=C in env, got %v", cmd.Env)
	}
	if !containsEnvEntry(cmd.Env, "LANG=C") {
		t.Fatalf("expected LANG=C in env, got %v", cmd.Env)
	}
	if countEnvEntries(cmd.Env, "LC_ALL") != 1 {
		t.Fatalf("expected one LC_ALL entry, got %v", cmd.Env)
	}
	if countEnvEntries(cmd.Env, "LANG") != 1 {
		t.Fatalf("expected one LANG entry, got %v", cmd.Env)
	}
}

func TestProcessStateCommandForcesCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	t.Setenv("LANG", "fr_FR.UTF-8")

	cmd := processStateCommand(123)

	if got := strings.Join(cmd.Args[1:], " "); got != "-p 123 -o stat=" {
		t.Fatalf("args = %q, want %q", got, "-p 123 -o stat=")
	}
	if filepath.Base(cmd.Path) != "ps" {
		t.Fatalf("path = %q, want ps executable", cmd.Path)
	}
	if !containsEnvEntry(cmd.Env, "LC_ALL=C") {
		t.Fatalf("expected LC_ALL=C in env, got %v", cmd.Env)
	}
	if !containsEnvEntry(cmd.Env, "LANG=C") {
		t.Fatalf("expected LANG=C in env, got %v", cmd.Env)
	}
	if countEnvEntries(cmd.Env, "LC_ALL") != 1 {
		t.Fatalf("expected one LC_ALL entry, got %v", cmd.Env)
	}
	if countEnvEntries(cmd.Env, "LANG") != 1 {
		t.Fatalf("expected one LANG entry, got %v", cmd.Env)
	}
}

func TestProcessRunningTreatsZombieAsNotRunning(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	defer func() {
		_ = cmd.Wait()
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		running, err := processRunning(cmd.Process.Pid)
		if err != nil {
			t.Fatalf("processRunning returned error: %v", err)
		}
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("expected zombie pid %d to be treated as not running", cmd.Process.Pid)
}

func containsEnvEntry(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func countEnvEntries(env []string, key string) int {
	prefix := key + "="
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			count++
		}
	}
	return count
}

// The fake ps runs only after kill(pid, 0) succeeded, then kills and waits
// for the real child to be reaped before returning ps's documented no-match.
func TestProcessRunningAcceptsExitBetweenProbes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, "/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	defer func() { _ = child.Process.Kill(); <-done }()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\n/bin/kill -KILL %d\nwhile /bin/kill -0 %d 2>/dev/null; do /bin/sleep 0.01; done\nexit 1\n", child.Process.Pid, child.Process.Pid)
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	running, err := processRunning(child.Process.Pid)
	if err != nil || running {
		t.Fatalf("exited child reported running=%v err=%v", running, err)
	}
}

func TestProcessRunningRetainsRealProbeFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\necho 'probe access denied' >&2\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	_, err := processRunning(os.Getpid())
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 || !strings.Contains(err.Error(), "probe access denied") || !strings.Contains(err.Error(), fmt.Sprint(os.Getpid())) {
		t.Fatalf("lost probe cause, stderr or pid: %v", err)
	}
}

func TestProcessRunningDoesNotHideAmbiguousPSFailures(t *testing.T) {
	for _, script := range []string{"exit 1", "exit 0", "echo 'ps unavailable' >&2; exit 1", "echo R; exit 1"} {
		t.Run(script, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			if running, err := processRunning(os.Getpid()); err == nil {
				t.Fatalf("existing pid probe accepted: running=%v", running)
			}
		})
	}
}
