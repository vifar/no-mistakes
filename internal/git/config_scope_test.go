package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
			expectedKey := key
			if strings.HasPrefix(key, "remote.") {
				sum := sha256.Sum256([]byte(key))
				expectedKey = "redacted config key (sha256:" + hex.EncodeToString(sum[:]) + ")"
			}
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(strings.ToLower(err.Error()), expectedKey) {
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

func TestIssue131EnrollmentRefusesNonGateBeforeWritingConfig(t *testing.T) {
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "foreign.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if err := IsolateHooksPath(ctx, bare); err != nil {
		t.Fatal(err)
	}
	runGitOrFatal(t, bare, "config", "--local", "core.hooksPath", ".husky/_")
	path := filepath.Join(bare, "config")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkGateConfigCurrent(bare); err == nil {
		t.Fatal("accepted a repository outside the managed gate layout")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("refused enrollment changed the shared config before validating gate ownership")
	}
	if _, err := os.Stat(filepath.Join(bare, sharedConfigBaselineFile)); !os.IsNotExist(err) {
		t.Fatalf("refused enrollment wrote a baseline: %v", err)
	}
}

// Git subsection names can contain credentials, even though ordinary variable
// names cannot. The persisted baseline and diagnostics are public contracts.
func TestIssue131SensitiveConfigIdentitiesNeverPersistOrPrint(t *testing.T) {
	for _, key := range []string{
		"url.https://fixture-user:fixture-password@example.invalid/.insteadof",
		"http.https://fixture-user:fixture-password@example.invalid/.extraheader",
		"remote.fixture-password.url",
		"credential.https://fixture-user:fixture-password@example.invalid/.username",
	} {
		for _, change := range []string{"value", "identity", "deletion", "addition"} {
			// Do not put credential-bearing keys into subtest names or failure output.
			t.Run(strings.SplitN(key, ".", 2)[0]+"/"+change, func(t *testing.T) {
				ctx := context.Background()
				bare, _ := issue131Gate(t)
				path := filepath.Join(bare, "config")
				if change != "addition" {
					runGitOrFatal(t, bare, "config", "--file", path, key, "fixture-value")
				}
				if err := RecordSharedConfigBaseline(ctx, bare); err != nil {
					t.Fatal("cannot record fixture baseline")
				}
				data, err := os.ReadFile(filepath.Join(bare, sharedConfigBaselineFile))
				if err != nil {
					t.Fatal(err)
				}
				for _, raw := range []string{key, "fixture-user", "fixture-password", "fixture-value"} {
					if strings.Contains(string(data), raw) {
						t.Fatal("baseline disclosed a sensitive config identity or value")
					}
				}
				if change != "addition" {
					sum := sha256.Sum256([]byte(key))
					if !strings.Contains(string(data), "sha256:"+hex.EncodeToString(sum[:])) {
						t.Fatal("baseline did not retain a hashed sensitive key identity")
					}
				}
				switch change {
				case "value", "addition":
					runGitOrFatal(t, bare, "config", "--file", path, key, "fixture-new-value")
				case "identity":
					runGitOrFatal(t, bare, "config", "--file", path, "--unset", key)
					runGitOrFatal(t, bare, "config", "--file", path, strings.ReplaceAll(key, "fixture-password", "fixture-other-password"), "fixture-value")
				case "deletion":
					runGitOrFatal(t, bare, "config", "--file", path, "--unset", key)
				}
				guard, err := ProtectSharedConfig(ctx, bare)
				if err == nil {
					guard.Close()
					t.Fatal("sensitive config change escaped preflight")
				}
				diagnostic := err.Error()
				if !strings.Contains(diagnostic, path) || !strings.Contains(diagnostic, "redacted config key") {
					t.Fatal("diagnostic did not name the file and redacted key identity")
				}
				for _, raw := range []string{key, "fixture-user", "fixture-password", "fixture-other-password", "fixture-value", "fixture-new-value"} {
					if strings.Contains(diagnostic, raw) {
						t.Fatal("diagnostic disclosed sensitive config data")
					}
				}
			})
		}
	}
}

func TestIssue131LegacySensitiveBaselineDiagnosticIsRedacted(t *testing.T) {
	bare, _ := issue131Gate(t)
	// Simulate an older baseline whose key names were persisted verbatim.
	path := filepath.Join(bare, sharedConfigBaselineFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key := "url.https://fixture-user:fixture-password@example.invalid/.insteadof"
	data = append([]byte(`{"`+key+`":"legacy-value-hash",`), data[1:]...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	guard, err := ProtectSharedConfig(context.Background(), bare)
	if err == nil {
		guard.Close()
		t.Fatal("legacy baseline mismatch escaped detection")
	}
	if strings.Contains(err.Error(), "fixture-user") || strings.Contains(err.Error(), "fixture-password") {
		t.Fatal("legacy baseline diagnostic disclosed a sensitive config key")
	}
	if !strings.Contains(err.Error(), "redacted config key") {
		t.Fatal("legacy diagnostic lost the redacted identity")
	}
}
