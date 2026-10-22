package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A command that is handed an environment uses THAT environment, for the
// binary as well as for the child.
//
// exec.CommandContext resolves a bare name against the PROCESS's PATH, so an
// environment that carries its own PATH used to be honoured for the child's
// environment and ignored for the binary lookup. A step's environment is
// exactly such an environment - it carries the step's scoped credentials and
// the binaries the step injects - so the command ran a different git than the
// step asked for.
func TestRunWithEnvUsesTheEnvironmentItIsGiven(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	marker := filepath.Join(binDir, "marker")
	fake := filepath.Join(binDir, "git")
	script := "#!/bin/sh\nprintf 'the-environment-git'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := RunWithEnv(context.Background(), dir, []string{"PATH=" + binDir}, "version")
	if err != nil {
		t.Fatalf("git with the given environment: %v", err)
	}
	if !strings.Contains(out, "the-environment-git") {
		t.Fatalf("the command ran a git the given environment does not name: %q", out)
	}

	// An environment with no PATH of its own keeps the process's PATH, so
	// every caller that does not choose one is unaffected.
	real, err := Run(context.Background(), dir, "version")
	if err != nil {
		t.Fatalf("git with the process environment: %v", err)
	}
	if strings.Contains(real, "the-environment-git") {
		t.Fatalf("the command used an environment it was not given: %q", real)
	}
}

// An environment whose PATH names no git says so, rather than quietly running
// a different binary than the caller asked for.
func TestRunWithEnvRefusesAnEnvironmentWithoutGit(t *testing.T) {
	dir := t.TempDir()
	empty := t.TempDir()
	_, err := RunWithEnv(context.Background(), dir, []string{"PATH=" + empty}, "version")
	if err == nil {
		t.Fatal("a PATH with no git in it reported success")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Fatalf("the refusal does not name the binary: %v", err)
	}
}
