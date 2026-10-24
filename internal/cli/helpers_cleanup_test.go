package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoveTestDirRemovesReadOnlyTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	pkg := filepath.Join(dir, "go", "pkg", "mod", "example.com", "m@v1.0.0")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "m.go"), []byte("package m\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	// Mirror the module cache: every directory read-only, deepest first.
	for p := pkg; p != filepath.Dir(dir); p = filepath.Dir(p) {
		if err := os.Chmod(p, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = removeTestDir(dir) })

	if err := removeTestDir(dir); err != nil {
		t.Fatalf("removeTestDir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("test dir still exists: %v", err)
	}
}

// makeLeftoverTestDir creates dir as a killed run leaves it: a module cache
// with read-only directories, last modified at modTime.
func makeLeftoverTestDir(t *testing.T, dir string, modTime time.Time) {
	t.Helper()
	pkg := filepath.Join(dir, "go", "pkg", "mod", "example.com", "m@v1.0.0")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "m.go"), []byte("package m\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	for p := pkg; p != dir; p = filepath.Dir(p) {
		if err := os.Chmod(p, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(dir, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeTestDir(dir) })
}

func TestSweepStaleTestDirsRemovesOnlyStaleLeftovers(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	stale := now.Add(-staleTestDirAge - time.Hour)

	staleHome := filepath.Join(base, "nm-cli-home-111")
	staleRoot := filepath.Join(base, "nm-cli-test-222")
	freshHome := filepath.Join(base, "nm-cli-home-333")
	unrelated := filepath.Join(base, "nm-other-444")
	makeLeftoverTestDir(t, staleHome, stale)
	makeLeftoverTestDir(t, staleRoot, stale)
	// A concurrent run may own a young directory: it must survive.
	makeLeftoverTestDir(t, freshHome, now.Add(-time.Minute))
	makeLeftoverTestDir(t, unrelated, stale)

	// A symlink named like a test dir is never followed or removed.
	target := filepath.Join(t.TempDir(), "target")
	makeLeftoverTestDir(t, target, stale)
	link := filepath.Join(base, "nm-cli-home-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	removed := sweepStaleTestDirs(base, now, staleTestDirAge)

	if len(removed) != 2 {
		t.Fatalf("removed %v, want only %s and %s", removed, staleHome, staleRoot)
	}
	for _, dir := range []string{staleHome, staleRoot} {
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("stale leftover %s still exists: %v", dir, err)
		}
	}
	for _, path := range []string{freshHome, unrelated, link, target} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("%s was removed: %v", path, err)
		}
	}
}

func TestChildGoCommandsKeepCachesOutOfTestHome(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	out, err := exec.Command("go", "env", "GOMODCACHE", "GOCACHE").Output()
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	// A cache directory may not exist yet, so compare both the literal and
	// the symlink-resolved forms (/tmp is /private/tmp on macOS).
	home := os.Getenv("HOME")
	resolvedHome := resolvePath(t, home)
	for _, value := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		value = strings.TrimSpace(value)
		if value == "" || value == "off" {
			continue
		}
		if within(value, home) || within(resolvePath(t, value), resolvedHome) {
			t.Fatalf("a child go command caches in %q, inside the temporary HOME %q: a killed run leaks that cache with the HOME", value, home)
		}
	}
}
