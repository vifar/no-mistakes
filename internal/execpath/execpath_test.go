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
	want, err := os.Stat(fake)
	if err != nil {
		t.Fatal(err)
	}
	path, hasPath, found := Resolve([]string{"PATH=" + dir}, "git", dir)
	if !hasPath || !found {
		t.Fatalf("resolve = %q, hasPath=%v, found=%v", path, hasPath, found)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, want) {
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

func TestResolveReturnsAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	name := "git"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	for _, tc := range []struct {
		name, baseDir, path, directory string
	}{
		{"current", ".", ".", "."},
		{"empty", "", ".", "."},
		{"relative", "repo", "bin", filepath.Join("repo", "bin")},
		{"absolute", root, "bin", "bin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.MkdirAll(tc.directory, 0o755); err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(root, tc.directory, name)
			if err := os.WriteFile(want, []byte("fixture"), 0o755); err != nil {
				t.Fatal(err)
			}
			wantInfo, err := os.Stat(want)
			if err != nil {
				t.Fatal(err)
			}
			got, hasPath, found := Resolve([]string{"PATH=" + tc.path}, "git", tc.baseDir)
			if !hasPath || !found || !filepath.IsAbs(got) {
				t.Fatalf("resolve = %q, hasPath=%v, found=%v; want %q", got, hasPath, found, want)
			}
			gotInfo, err := os.Stat(got)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(gotInfo, wantInfo) {
				t.Fatalf("resolved %q, want the file at %q", got, want)
			}
		})
	}
}

func TestResolvePreservesSymlinkParentTraversal(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	for _, dir := range []string{"actual/repo", "actual/bin", "bin", "work"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "actual", "repo"), "link"); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	name := "git"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	for _, dir := range []string{"actual/bin", "bin"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.Stat(filepath.Join("actual", "bin", name))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, baseDir, path string
	}{
		{"symlink workdir", filepath.Join(root, "link"), "../bin"},
		{"relative symlink workdir", "link", "../bin"},
		{"relative path", root, "link/../bin"},
		{"absolute path", root, root + "/link/../bin"},
		{"relative workdir parent", "link/../repo", "../bin"},
		{"unrelated workdir", filepath.Join(root, "work"), root + "/link/../bin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, hasPath, found := Resolve([]string{"PATH=" + tc.path}, "git", tc.baseDir)
			if !hasPath || !found || !filepath.IsAbs(got) {
				t.Fatalf("resolve = %q, hasPath=%v, found=%v", got, hasPath, found)
			}
			info, err := os.Stat(got)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(info, want) {
				t.Fatalf("resolved %q instead of actual/bin/%s", got, name)
			}
		})
	}
}
