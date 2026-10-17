package daemon

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Owner input comes through the real IPC boundary; the request is a map so this
// regression executes against the pre-owner protocol too.
func TestLaunchOwner(t *testing.T) {
	for _, tc := range []struct {
		name, owner       string
		required, refused bool
	}{
		{name: "ownerless"}, {name: "owned", owner: "firstmate-thread"},
		{name: "refuse ownerless", required: true, refused: true},
		{name: "required owned", owner: "firstmate-thread", required: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ""
			if tc.required {
				cfg = "intent:\n  require_owner: true\n"
			}
			p, d := startTestDaemonWithStepsConfig(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepReview}} }, cfg)
			repo, head := setupTestGitRepo(t, p, d, "owner-launch")
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var activeID string
			if tc.refused {
				active, err := d.InsertRun(repo.ID, "main", head, "")
				if err != nil {
					t.Fatal(err)
				}
				activeID = active.ID
				if err := d.UpdateRunStatus(activeID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
			}
			var result ipc.StartFreshRunResult
			err = client.Call(ipc.MethodStartFreshRun, map[string]any{"repo_id": repo.ID, "branch": "main", "head_sha": head, "intent": "record responsibility", "launch_nonce": "owner-nonce", "validation_generation": "owner-gen", "owner": tc.owner}, &result)
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "owner") {
					t.Fatalf("ownerless launch should be refused, got %v, %+v", err, result)
				}
				runs, e := d.GetRunsByRepo(repo.ID)
				if e != nil || len(runs) != 1 || runs[0].ID != activeID || runs[0].Status != types.RunRunning {
					t.Fatalf("refusal created run: %v %v", runs, e)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r := waitForRunTerminalState(t, d, result.Receipt.RunID)
			if r.Owner() != tc.owner {
				t.Fatalf("durable launch owner = %q, want %q", r.Owner(), tc.owner)
			}
			if r.IntentSessionID == nil || *r.IntentSessionID != tc.owner {
				t.Fatalf("persisted owner = %v; want %q", r.IntentSessionID, tc.owner)
			}
			var info map[string]any
			if err := client.Call(ipc.MethodGetRun, map[string]any{"run_id": r.ID}, &info); err != nil {
				t.Fatal(err)
			}
			if info["run"].(map[string]any)["owner"] != tc.owner {
				t.Fatalf("snapshot owner = %v; want %q", info["run"], tc.owner)
			}
		})
	}
}
