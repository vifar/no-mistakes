package execpath

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A resolver has to read the PATH the way the CHILD will see it: os/exec
// keeps the LAST entry for a repeated key, so a reader that took the first
// would look in a different directory than the child does.
func TestValueReadsTheLastEntry(t *testing.T) {
	env := []string{"PATH=/first", "OTHER=1", "PATH=/second"}
	got, ok := Value(env, "PATH")
	if !ok {
		t.Fatal("PATH not found")
	}
	if got != "/second" {
		t.Fatalf("PATH = %q, want the last entry", got)
	}
}

// The key match is case-insensitive on Windows, where the environment's keys
// are.
func TestValueIsCaseInsensitiveOnWindows(t *testing.T) {
	env := []string{"Path=/mixed"}
	if _, ok := ValueForOS(env, "PATH", "windows"); !ok {
		t.Fatal("PATH not found under a mixed-case key")
	}
	if _, ok := ValueForOS(env, "PATH", "linux"); ok {
		t.Fatal("a mixed-case key matched on a case-sensitive platform")
	}
}

// Windows resolves a name without an extension through PATHEXT, so the lookup
// has to try the same names the platform would.
func TestCandidatesHonoursPathExt(t *testing.T) {
	env := []string{"PATHEXT=.COM;.EXE"}
	got := CandidatesForOS("windows", "git", env)
	if len(got) < 2 || got[0] != "git" || got[1] != "git.COM" {
		t.Fatalf("candidates = %v, want git and git.COM first", got)
	}
	custom := []string{"PATHEXT=.CMD"}
	got = CandidatesForOS("windows", "git", custom)
	if len(got) < 2 || got[1] != "git.CMD" {
		t.Fatalf("candidates = %v, want the environment's PATHEXT honoured", got)
	}
	// A name that already carries an extension is used as it is.
	got = CandidatesForOS("windows", "git.exe", env)
	if len(got) != 1 || got[0] != "git.exe" {
		t.Fatalf("candidates = %v, want the name as it is", got)
	}
}

// An environment that names no PATH leaves the process's PATH in charge. An
// environment that names an EMPTY PATH names no program at all, and the
// child receives that empty PATH, so the process's binary must not be used.
func TestResolveDistinguishesAbsentFromEmptyPath(t *testing.T) {
	if _, hasPath, _ := Resolve([]string{"HOME=/tmp"}, "git", "/tmp"); hasPath {
		t.Fatal("an environment with no PATH reported one")
	}
	_, hasPath, found := Resolve([]string{"PATH="}, "git", "/tmp")
	if !hasPath {
		t.Fatal("an environment with an empty PATH reported none")
	}
	if found {
		t.Fatal("an empty PATH found a program")
	}
}

func TestResolveFindsTheProgram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the executable check differs on Windows")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path, hasPath, found := Resolve([]string{"PATH=" + dir}, "git", dir)
	if !hasPath || !found {
		t.Fatalf("resolve = %q, hasPath=%v, found=%v", path, hasPath, found)
	}
	if path != fake {
		t.Fatalf("resolved %q, want %q", path, fake)
	}
	// A program that is not executable is not a program the platform runs.
	plain := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, found := Resolve([]string{"PATH=" + filepath.Join(dir, "empty")}, "not-executable", dir); found {
		t.Fatal("a file without the executable bit was resolved")
	}
}
