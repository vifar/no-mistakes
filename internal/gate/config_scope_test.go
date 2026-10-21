package gate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Issue #121: enrollment must never change the caller's working-tree identity,
// including when Git's own environment names the already registered checkout.
func TestIssue121ReinitKeepsRegisteredCheckoutWorking(t *testing.T) {
	work := setupTestRepo(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d := openTestDB(t, p)
	if _, _, err := Init(context.Background(), d, p, work); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(work, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(work, ".git"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(work, ".git"))
	if _, created, err := Init(context.Background(), d, p, work); err != nil || created {
		t.Fatalf("re-init: created=%v, error=%v", created, err)
	}
	if got := issue121Git(t, work, "config", "--get", "core.bare"); got != "false" {
		t.Fatalf("working checkout core.bare = %q, want false", got)
	}
	issue121Git(t, work, "status", "--porcelain")
	if got := issue121Git(t, work, "rev-parse", "--show-toplevel"); got != work {
		t.Fatalf("root = %q, want %q", got, work)
	}
	after, err := os.ReadFile(filepath.Join(work, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("re-init rewrote the registered checkout's config")
	}
}

func TestIssue121ManualRepairPreservesRegisteredCheckout(t *testing.T) {
	work := setupTestRepo(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d := openTestDB(t, p)
	if _, _, err := Init(context.Background(), d, p, work); err != nil {
		t.Fatal(err)
	}
	head := issue121Git(t, work, "rev-parse", "HEAD")
	issue121Git(t, work, "config", "core.bare", "true")
	issue121Git(t, work, "config", "core.bare", "false")
	issue121Git(t, work, "status", "--porcelain")
	if got := issue121Git(t, work, "rev-parse", "--show-toplevel"); got != work {
		t.Fatalf("root = %q", got)
	}
	if got := issue121Git(t, work, "rev-parse", "HEAD"); got != head {
		t.Fatal("repair changed HEAD")
	}
}

func issue121Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
