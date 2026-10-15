package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The CI monitor must release the actual daemon slot, not merely call Yield
// on a recorder: with cap one, two real monitors can poll their pending PRs.
// The forge executable is an external service fixture; scheduler, executor,
// IPC, database, git setup, and CIStep are the production implementations.
func TestQueueRealCIMonitorsReleaseCapacity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("forge fixture uses a POSIX shell")
	}
	root, err := os.MkdirTemp("", "qci")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, head := setupTestGitRepo(t, p, database, "live-ci")
	forge := filepath.Join(root, "forge")
	forgeCalls := filepath.Join(root, "forge-calls")
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> '%s'
case "$1 $2" in
  "status "*) echo '{"protocol_version":1,"capabilities":{},"max_pr_body_chars":0}' ;;
  "pr find")
    for arg in "$@"; do case "$arg" in --head=ci-*) n=${arg#--head=ci-} ;; esac; done
    printf '{"pr":{"number":%%s,"url":"https://queue.invalid/test/repo/pulls/%%s","head_branch":"ci-%%s","base_branch":"main","head_sha":"%s"}}\n' "$n" "$n" "$n" ;;
  "pr view") printf '{"pr":{"number":%%s,"url":"https://queue.invalid/test/repo/pulls/%%s","head_branch":"ci-%%s","base_branch":"main","head_sha":"%s"},"state":"open","title":"Pending fixture","body":""}\n' "$3" "$3" "$3" ;;
  "pr checks") echo '{"checks":[{"name":"pending-workload","bucket":"pending","state":"PENDING","head_sha":"%s"}]}' ;;
  *) exit 1 ;;
esac
`, forgeCalls, head, head, head)
	if err := os.WriteFile(forge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ag := writeMockClaude(t, t.TempDir())
	cfg := fmt.Sprintf("agent: claude\nagent_path_override:\n  claude: %s\ndaemon:\n  max_concurrent_runs: 1\nprovider_plugins:\n  fixture:\n    command: %s\n    hosts: [queue.invalid]\n", ag, forge)
	if err := os.WriteFile(p.ConfigFile(), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	// All Git traffic stays local; only forge API identity uses this host.
	gitCmd(t, p.RepoDir(repo.ID), "config", "url."+p.RepoDir(repo.ID)+".insteadOf", "https://queue.invalid/test/repo.git")
	if _, err := database.ReplaceRepoURLs(repo.ID, "https://queue.invalid/test/repo.git", ""); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 1; i <= 2; i++ {
		run, err := database.InsertRun(repo.ID, fmt.Sprintf("ci-%d", i), head, head)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateRunPRURL(run.ID, fmt.Sprintf("https://queue.invalid/test/repo/pulls/%d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := database.MarkRunQueued(run.ID, `{"trigger":"push"}`); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
	}
	runTestDaemon(t, p, database, func() []pipeline.Step { return []pipeline.Step{&steps.CIStep{}} }, 20*time.Second)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	deadline := time.Now().Add(45 * time.Second)
	for {
		ready := true
		var snapshots []*ipc.RunInfo
		for _, id := range ids {
			var result ipc.GetRunResult
			if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: id}, &result); err != nil {
				t.Fatal(err)
			}
			snapshots = append(snapshots, result.Run)
			if result.Run.Status.Terminal() {
				t.Fatalf("CI monitor ended instead of waiting: %+v", result.Run)
			}
			stepRows, err := database.GetStepsByRun(id)
			if err != nil {
				t.Fatal(err)
			}
			if result.Run.Status != types.RunRunning || len(stepRows) != 1 || stepRows[0].Status != types.StepStatusRunning {
				ready = false
			}
		}
		var usage ipc.SlotsResult
		if err := client.Call(ipc.MethodGetSlots, nil, &usage); err != nil {
			t.Fatal(err)
		}
		if usage.MaxConcurrentRuns != 1 {
			t.Fatalf("live daemon cap = %d, want the configured cap of one", usage.MaxConcurrentRuns)
		}
		if ready && usage.InUse == 0 && usage.Queued == 0 && usage.ParkedReleased == 2 {
			wire, _ := json.MarshalIndent(struct {
				Slots ipc.SlotsResult `json:"slots"`
				Runs  []*ipc.RunInfo  `json:"runs"`
			}{usage, snapshots}, "", "  ")
			t.Logf("Live daemon get_slots and get_run responses:\n%s", wire)
			calls, _ := os.ReadFile(forgeCalls)
			t.Logf("Disposable forge calls:\n%s", calls)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("two pending CI monitors did not yield the one execution slot: %+v, runs %+v", usage, snapshots)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
