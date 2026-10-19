//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Regression for 30fb0ae14b0e3683db0773aea195a23ae2233da5: a bounded
// journey wait could outlive the remaining package budget and lose its named
// diagnostic to testing's panic. The child deliberately has no daemon.
func TestHarnessWaitReportsConditionBeforePackageTimeout(t *testing.T) {
	if os.Getenv("NM_E2E_WAIT_DEADLINE_CHILD") == "1" {
		h := &Harness{t: t, NMHome: t.TempDir()}
		h.WaitForRun("feature/deliberately-stalled", 90*time.Second)
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHarnessWaitReportsConditionBeforePackageTimeout$", "-test.timeout=3s", "-test.v")
	cmd.Env = append(os.Environ(), "NM_E2E_WAIT_DEADLINE_CHILD=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil || err == nil || !strings.Contains(string(out), "no run found for branch feature/deliberately-stalled within") || strings.Contains(string(out), "panic: test timed out") {
		t.Fatalf("stalled journey must fail with its condition before the package alarm: err=%v context=%v\n%s", err, ctx.Err(), out)
	}
}
