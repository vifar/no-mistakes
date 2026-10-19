//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// An uncertified CI monitor keeps its interrupted display state across a forced
// restart, but recovery must reconcile its published status to failure. Only an
// unchanged, certified watcher earns the planned-shutdown preservation exception.
func TestForcedRestartKeepsCIMonitorRunInterrupted(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setForge(forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{runningCheck("build")}})
	j.init()

	const branch = "feature/restart-ci-monitor"
	j.push(branch)
	run := j.expectWaiting(branch, "CI checks running, waiting for results...")
	if run.PRURL == nil || *run.PRURL == "" {
		t.Fatalf("run %s is monitoring CI without a PR", run.ID)
	}
	failuresBefore := j.ghCalls("/statuses/", "state=failure")

	if out, err := j.h.Run("daemon", "restart", "--force"); err != nil {
		t.Fatalf("daemon restart --force: %v\n%s", err, out)
	} else {
		t.Logf("restart output: %s", strings.TrimSpace(out))
	}

	deadline := time.Now().Add(30 * time.Second)
	info := j.h.RunInfo(run.ID)
	for info.Status != types.RunCIMonitorInterrupted && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		info = j.h.RunInfo(run.ID)
	}
	if info.Status != types.RunCIMonitorInterrupted || deref(info.Error) != types.RunCIMonitorInterruptedReason {
		t.Fatalf("run after forced restart = %s (%q), want %s", info.Status, deref(info.Error), types.RunCIMonitorInterrupted)
	}
	ci, _ := findStep(info.Steps, types.StepCI)
	if ci.Status != types.StepStatusSkipped || deref(ci.Error) != types.RunCIMonitorInterruptedReason {
		t.Fatalf("CI step after forced restart = %s (%q), want skipped as interrupted", ci.Status, deref(ci.Error))
	}
	// Status reconciliation runs in the background after IPC readiness.
	deadline = time.Now().Add(30 * time.Second)
	for j.ghCalls("/statuses/", "state=failure") == failuresBefore && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if got := j.ghCalls("/statuses/", "state=failure"); got != failuresBefore+1 {
		t.Fatalf("forced restart posted %d failure statuses to the PR head, want one for the uncertified monitor", got-failuresBefore)
	}
	t.Logf("run %s kept as %s across the forced restart; uncertified status reconciled to failure", run.ID, info.Status)
}
