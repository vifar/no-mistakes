package daemon

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The operator's never-started release contract must also hold when the real
// daemon dispatches a durable queued launch and its global config has broken.
// An active custody claim or a created worktree would reject that contract.
func TestQueueSetupFailureReleasesCustodyThroughLiveDaemon(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n")
	holder, _ := f.push("holder")
	f.waitStarted(holder)
	id, _ := f.push("queued-setup")
	f.assertQueued(id, 1, 1)
	repo, err := f.d.GetRepo(f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "queued-setup")
	if err := os.WriteFile(f.p.ConfigFile(), []byte("daemon: [invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.step.release(holder)
	f.waitStatus(id, types.RunFailed)
	f.assertNoWorktree(id)
	info := f.info(id)
	if info.Status != types.RunFailed {
		t.Fatalf("get_run did not expose setup failure: %+v", info)
	}
	state := (&branchsync.Service{DB: f.d, Repo: repo, WorkDir: repo.WorkingPath, Paths: f.p, GateDir: f.p.RepoDir(repo.ID)}).InspectCached(context.Background())
	if state.State != branchsync.StateUserOwned || state.Safety != "user_owned" || state.NextAction != nil || state.Pipeline.RunID != id {
		t.Fatalf("never-started setup failure retained custody: %+v", state)
	}
	if usage := f.slots(); usage.InUse != 0 || usage.Queued != 0 {
		t.Fatalf("setup failure retained capacity: %+v", usage)
	}
	wire, _ := json.MarshalIndent(info, "", "  ")
	t.Logf("get_run after invalid-config dispatch:\n%s", wire)
	wire, _ = json.MarshalIndent(state, "", "  ")
	t.Logf("branch_sync after never-started setup failure:\n%s", wire)
}

// Disabling hold must release an already parked slot without answering its
// gate. IPC snapshots and actual FIFO dispatch are independent observations.
func TestQueueDisablingHoldReleasesParkedSlotThroughLiveDaemon(t *testing.T) {
	f := newSchedFixture(t, "daemon:\n  max_concurrent_runs: 1\n  hold_slot_while_parked: true\n")
	parked, _ := f.push("park-policy")
	f.waitParked(parked)
	blocked, _ := f.push("blocked")
	f.assertQueued(blocked, 1, 1)
	f.assertNotStarted(blocked)
	t.Logf("before policy change: get_slots=%+v", f.slots())
	if err := config.SetGlobalDaemonSetting(f.p.ConfigFile(), "hold_slot_while_parked", "false"); err != nil {
		t.Fatal(err)
	}
	f.reloadSlots()
	f.waitStarted(blocked)
	info := f.info(parked)
	if info.AwaitingAgentSince == nil || info.Status != types.RunRunning {
		t.Fatalf("policy change answered the parked gate: %+v", info)
	}
	if usage := f.slots(); usage.HoldSlotWhileParked || usage.InUse != 1 || usage.ParkedReleased != 1 || usage.Queued != 0 {
		t.Fatalf("policy change stranded capacity: %+v", usage)
	}
	t.Logf("after policy change: queued workload executing; gate still unanswered; get_slots=%+v", f.slots())
	f.respondApprove(parked)
	f.step.release(blocked)
	f.waitStatus(parked, types.RunCompleted)
	f.waitStatus(blocked, types.RunCompleted)
}
