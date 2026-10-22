package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Exercise the actual init command twice with the package's isolated daemon
// fixture: an already registered checkout must stay a working checkout.
func TestIssue121InitCommandReinitKeepsWorkingCheckout(t *testing.T) {
	work := setupTestRepo(t)
	canonicalWork, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	startTestDaemon(t, p, d)
	for i := 0; i < 2; i++ {
		if i == 1 {
			t.Setenv("GIT_COMMON_DIR", filepath.Join(work, ".git"))
		}
		if output, err := executeCmd("init", "--install-skill=false"); err != nil {
			t.Fatalf("init %d: %v: %s", i+1, err, output)
		}
		bare, err := git.Run(context.Background(), work, "config", "--get", "--default=false", "core.bare")
		if err != nil || bare != "false" {
			t.Fatalf("init %d left core.bare=%q: %v", i+1, bare, err)
		}
		if _, err := git.Run(context.Background(), work, "status", "--porcelain"); err != nil {
			t.Fatal(err)
		}
		top, err := git.Run(context.Background(), work, "rev-parse", "--show-toplevel")
		if err != nil || top != canonicalWork {
			t.Fatalf("init %d lost worktree: %q: %v", i+1, top, err)
		}
	}
}
