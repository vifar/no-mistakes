//go:build e2e && !windows

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Planned shutdown cancels an executing review with its durable shutdown reason.
// Drive the real review subprocess so leaving it active for recovery, as a parked
// gate or uncertified CI monitor would be, cannot pass this check.
func TestForcedRestartCancelsAnInterruptedReview(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: silentAgentScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/restart-mid-review"
	h.CommitChange(branch, "feature.txt", "shutdown boundary\n", "exercise interrupted review")
	h.PushToGate(branch)
	run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusRunning, 90*time.Second)
	if out, err := h.Run("daemon", "restart", "--force"); err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	} else {
		t.Logf("restart output: %s", strings.TrimSpace(out))
	}
	got := h.RunInfo(run.ID)
	if got.Status != types.RunCancelled || deref(got.Error) != "cancelled: planned daemon shutdown" {
		t.Fatalf("interrupted review = %s (%q), want cancelled with the planned shutdown reason", got.Status, deref(got.Error))
	}
	t.Logf("run %s after restart: status=%s error=%q", got.ID, got.Status, deref(got.Error))
}

// Delay the actual OS query, without manufacturing its output or exit status.
// The real daemon exits during that delay and the real ps then exits 1. The
// incident's race must still leave a healthy replacement and no old owner.
func TestForcedRestartWithRealProcessExitDuringPIDInspection(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	previous := daemonPIDForRoot(t, h)
	realPS, err := exec.LookPath("ps")
	if err != nil {
		t.Fatal(err)
	}
	probeDir := t.TempDir()
	inspectionPath := filepath.Join(probeDir, "inspecting")
	resultPath := filepath.Join(probeDir, "result")
	// An idle, registered connection keeps the real server draining until the
	// post-shutdown start-time query begins. Without it, the daemon can exit
	// before processRunning even reaches that query.
	keeper, err := ipc.Dial(paths.WithRoot(h.NMHome).Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer keeper.Close()
	var health ipc.HealthResult
	if err := keeper.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		defer keeper.Close()
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-deadline.C:
				released <- fmt.Errorf("post-shutdown PID inspection never began")
				return
			case <-ticker.C:
				if _, err := os.Stat(inspectionPath); err == nil {
					released <- nil
					return
				}
			}
		}
	}()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = '-p' ] && [ "$2" = '%d' ] && [ "$4" = 'lstart=' ] && [ ! -S %s ]; then
  touch %s
  sleep 2
  %s "$@"
  rc=$?
  echo "real ps for old daemon pid %d exited $rc after delayed inspection" > %s
  exit "$rc"
fi
exec %s "$@"
`, previous, shellQuote(paths.WithRoot(h.NMHome).Socket()), shellQuote(inspectionPath), shellQuote(realPS), previous, shellQuote(resultPath), shellQuote(realPS))
	if err := os.WriteFile(filepath.Join(probeDir, "ps"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := h.RunInDirWithEnv(h.WorkDir, map[string]string{
		"PATH":                        probeDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"NM_TEST_DAEMON_STOP_TIMEOUT": "1ns",
	}, "daemon", "restart", "--force")
	t.Logf("restart output: %s", strings.TrimSpace(out))
	if releaseErr := <-released; releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	result, err := os.ReadFile(resultPath)
	if err != nil || !strings.Contains(string(result), "exited 1") {
		t.Fatalf("did not exercise a real vanished-PID inspection: %q, %v", result, err)
	}
	t.Logf("%s", strings.TrimSpace(string(result)))
	current := daemonPIDForRoot(t, h)
	owners := daemonProcessesForRoot(t, h, "after delayed PID inspection")
	if current == previous || len(owners) != 1 || owners[0] != current {
		t.Fatalf("old pid=%d replacement=%d owners=%v", previous, current, owners)
	}
	status, err := h.Run("daemon", "status")
	if err != nil || !strings.Contains(status, "daemon running") {
		t.Fatalf("replacement status: %v\n%s", err, status)
	}
	t.Logf("old pid=%d replacement=%d sole owner; status: %s", previous, current, strings.TrimSpace(status))
}
