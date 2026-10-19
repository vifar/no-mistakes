//go:build e2e && !windows

package e2e

import (
	"encoding/json"
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
	statusCommand(t, h, h.WorkDir, "daemon", "stop", "--force")
	statusCommand(t, h, h.WorkDir, "daemon", "start")
	events := filepath.Join(h.NMHome, "logs", "ref-reconciliation.jsonl")
	history, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	var sawOrphan, sawInvalid bool
	for _, line := range strings.Split(strings.TrimSpace(string(history)), "\n") {
		var event struct {
			Code string `json:"code"`
			Ref  string `json:"ref"`
			Head string `json:"head"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid startup event: %v\n%s", err, line)
		}
		sawOrphan = sawOrphan || event.Code == "STARTUP_ORPHANED_REF" && event.Ref == "refs/heads/unrecorded" && event.Head == head
		sawInvalid = sawInvalid || event.Code == "STARTUP_RECONCILIATION_FAILED"
	}
	if !sawOrphan || !sawInvalid {
		t.Fatalf("restart lost exact orphan or invalid-evidence diagnostics:\n%s", history)
	}
	if out, err := h.runGit(t.Context(), h.WorkDir, "--git-dir="+gate, "rev-parse", "refs/heads/unrecorded"); err != nil || strings.TrimSpace(string(out)) != head {
		t.Fatalf("diagnostic restart changed the orphan ref: %v\n%s", err, out)
	}
	t.Logf("real startup retained exact-head diagnostics without changing the ref:\n%s", history)
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
	if retained, err := os.ReadFile(events); err != nil || string(retained) != string(history) {
		t.Fatalf("resolved status erased startup history: %v\n%s", err, retained)
	}
	if out, err := h.runGit(t.Context(), h.WorkDir, "--git-dir="+gate, "update-ref", "refs/heads/unrecorded", head); err != nil {
		t.Fatalf("restore orphan that requires a startup event: %v\n%s", err, out)
	}
	statusCommand(t, h, h.WorkDir, "daemon", "stop", "--force")
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
