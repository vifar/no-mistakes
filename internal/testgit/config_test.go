package testgit

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClearConfigEnvPreservesRepositoryConfig(t *testing.T) {
	binary, err := RealGit()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "no-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	command := func(args ...string) (string, error) {
		cmd := exec.Command(binary, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := command("init"); err != nil {
		t.Fatalf("init: %s, %v", out, err)
	}
	if out, err := command("config", "issue80.retained", "repository"); err != nil {
		t.Fatalf("config: %s, %v", out, err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "issue80.count")
	t.Setenv("GIT_CONFIG_VALUE_0", "injected")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'issue80.parameters=injected' 'issue80.retained=parameters'")
	for _, key := range []string{"issue80.count", "issue80.parameters", "issue80.retained"} {
		if got, err := command("config", "--get", key); err != nil || got == "" {
			t.Fatalf("missing injected input %s: %q, %v", key, got, err)
		}
	}
	ClearConfigEnv()
	for _, key := range []string{"issue80.count", "issue80.parameters"} {
		if got, err := command("config", "--get", key); err == nil || got != "" {
			t.Fatalf("leaked config %s: %q, %v", key, got, err)
		}
	}
	if got, err := command("config", "--get", "issue80.retained"); err != nil || got != "repository" {
		t.Fatalf("repository config changed: %q, %v", got, err)
	}
}
