package daemon

import (
	"context"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"path/filepath"
	"strings"
	"testing"
)

func TestShippingLaunchPinsDraftAndRejectsNonceConflict(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepIntent}} })
	repo, head := setupTestGitRepo(t, p, d, "shipping-launch")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	draft := true
	params := &ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "main", HeadSHA: head, Intent: "strict goal", LaunchNonce: "shipping-nonce", ValidationGeneration: "generation", Shipping: &types.ShippingOptions{Mode: types.ShippingFull, Draft: &draft}}
	var result ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartFreshRun, params, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.Receipt.RunID)
	if run.Shipping == nil || run.Shipping.Mode != types.ShippingFull || run.Shipping.Draft == nil || !*run.Shipping.Draft {
		t.Fatalf("shipping pin lost: %+v", run.Shipping)
	}
	params.Shipping = &types.ShippingOptions{Mode: types.ShippingImmutableSource}
	if err := client.Call(ipc.MethodStartFreshRun, params, &result); err == nil {
		t.Fatal("nonce changed shipping mode")
	}
	var claim ipc.ClaimLaunchReceiptResult
	if err := client.Call(ipc.MethodClaimLaunchReceipt, &ipc.ClaimLaunchReceiptParams{RepoID: repo.ID, Branch: "main", LaunchNonce: "shipping-nonce", SubmittedHeadSHA: head, ValidationGeneration: "generation", IntentDigest: digestIntent(params.Intent), Shipping: params.Shipping}, &claim); err == nil {
		t.Fatal("conflicting claim accepted")
	}
	var status ipc.GetRunResult
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: run.ID}, &status); err != nil {
		t.Fatal(err)
	}
	if status.Run.Shipping == nil || status.Run.Shipping.Mode != types.ShippingFull {
		t.Fatal("native IPC lost shipping mode")
	}
}

func TestShippingApprovalRejectsUnsupportedCommandException(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return nil })
	repo, head := setupTestGitRepo(t, p, d, "shipping-approval")
	run, err := d.InsertRunWithShipping(repo.ID, "feature", head, head, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pipeline.RecordRunFailure(context.Background(), d, run.ID, "test fixture finished", nil); err != nil {
			t.Errorf("settle owned shipping fixture: %v", err)
		}
	})
	sr, err := d.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"findings":[]}`
	if err := d.StartStep(sr.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(sr.ID, 1, "initial", &raw, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 1, 1, &raw); err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(d, p, nil)
	executor := pipeline.NewExecutor(d, p, nil, nil, []pipeline.Step{&mockPassStep{name: types.StepTest}}, nil)
	parked, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.RegisterRecoveredGate(parked); err != nil {
		t.Fatal(err)
	}
	m.runs[run.ID] = &managedRun{run: parked, executor: executor}
	if _, err := m.HandleRespondWithOverrides(run.ID, types.StepTest, types.ActionApprove, nil, nil, nil, nil, "", managerGateSnapshot(t, m, run.ID, types.StepTest)); err == nil || !strings.Contains(err.Error(), "command overrides") {
		t.Fatalf("command exception = %v", err)
	}
}

func TestShippingStatusBeforeReviewUsesRecordedEnrollment(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo("/unavailable-project", "git@github.com:owner/project.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	run, err := d.InsertRunWithShipping(repo.ID, "refs/heads/feature", head, head, nil, "", "", "", "dev", false, nil, &types.ShippingOptions{Mode: types.ShippingFull, TargetBranch: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	info := runToInfo(d, run, nil)
	if info.SourceRepo != "owner/project" || info.SourceBranch != "feature" || info.TargetBranch != "dev" || info.ReviewGeneration != 0 || info.TargetSHA != "" || info.CIReady {
		t.Fatalf("pre-review status invented proof: %+v", info)
	}
}
