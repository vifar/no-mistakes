//go:build e2e && !windows

package e2e

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The acceptance contract requires a failed live comparison read to clear
// readiness without erasing the exact historical qualification or Test gap.
// Removing the disposable remote makes the real Git read fail; restoring it
// must permit recovery only after new protection and mergeability checks.
func TestTargetAdvanceAcceptedGapComparisonOutageLiveJourney(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: acceptedTestGapScenario(t)})
	j.setTrusted("commands:\n  test: 'true'\n  lint: 'true'\n")
	healthy := forgeState{Protection: &forgeEndpoint{Body: []byte(`{"strict":false,"contexts":[],"checks":[]}`)}, Checks: []forgeCheck{greenCheck("build")}}
	j.setForge(healthy)
	j.init()
	const branch = "feature/comparison-outage-gap"
	j.push(branch)
	parked := waitForStepStatus(t, j.h, branch, types.StepTest, types.StepStatusAwaitingApproval, 90*time.Second)
	client, err := ipc.Dial(paths.WithRoot(j.h.NMHome).Socket())
	if err != nil {
		t.Fatal(err)
	}
	testGate, _ := findStep(parked.Steps, types.StepTest)
	var response ipc.RespondResult
	err = client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: parked.ID, Step: types.StepTest, Action: types.ActionApprove, ApprovalReason: "accept the disposable comparison-outage fixture gap", ExpectedGate: testGate.Gate}, &response)
	_ = client.Close()
	if err != nil || !response.OK {
		t.Fatalf("accept fixture gap: %+v %v", response, err)
	}
	run := j.expectReady(branch, 90*time.Second)
	database, err := db.OpenReadOnly(filepath.Join(j.h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	original, err := database.GetRunPRContext(run.ID)
	if err != nil || original == nil || !original.CIQualified {
		t.Fatalf("missing qualified comparison: %+v %v", original, err)
	}
	stepsBefore, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	head := j.h.UpstreamBranchSHA(branch)
	body := livePRBody(t, j.statePath, branch)
	statusWrites := j.ghCalls("/statuses/")
	advanceMain(t, j.h, "advanced.txt", "new target\n", "advance target before comparison outage")
	offline := j.h.UpstreamDir + ".offline"
	if err := os.Rename(j.h.UpstreamDir, offline); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(offline); err == nil {
			_ = os.Rename(offline, j.h.UpstreamDir)
		}
	})
	waitReady := func(want bool) {
		t.Helper()
		j.h.WaitForCIReady(run.ID, want, 75*time.Second)
	}

	assertComparison := func() {
		t.Helper()
		current, err := database.GetRunPRContext(run.ID)
		if err != nil || !reflect.DeepEqual(current, original) {
			t.Fatalf("comparison read failure transferred or erased qualification: %+v -> %+v (%v)", original, current, err)
		}
	}
	waitReady(false)
	assertComparison()
	t.Logf("unavailable disposable remote cleared readiness at unchanged qualification %s:\n%s", head, j.ciLog(run.ID))
	readsBefore := j.protectionReads()
	blocked := healthy
	blocked.Mergeable = "UNKNOWN"
	j.setForge(blocked)
	if err := os.Rename(offline, j.h.UpstreamDir); err != nil {
		t.Fatal(err)
	}
	// Restoring the comparison read alone must not restore readiness from a
	// cached mergeability result. Wait for the real monitor to observe that
	// mergeability is unresolved (the adapter normalizes UNKNOWN to PENDING).
	deadline := pollDeadline(t, 75*time.Second)
	for !strings.Contains(j.ciLog(run.ID), "mergeable state still pending:") && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(j.ciLog(run.ID), "mergeable state still pending:") || j.h.RunInfo(run.ID).CIReady {
		t.Fatalf("comparison recovery ignored current mergeability:\n%s", j.ciLog(run.ID))
	}
	assertComparison()
	t.Logf("restored remote with unresolved live mergeability kept readiness false:\n%s", j.ciLog(run.ID))
	j.setForge(healthy)
	waitReady(true)
	assertComparison()
	stepsAfter, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, step := range stepsBefore {
		if step.StepName == types.StepTest && !reflect.DeepEqual(step, stepsAfter[i]) {
			t.Fatalf("comparison recovery changed the accepted Test gap: %+v -> %+v", step, stepsAfter[i])
		}
	}
	if j.protectionReads() <= readsBefore || j.h.UpstreamBranchSHA(branch) != head || livePRBody(t, j.statePath, branch) != body || j.ghCalls("/statuses/") != statusWrites {
		t.Fatal("comparison recovery skipped live protection or changed the head, attestation, or status")
	}
	out, err := j.h.Run("axi", "status", "--run", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("restored remote and fresh policy check recovered readiness with the accepted gap intact:\n%s", out)
	j.finish(branch)
}
