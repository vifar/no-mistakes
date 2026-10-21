package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func issue131Gate(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	bare := filepath.Join(base, "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	seedGate(t, base, bare)
	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatal(err)
	}
	runGitOrFatal(t, bare, "remote", "add", "origin", "https://example.com/owner/repo.git")
	if err := RecordSharedConfigBaseline(ctx, bare); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(base, "run")
	if err := WorktreeAdd(ctx, bare, wt, "main"); err != nil {
		t.Fatal(err)
	}
	return bare, wt
}

func TestIssue131SharedWritesDisabledAndScopedIntentPreserved(t *testing.T) {
	ctx := context.Background()
	bare, wt := issue131Gate(t)
	path := filepath.Join(bare, "config")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := ProtectSharedConfig(ctx, bare)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	for _, args := range [][]string{
		{"config", "core.bare", "true"},
		{"config", "core.hooksPath", ".husky/_"},
		{"remote", "add", "no-mistakes", "/different/gate.git"},
	} {
		cmd := exec.Command("git", append([]string{"-C", wt}, args...)...)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("shared write succeeded: %v: %s", args, out)
		}
	}
	for key, value := range map[string]string{"core.hooksPath": ".husky/_", "extensions.worktreeConfig": "true"} {
		if err := SetWorktreeConfig(ctx, wt, key, value); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(runGitOrFatal(t, wt, "config", "--worktree", "--get", key)); got != value {
			t.Fatalf("%s not in worktree scope", key)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("shared config FILE changed")
	}
	if err := guard.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGitOrFatal(t, wt, "rev-parse", "--is-inside-work-tree")); got != "true" {
		t.Fatal("run stopped being a worktree")
	}
}

func TestIssue131BaselineDetectsFileAndKey(t *testing.T) {
	for _, key := range []string{"core.bare", "core.hookspath", "remote.no-mistakes.url", "remote.origin.url", "extensions.worktreeconfig"} {
		t.Run(key, func(t *testing.T) {
			bare, _ := issue131Gate(t)
			path := filepath.Join(bare, "config")
			value := "true"
			if key == "extensions.worktreeconfig" {
				value = "false"
			}
			runGitOrFatal(t, bare, "config", "--file", path, key, value)
			_, err := ProtectSharedConfig(context.Background(), bare)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(strings.ToLower(err.Error()), key) {
				t.Fatalf("want file and key, got %v", err)
			}
		})
	}
}

func TestIssue131ConcurrentStepGuardsKeepWritesDisabled(t *testing.T) {
	bare, wt := issue131Gate(t)
	a, err := ProtectSharedConfig(context.Background(), bare)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := ProtectSharedConfig(context.Background(), bare)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), wt, "config", "core.bare", "true"); err == nil {
		t.Fatal("first step released the second step's protection")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bare, "config.lock")); !os.IsNotExist(err) {
		t.Fatalf("lock survived final close: %v", err)
	}
}

func TestIssue121BareInitializationRefusesWorkingCheckout(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	runGitOrFatal(t, root, "init", work)
	for _, target := range []string{work, filepath.Join(work, ".git")} {
		if err := InitBare(ctx, target); err == nil {
			t.Fatalf("bare initialization accepted working checkout metadata: %s", target)
		}
	}
	runGitOrFatal(t, work, "status", "--porcelain")
}

func TestIssue131EnrollmentKeepsHooksInPerWorktreeScope(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	bare := filepath.Join(base, "repos", "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if err := RefreshManagedGateHooks(bare); err != nil {
		t.Fatal(err)
	}
	runGitOrFatal(t, bare, "config", "core.hooksPath", ".husky/_")
	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatal(err)
	}
	// Legacy helper behavior remains unchanged; enrollment owns the cleanup.
	if err := MarkGateConfigCurrent(bare); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(bare, "config")
	for _, key := range []string{"core.bare", "core.hookspath"} {
		if _, err := Run(ctx, bare, "config", "--file", path, "--get", key); err == nil {
			t.Fatalf("%s remains in shared FILE %s", key, path)
		}
	}
	ownConfig := filepath.Join(bare, "config.worktree")
	if got, err := Run(ctx, bare, "config", "--file", ownConfig, "--get", "core.bare"); err != nil || got != "true" {
		t.Fatalf("bare flag lost from own worktree config: %q: %v", got, err)
	}
	if !GateConfigCurrent(bare) {
		t.Fatal("enrollment failed to stamp current config")
	}
}

func TestIssue131NeverRemovesAForeignConfigLock(t *testing.T) {
	bare, _ := issue131Gate(t)
	path := filepath.Join(bare, "config.lock")
	if err := os.WriteFile(path, []byte("another writer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if guard, err := ProtectSharedConfig(context.Background(), bare); err == nil {
		guard.Close()
		t.Fatal("acquired another writer's lock")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "another writer" {
		t.Fatalf("foreign lock changed: %q: %v", data, err)
	}
}

func TestIssue121BareInitializationIgnoresInheritedCommonDirectory(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	runGitOrFatal(t, root, "init", work)
	before, err := os.ReadFile(filepath.Join(work, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_COMMON_DIR", filepath.Join(work, ".git"))
	bare := filepath.Join(root, "gate.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(work, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("bare initialization rewrote caller config through GIT_COMMON_DIR")
	}
	if err := ValidateBareRepository(ctx, bare); err != nil {
		t.Fatal(err)
	}
}
