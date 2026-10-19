//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A temporary directory with no .git must not inherit the checkout above
// TMPDIR. In particular, init must refuse instead of changing its remote.
func TestHarnessNonRepositoryTempDirStaysOutsideCheckout(t *testing.T) {
	// Shared binaries must outlive this test's nested temporary directory;
	// later journeys reuse them through buildOnce after this test cleans up.
	buildBinaries(t)
	parent := t.TempDir()
	tmp := filepath.Join(parent, "tmp")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	h := NewHarness(t, SetupOpts{Agent: "opencode"})
	mustGit := func(args ...string) string {
		t.Helper()
		out, err := h.runGit(context.Background(), parent, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mustGit("init", "--initial-branch=main")
	mustGit("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "parent checkout")
	mustGit("remote", "add", "origin", h.UpstreamDir)
	wantRemotes := mustGit("remote", "-v")

	for _, check := range []struct {
		command string
		fails   bool
		message string
	}{
		{"status", false, "not in a git repository"},
		{"runs", true, "not in a git repository"},
		{"init", true, "not a git repository"},
	} {
		t.Run(check.command, func(t *testing.T) {
			dir, err := os.MkdirTemp(tmp, "nonrepo-")
			if err != nil {
				t.Fatal(err)
			}
			out, err := h.RunInDir(dir, check.command)
			if (err != nil) != check.fails || !strings.Contains(out, check.message) {
				t.Fatalf("%s outside Git: err=%v output=%s", check.command, err, out)
			}
			t.Logf("no-mistakes %s outside Git: err=%v\n%s", check.command, err, out)
		})
	}
	if got := mustGit("remote", "-v"); got != wantRemotes {
		t.Fatalf("parent remotes changed: got %q, want %q", got, wantRemotes)
	}
}
