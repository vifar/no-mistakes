package scm

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAuthCheckCommand(t *testing.T) {
	tests := []struct {
		provider Provider
		want     []string
	}{
		{ProviderGitHub, []string{"gh", "auth", "status"}},
		{ProviderGitLab, []string{"glab", "auth", "status"}},
		{ProviderBitbucket, []string{"bb", "profile", "which"}},
		{ProviderAzureDevOps, []string{"az", "account", "show"}},
		// Gitea declines the unscoped generic probe; see AuthCheckCommand.
		{ProviderGitea, nil},
		{ProviderOrigin, []string{"origin", "auth", "status"}},
	}

	for _, tt := range tests {
		got := tt.provider.AuthCheckCommand()
		if len(got) != len(tt.want) {
			t.Fatalf("%q AuthCheckCommand len = %d, want %d", tt.provider, len(got), len(tt.want))
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("%q AuthCheckCommand[%d] = %q, want %q", tt.provider, i, got[i], tt.want[i])
			}
		}
	}
}

// TestAuthConfiguredDeclinesUnscopedGiteaProbe pins that the generic probe
// never asks an ambient tea account: with a tea on PATH that would succeed,
// Gitea is still reported unconfigured so only the repository-scoped
// gitea.Host check can authorize it.
func TestAuthConfiguredDeclinesUnscopedGiteaProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "tea"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	if AuthConfigured(t.Context(), ProviderGitea, t.TempDir()) {
		t.Fatal("generic probe authorized Gitea through an unscoped tea account")
	}
}

func TestCLIAvailable(t *testing.T) {
	binDir := t.TempDir()
	for _, name := range []string{"gh", "bb"} {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(""), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Set PATH to JUST the temp dir so a real glab installed on the host does
	// not leak in and make the ProviderGitLab assertion below flaky.
	t.Setenv("PATH", binDir)

	if !CLIAvailable(ProviderGitHub) {
		t.Fatal("expected gh to be available")
	}
	if !CLIAvailable(ProviderBitbucket) {
		t.Fatal("expected bb to be available")
	}
	if CLIAvailable(ProviderGitLab) {
		t.Fatal("did not expect glab to be available")
	}
	if CLIAvailable(ProviderUnknown) {
		t.Fatal("did not expect unknown provider to be available")
	}
}
