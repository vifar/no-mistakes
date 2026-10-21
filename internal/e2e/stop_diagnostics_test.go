//go:build e2e && !windows

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The child fails through statusCommand, then runs the real fixture cleanup.
// The parent checks the persisted artifacts after the child's cleanup finished.
func TestFailedStatusCommandRetainsFixtureDiagnostics(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "fixture-root")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFailedStatusCommandDiagnosticsChild$", "-test.v")
	cmd.Env = append(os.Environ(), "NM_STOP_DIAGNOSTICS_CHILD="+marker)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected fixture failure: %s", out)
	}
	rootBytes, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Fatalf("child did not create fixture: %v\n%s", readErr, out)
	}
	root := string(rootBytes)
	defer os.RemoveAll(root)
	for path, want := range map[string]string{
		"h/logs/daemon.log":                "daemon fixture trace",
		"h/logs/cli.log":                   "stop caller trace",
		"h/diagnostics/daemon.pid":         "424244",
		"h/diagnostics/command-output.log": "probe access denied",
	} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || !strings.Contains(string(data), want) {
			t.Errorf("diagnostic %s lost after fixture cleanup: %q %v\n%s", path, data, err, out)
		}
	}
}

func TestFailedStatusCommandDiagnosticsChild(t *testing.T) {
	marker := os.Getenv("NM_STOP_DIAGNOSTICS_CHILD")
	if marker == "" {
		t.Skip("intentional failure subprocess only")
	}
	h := NewHarness(t, SetupOpts{})
	root := filepath.Dir(h.NMHome)
	if err := os.WriteFile(marker, []byte(root), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.NMHome, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{"logs/daemon.log": "daemon fixture trace", "logs/cli.log": "stop caller trace", "daemon.pid": "424244"} {
		if err := os.WriteFile(filepath.Join(h.NMHome, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(root, "fail-stop")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nrm -f "+shellQuote(filepath.Join(h.NMHome, "daemon.pid"))+"\necho 'probe access denied' >&2\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	h.NMBin = script
	statusCommand(t, h, h.WorkDir, "daemon", "stop", "--force")
}
