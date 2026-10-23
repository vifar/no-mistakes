package cli

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// The CLI tests exercise the real product paths: enrollment, enrollment
// refreshes, run starts, recovery. Any one of those can write Git
// configuration, and a test that resolves the OPERATOR'S home instead of a
// throwaway one writes into the account that is running the suite - the
// operator's real ~/.no-mistakes root, or the shared configuration of a real
// registered checkout.
//
// TestMain therefore points HOME, NM_HOME and GIT_CONFIG_GLOBAL at temporary
// directories. This file pins that arrangement, because a silent loss of it
// is exactly the failure that damaged a registered checkout on 2026-10-22
// (#183, #184): the suite stayed green while it rewrote a real repository's
// configuration.

// operatorHome resolves the home directory of the account running the tests
// from the account database, not from $HOME: TestMain replaces $HOME for the
// whole package, so $HOME cannot be used to find the real one.
func operatorHome(t *testing.T) string {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Skipf("cannot resolve the running account: %v", err)
	}
	if current.HomeDir == "" {
		t.Skip("the running account has no home directory recorded")
	}
	return resolvePath(t, current.HomeDir)
}

// resolvePath makes a path absolute and resolves symlinks along the part of
// it that exists, so /tmp and /private/tmp compare equal on macOS and a
// temporary directory that is a symlink cannot masquerade as the real home.
// A path that does not exist yet is resolved through its parent.
func resolvePath(t *testing.T, path string) string {
	t.Helper()
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("absolute path for %q: %v", path, err)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return absolute
	}
	return filepath.Join(parent, filepath.Base(absolute))
}

// within reports whether path is root itself or lies inside it.
func within(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || (!strings.HasPrefix(relative, ".."+string(filepath.Separator)) && relative != "..")
}

func TestTestEnvironmentNeverResolvesTheOperatorsHome(t *testing.T) {
	real := operatorHome(t)
	realRoot := filepath.Join(real, ".no-mistakes")

	// HOME and NM_HOME are both replaced, and neither may be the operator's.
	for _, name := range []string{"HOME", "NM_HOME"} {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s is unset: a CLI test would resolve the operator's real %s", name, name)
		}
		resolved := resolvePath(t, value)
		if resolved == real || within(resolved, real) {
			t.Fatalf("%s resolves to %q, inside the operator's home %q: a CLI test would write into the account running the suite", name, resolved, real)
		}
	}

	// The package's own resolver must agree: paths.New is what every CLI
	// command uses to find the root, the database and the gate repositories.
	root, err := paths.New()
	if err != nil {
		t.Fatalf("paths.New under test: %v", err)
	}
	resolvedRoot := resolvePath(t, root.Root())
	if resolvedRoot == realRoot || within(resolvedRoot, realRoot) {
		t.Fatalf("paths.New resolved %q, the operator's real root: a test would drive the real daemon and its registered repositories", resolvedRoot)
	}
	if want := resolvePath(t, os.Getenv("NM_HOME")); resolvedRoot != want {
		t.Fatalf("paths.New resolved %q, want the test NM_HOME %q", resolvedRoot, want)
	}

	// Git configuration must be a throwaway file too. A fixture commit that
	// reads the operator's ~/.gitconfig inherits commit.gpgsign, hooks and
	// includes; a fixture write aimed at "global" then lands in the real one.
	global := os.Getenv("GIT_CONFIG_GLOBAL")
	if global == "" {
		t.Fatal("GIT_CONFIG_GLOBAL is unset: a fixture git command would read and write the operator's real global configuration")
	}
	resolvedGlobal := resolvePath(t, global)
	if resolvedGlobal == filepath.Join(real, ".gitconfig") || within(resolvedGlobal, real) {
		t.Fatalf("GIT_CONFIG_GLOBAL resolves to %q, inside the operator's home %q", resolvedGlobal, real)
	}
	if home := resolvePath(t, os.Getenv("HOME")); !within(resolvedGlobal, home) {
		t.Fatalf("GIT_CONFIG_GLOBAL %q is not inside the test HOME %q", resolvedGlobal, home)
	}
}

// TestFixtureGitCommandsReadOnlyTheTemporaryGlobalConfig proves the same
// property the way the suspect tests actually exercise it: by running git, in
// a fixture repository, through the same package path a CLI test uses. The
// sentinel lives only in the temporary global file, so a fixture command that
// resolves it is reading that file and nothing else.
func TestFixtureGitCommandsReadOnlyTheTemporaryGlobalConfig(t *testing.T) {
	real := operatorHome(t)
	dir := t.TempDir()
	cliGit(t, dir, "init", "-b", "main")

	out, err := exec.Command("git", "-C", dir, "config", "--list", "--show-origin").CombinedOutput()
	if err != nil && len(out) == 0 {
		t.Fatalf("git config --list: %v", err)
	}
	text := string(out)
	if strings.Contains(text, filepath.Join(real, ".gitconfig")) {
		t.Fatalf("a fixture git command read the operator's real global configuration %q:\n%s", filepath.Join(real, ".gitconfig"), text)
	}

	// The fixture command must resolve the temporary global file as its
	// global scope. Without this, a test that writes "global" configuration
	// could still be writing into the operator's real ~/.gitconfig.
	sentinel, err := exec.Command("git", "-C", dir, "config", "--get", "no-mistakes-test.isolation-sentinel").Output()
	if err != nil {
		t.Fatalf("a fixture git command does not read the temporary global configuration %q (%v):\n%s", os.Getenv("GIT_CONFIG_GLOBAL"), err, text)
	}
	if got := strings.TrimSpace(string(sentinel)); got != "1" {
		t.Fatalf("global sentinel = %q, want 1 from %q", got, os.Getenv("GIT_CONFIG_GLOBAL"))
	}

	// A "global" write from a fixture lands in the temporary file, not the
	// operator's. This is the write that damaged a registered checkout: it
	// must be confined to the test HOME.
	if out, err := exec.Command("git", "-C", dir, "config", "--global", "--get", "no-mistakes-test.isolation-sentinel").Output(); err != nil || strings.TrimSpace(string(out)) != "1" {
		t.Fatalf("the global scope in force is not the temporary file: %q, %v", out, err)
	}
}
