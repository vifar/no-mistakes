package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func pendingRewindExecutor(t *testing.T) (*Executor, *db.Run, string) {
	t.Helper()
	d, p, r, _ := setupTest(t)
	dir := completionFixture(t, d, r)
	c := db.PRContextCandidate{LocalHeadSHA: r.HeadSHA, TargetBranch: "main", TargetSHA: strings.Repeat("a", 40), MergeBaseSHA: strings.Repeat("a", 40), DiffDigest: strings.Repeat("b", 64)}
	if _, err := d.BindRunPRContext(r.ID, c, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	sr, err := d.InsertStepResult(r.ID, types.StepRebase)
	if err != nil {
		t.Fatal(err)
	}
	c.TargetSHA = strings.Repeat("c", 40)
	if _, err := d.RewindRunPRContext(r.ID, c, db.TargetAdvanceRewindRequest{ExpectedGeneration: 2, Step: types.StepTest, MaxRewinds: 3}); err != nil {
		t.Fatal(err)
	}
	if err = d.StartStep(sr.ID); err != nil {
		t.Fatal(err)
	}
	if err = d.SetStepDuration(sr.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err = d.UpdateStepStatus(sr.ID, types.StepStatusAwaitingApproval); err != nil {
		t.Fatal(err)
	}

	findings := `{"findings":[{"id":"conflict","severity":"warning","file":"feature.txt","description":"conflict","action":"ask-user"}]}`
	if err := d.SetStepFindings(sr.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(r.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunAwaitingAgent(r.ID); err != nil {
		t.Fatal(err)
	}
	r, err = d.GetRun(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(d, p, nil, nil, []Step{newPassStep(types.StepRebase)}, nil)
	e.waiting = true
	e.waitingStep = types.StepRebase
	e.waitingStepResultID = sr.ID
	return e, r, dir
}

func TestTargetRewindAdmissionRefusesApproveSkipBeforeAnyDelivery(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionSkip} {
			t.Run(string(action)+"/"+map[bool]string{false: "live", true: "recovered"}[recovered], func(t *testing.T) {
				e, r, _ := pendingRewindExecutor(t)

				if recovered {
					restored := NewExecutor(e.db, e.paths, nil, nil, e.steps, nil)
					if err := restored.RegisterRecoveredGate(r); err != nil {
						t.Fatal(err)
					}
					e = restored
				}
				slot := &recordingSlot{}
				e.slot = slot
				// Both live and recovered gates enter the same admission function; no
				// network or step execution is needed to refuse these unsafe responses.
				if _, err := e.RespondWithAcceptedGaps(types.StepRebase, action, nil, nil, nil, nil, "", nil); err == nil {
					t.Fatal("pending Rebase was bypassed")
				}
				if len(slot.log()) != 0 {
					t.Fatalf("refusal touched slot: %v", slot.log())
				}
				if !e.waiting || e.waitingStep != types.StepRebase || e.slotWaitStep != "" {
					t.Fatal("refusal unparked or reserved the gate")
				}
				select {
				case <-e.approvalCh:
					t.Fatal("refusal delivered a response")
				default:
				}
				row, err := e.db.GetStepResult(e.waitingStepResultID)
				if err != nil || row.PendingResponse != nil || row.Status != types.StepStatusAwaitingApproval {
					t.Fatalf("refusal persisted response: %+v %v", row, err)
				}
				if pending, err := e.db.GetPendingTargetRewind(r.ID); err != nil || pending == nil {
					t.Fatalf("refusal lost pending state: %+v %v", pending, err)
				}
			})
		}
	}
}

func TestTargetRewindAdmissionAllowsAbortWithoutRemoteAccess(t *testing.T) {
	e, _, _ := pendingRewindExecutor(t)
	if err := e.Respond(types.StepRebase, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-e.approvalCh:
		if response.action != types.ActionAbort {
			t.Fatal("wrong response")
		}
	default:
		t.Fatal("abort was not delivered")
	}
}

func TestTargetRewindPreparationInvalidationSerializesWithCallback(t *testing.T) {
	shared := &RunShared{}
	dir := completionFixtureForPreparation(t)
	marker := filepath.Join(dir, ".git", "no-mistakes-prepared")
	started, release, prepared, invalidated := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan error, 1)
	go func() {
		prepared <- shared.EnsurePrepared(func() error { close(started); <-release; return os.WriteFile(marker, []byte("old"), 0600) })
	}()
	<-started
	go func() { invalidated <- shared.InvalidatePreparation(context.Background(), dir) }()
	select {
	case err := <-invalidated:
		t.Fatalf("invalidation raced preparation: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-prepared; err != nil {
		t.Fatal(err)
	}
	if err := <-invalidated; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("stale marker survived: %v", err)
	}
	calls := 0
	if err := shared.EnsurePrepared(func() error { calls++; return nil }); err != nil || calls != 1 {
		t.Fatalf("memory cache survived: calls=%d %v", calls, err)
	}
}

func completionFixtureForPreparation(t *testing.T) string {
	t.Helper()
	d, _, r, _ := setupTest(t)
	return completionFixture(t, d, r)
}

func TestTargetRewindPreparationRemovalFailureIsClosed(t *testing.T) {
	dir := completionFixtureForPreparation(t)
	marker := filepath.Join(dir, ".git", "no-mistakes-prepared")
	if err := os.MkdirAll(marker, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(marker, "busy"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := (&RunShared{}).InvalidatePreparation(context.Background(), dir); err == nil {
		t.Fatal("ignored marker removal failure")
	}
}

func TestTargetRewindEvidenceGenerationArchivesAndIsolatesArtifacts(t *testing.T) {
	e, r, dir := pendingRewindExecutor(t)
	old := e.runEvidenceDir(r.ID)
	if err := os.MkdirAll(old, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "obsolete.log"), []byte("must not be reused"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.applyTargetRewind(context.Background(), r, dir, 1); err != nil {
		t.Fatal(err)
	}
	current := e.runEvidenceDir(r.ID)
	if current == old || filepath.Base(current) != "g1" {
		t.Fatalf("evidence was not scoped to g1: %s", current)
	}
	if _, err := os.Stat(filepath.Join(current, "obsolete.log")); !os.IsNotExist(err) {
		t.Fatal("stale artifact visible to current consumers")
	}
	if b, err := os.ReadFile(filepath.Join(e.runEvidenceRoot(r.ID), "superseded", "g0", "obsolete.log")); err != nil || string(b) != "must not be reused" {
		t.Fatalf("diagnostic evidence lost: %q %v", b, err)
	}
}

func TestTargetRewindDropsObsoleteRecoveredResponseAndCachedAuthority(t *testing.T) {
	e, r, dir := pendingRewindExecutor(t)
	e.recoveredResponse = &approvalResponse{action: types.ActionApprove}
	e.answeredParkedMS = 42
	e.slotWaitStep = types.StepTest
	head := r.HeadSHA
	stamp := int64(1)
	r.ReviewApprovedHeadSHA = &head
	r.TerminalHeadVerifiedAt = &stamp
	r.AwaitingAgentSince = &stamp
	if err := e.applyTargetRewind(context.Background(), r, dir, 1); err != nil {
		t.Fatal(err)
	}
	if e.recoveredResponse != nil || e.slotWaitStep != "" || e.answeredParkedMS != 0 || r.ReviewApprovedHeadSHA != nil || r.TerminalHeadVerifiedAt != nil || r.AwaitingAgentSince != nil {
		t.Fatal("obsolete gate response or cached authority survived the rewind")
	}
}

func TestTargetRewindRecoveryKeepsAcceptedFixForTheExistingGeneration(t *testing.T) {
	original, r, dir := pendingRewindExecutor(t)
	raw, err := encodePendingResponse(&approvalResponse{action: types.ActionFix, findingIDs: []string{"conflict"}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = original.db.SetStepPendingResponse(original.waitingStepResultID, raw, "already answered; waiting for a run slot"); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(original.db, original.paths, nil, nil, original.steps, nil)
	if err = e.RegisterRecoveredGate(r); err != nil {
		t.Fatal(err)
	}
	e.SetPRContextGuard(func(*StepContext, types.StepName) (PRContextDecision, error) {
		return PRContextDecision{Target: PRTargetSelection{TargetBranch: "main"}}, nil
	})
	if _, err = e.checkPRContext(context.Background(), r, &db.Repo{ID: r.RepoID}, dir, types.StepRebase, 0, false); err != nil {
		t.Fatal(err)
	}
	if e.evidenceGeneration.Load() != 1 || e.recoveredResponse == nil || e.recoveredResponse.action != types.ActionFix {
		t.Fatal("loading a parked generation discarded its legitimate accepted fix")
	}
}
