package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPostReceiveOwnerPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, owner     string
		required, nonce bool
	}{
		{name: "direct_ownerless", required: true},
		{name: "tui_ownerless", required: true},
		{name: "nonce_ownerless", required: true, nonce: true},
		{name: "owned", owner: "Firstmate", required: true},
		{name: "nonce_owned", owner: "Firstmate", required: true, nonce: true},
		{name: "policy_off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.ConfigFile(), []byte(fmt.Sprintf("intent:\n  require_owner: %v\n", tc.required)), 0600); err != nil {
				t.Fatal(err)
			}
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			gateDir := p.RepoDir("owner-policy")
			gitCmd(t, "", "init", "-b", "main", gateDir)
			gitCmd(t, gateDir, "config", "user.name", "Test")
			gitCmd(t, gateDir, "config", "user.email", "test@example.com")
			gitCmd(t, gateDir, "commit", "--allow-empty", "-m", "fixture")
			head := gitOutput(t, gateDir, "rev-parse", "HEAD")
			branch := "fresh-" + tc.name
			gitCmd(t, gateDir, "update-ref", "refs/heads/"+branch, head)
			repo, err := database.InsertRepoWithID("owner-policy", gateDir, "https://example.com/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			manager := NewRunManager(database, p, nil)
			h := newSchedHarness(1, false)
			if _, ok := h.tryAdmit("blocker"); !ok {
				t.Fatal("cannot hold scheduler")
			}
			manager.sched = h.slotScheduler
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			params := &ipc.PushReceivedParams{Gate: gateDir, Ref: "refs/heads/" + branch, Old: strings.Repeat("0", 40), New: head, Owner: tc.owner, Intent: "review this submission"}
			if tc.nonce {
				params.LaunchNonce = "post-receive-nonce"
				params.ValidationGeneration = "post-receive-generation"
			}
			id, err := manager.HandlePushReceived(context.Background(), params)
			if err != nil {
				t.Fatalf("post-receive must create a run after refs changed: %v; logs=%s", err, logs.String())
			}
			run, err := database.GetRun(id)
			if err != nil || run == nil {
				t.Fatalf("missing run: %v", err)
			}
			if run.Owner() != tc.owner || run.Status != types.RunQueued || run.Branch != branch {
				t.Fatalf("unexpected run: %+v", run)
			}
			bypass := tc.required && tc.owner == ""
			found := false
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["event"] != "launch_owner_policy_bypassed" {
					continue
				}
				found = true
				if record["msg"] != "require_owner not enforced for post-receive (TUI/direct push): no pre-receive check (follow-up #82)" || record["branch"] != branch || record["caller_path"] != "post-receive (TUI/direct push)" || record["ownerless_admitted"] != true || record["run_id"] != id {
					t.Fatalf("incomplete policy trace: %+v", record)
				}
			}
			if found != bypass {
				t.Fatalf("bypass warning=%v want=%v; logs=%s", found, bypass, logs.String())
			}
			logs.Reset()
			params.New = strings.Repeat("0", 40)
			if _, err := manager.HandlePushReceived(context.Background(), params); err == nil || !strings.Contains(err.Error(), "ref deletion") {
				t.Fatalf("deletion contract: %v", err)
			}
			rows, err := database.GetRunsByRepo(repo.ID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("deletion created run: %v %v", rows, err)
			}
			if strings.Contains(logs.String(), "launch_owner_policy_bypassed") {
				t.Fatal("deletion emitted admission warning")
			}
			if tc.nonce {
				params.New = head
				replayID, err := manager.HandlePushReceived(context.Background(), params)
				if err != nil || replayID != id {
					t.Fatalf("receipt replay: %s %v", replayID, err)
				}
				rows, err = database.GetRunsByRepo(repo.ID)
				if err != nil || len(rows) != 1 || rows[0].Owner() != tc.owner {
					t.Fatalf("replay changed owner/run: %v %v", rows, err)
				}
			}
			if tc.required && tc.owner == "" {
				gitCmd(t, gateDir, "update-ref", "refs/heads/axi-launch", head)
				request := &ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "axi-launch", HeadSHA: head, Intent: "pre-mutation admission", LaunchNonce: "axi-nonce", ValidationGeneration: "axi-generation"}
				if _, err := manager.HandleStartFreshRun(context.Background(), request); err == nil || !strings.Contains(err.Error(), "launch requires an owner") {
					t.Fatalf("AXI ownerless refusal lost: %v", err)
				}
				rows, err = database.GetRunsByRepo(repo.ID)
				if err != nil || len(rows) != 1 {
					t.Fatalf("AXI refusal changed run rows: %v %v", rows, err)
				}
				request.Owner = "Firstmate"
				receipt, err := manager.HandleStartFreshRun(context.Background(), request)
				if err != nil {
					t.Fatalf("immediate owned retry: %v", err)
				}
				owned, err := database.GetRun(receipt.RunID)
				if err != nil || owned == nil || owned.Owner() != "Firstmate" {
					t.Fatalf("owned retry lost owner: %+v %v", owned, err)
				}
			}
		})
	}
}
