//go:build e2e && !windows

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Startup diagnostics must expose bad receive evidence without admitting a
// run or changing its ref. Status must re-read current evidence, and a failure
// to retain the event log must refuse daemon startup. Only real CLI/daemon/Git
// and filesystem operations are involved in these diagnostic boundaries.
func TestStartupRefDiagnosticsOnRealCLI(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statusCommand(t, h, h.WorkDir, "init")
	gate := paths.WithRoot(h.NMHome).RepoDir(h.repoID())
	headBytes, err := h.runGit(t.Context(), h.WorkDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(headBytes))
	if out, err := h.runGit(t.Context(), h.WorkDir, "--git-dir="+gate, "fetch", h.WorkDir, "HEAD"); err != nil {
		t.Fatalf("copy disposable head to gate: %v\n%s", err, out)
	}
	if out, err := h.runGit(t.Context(), h.WorkDir, "--git-dir="+gate, "update-ref", "refs/heads/unrecorded", head); err != nil {
		t.Fatalf("create disposable orphan: %v\n%s", err, out)
	}
	notifications := filepath.Join(h.NMHome, "push-notifications")
	if err := os.MkdirAll(notifications, 0700); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{"{broken", `{"version":999}`} {
		if err := os.WriteFile(filepath.Join(notifications, "invalid.json"), []byte(payload), 0600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"status"}, {"axi", "status"}} {
			out := statusCommand(t, h, h.WorkDir, args...)
			for _, want := range []string{"STARTUP_RECONCILIATION_FAILED", "STARTUP_ORPHANED_REF", "refs/heads/unrecorded", head} {
				if !strings.Contains(out, want) {
					t.Fatalf("%v hid bad notification or exact orphan %q:\n%s", args, want, out)
				}
			}
			t.Logf("%v with notification %q:\n%s", args, payload, out)
		}
	}
	if runs := h.Runs(); len(runs) != 0 {
		t.Fatalf("diagnostics invented run admission: %+v", runs)
	}
	if err := os.RemoveAll(notifications); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runGit(t.Context(), h.WorkDir, "--git-dir="+gate, "update-ref", "-d", "refs/heads/unrecorded", head); err != nil {
		t.Fatalf("remove disposable orphan: %v\n%s", err, out)
	}
	out := statusCommand(t, h, h.WorkDir, "axi", "status")
	if strings.Contains(out, "refs/heads/unrecorded") || strings.Contains(out, "STARTUP_RECONCILIATION_FAILED") {
		t.Fatalf("status retained resolved diagnostics:\n%s", out)
	}
	t.Logf("status re-read current evidence without restart:\n%s", out)
	if out, err := h.runGit(t.Context(), h.WorkDir, "--git-dir="+gate, "update-ref", "refs/heads/unrecorded", head); err != nil {
		t.Fatalf("restore orphan that requires a startup event: %v\n%s", err, out)
	}
	statusCommand(t, h, h.WorkDir, "daemon", "stop", "--force")
	events := filepath.Join(h.NMHome, "logs", "ref-reconciliation.jsonl")
	if err := os.Remove(events); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(events, 0700); err != nil {
		t.Fatal(err)
	}
	out, err = h.Run("daemon", "start")
	if err == nil {
		t.Fatalf("daemon started without writable diagnostic event log:\n%s", out)
	}
	log, readErr := os.ReadFile(filepath.Join(h.NMHome, "logs", "daemon.log"))
	if readErr != nil || !strings.Contains(string(log), "STARTUP_RECONCILIATION_FAILED") {
		t.Fatalf("startup refusal lost named diagnostic: %v\n%s\n%s", readErr, out, log)
	}
	for _, line := range strings.Split(string(log), "\n") {
		if strings.Contains(line, "STARTUP_RECONCILIATION_FAILED") {
			t.Logf("real daemon refused startup with an unwritable event log:\n%s\n%s", out, line)
		}
	}
}
