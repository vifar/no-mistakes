package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The recovery contract requires a transient forge failure to preserve the
// pending reconciliation and its acceptance-time accounting. Exercise startup,
// real CIStep reconciliation, provider subprocesses and public IPC together.
// Only the external forge is disposable. A premature dispatch would erase the
// pending marker; failing instead of retrying would terminalize this run.
func TestQueueRecoveredRealCIReconciliationRetriesTransientForgeFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("external forge fixture uses a POSIX shell")
	}
	root, err := os.MkdirTemp("", "qretry")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repo, head := setupTestGitRepo(t, p, database, "retry-ci")
	forge := filepath.Join(root, "forge")
	failed := filepath.Join(root, "failed-read")
	release := filepath.Join(root, "release")
	calls := filepath.Join(root, "forge-calls")
	// pr find belongs to the comparison guard. pr view is the state read
	// made by the real CI reconciler. Hold that external failure until the
	// driver has observed the still-recoverable state through IPC and SQLite.
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> '%s'
case "$1 $2" in
  "status "*) echo '{"protocol_version":1,"capabilities":{},"max_pr_body_chars":0}' ;;
  "pr find") echo '{"pr":{"number":1,"url":"https://queue.invalid/test/repo/pulls/1","head_branch":"retry-ci","base_branch":"main","head_sha":"%s"}}' ;;
  "pr view")
    if [ ! -f '%s' ]; then
      touch '%s'
      echo 'temporary GetPRState transport failure' >&2
      exit 1
    fi
    echo '{"pr":{"number":1,"url":"https://queue.invalid/test/repo/pulls/1","head_branch":"retry-ci","base_branch":"main","head_sha":"%s"},"state":"closed","title":"Closed disposable PR","body":""}' ;;
  *) exit 1 ;;
esac
`, calls, head, release, failed, head)
	if err := os.WriteFile(forge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ag := writeMockClaude(t, t.TempDir())
	cfg := fmt.Sprintf("agent: claude\nagent_path_override:\n  claude: %s\ndaemon:\n  max_concurrent_runs: 1\ngate_reconcile_interval: 50ms\ngate_reconcile_timeout: 2s\nprovider_plugins:\n  fixture:\n    command: %s\n    hosts: [queue.invalid]\n", ag, forge)
	if err := os.WriteFile(p.ConfigFile(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, p.RepoDir(repo.ID), "config", "url."+p.RepoDir(repo.ID)+".insteadOf", "https://queue.invalid/test/repo.git")
	if _, err := database.ReplaceRepoURLs(repo.ID, "https://queue.invalid/test/repo.git", ""); err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "retry-ci", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	prURL := "https://queue.invalid/test/repo/pulls/1"
	if err := database.UpdateRunPRURL(run.ID, prURL); err != nil {
		t.Fatal(err)
	}
	workDir := p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), workDir, head); err != nil {
		t.Fatal(err)
	}
	testgit.BindRunComparison(t, database, run, workDir, "main", prURL, "https://queue.invalid/test/repo", "retry-ci")
	row, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(row.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[],"summary":"CI previously parked"}`
	if _, err := database.InsertStepRound(row.ID, 1, "initial", &findings, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, row.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
		t.Fatal(err)
	}
	// This serialized record is the durable acceptance contract, not a mock
	// of recovery: the pending reconciliation froze ten seconds at acceptance.
	pendingMarker := boundPendingFixture(t, database, row.ID, `{"kind":"reconcile","parked_ms":10000}`)
	if err := database.SetStepPendingResponse(row.ID, pendingMarker, "already answered; waiting for a run slot", databaseGateSnapshot(t, database, row.ID)); err != nil {
		t.Fatal(err)
	}
	// Restore a crash snapshot taken 120 seconds after parking, despite the
	// acceptance having frozen its parked duration at ten seconds.
	snapshotDB, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	_, err = snapshotDB.Exec("UPDATE runs SET awaiting_agent_since = ? WHERE id = ?", time.Now().Add(-120*time.Second).Unix(), run.ID)
	closeErr := snapshotDB.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("restore crash timestamp: %v, %v", err, closeErr)
	}
	runTestDaemon(t, p, database, func() []pipeline.Step { return []pipeline.Step{&steps.CIStep{}} }, 20*time.Second)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(failed); err == nil {
			break
		}
		current, _ := database.GetRun(run.ID)
		if current.Status.Terminal() || time.Now().After(deadline) {
			t.Fatalf("recovery did not reach the forge read: %+v", current)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Allow more than one retry so a failed first read cannot be mistaken
	// for a successful dispatch still executing elsewhere.
	time.Sleep(150 * time.Millisecond)
	current, err := database.GetRun(run.ID)
	if err != nil || current.Status != types.RunRunning {
		t.Fatalf("transient read failed the run: %+v, %v", current, err)
	}
	if err := pipeline.ValidateRecoveredRun(database, current, []pipeline.Step{&steps.CIStep{}}); err != nil {
		t.Fatalf("transient read lost recoverability: %v", err)
	}
	persisted, err := database.GetStepResult(row.ID)
	if err != nil || !persisted.AnsweredWaitingForSlot() || persisted.PendingResponse == nil || *persisted.PendingResponse != pendingMarker {
		t.Fatalf("transient read consumed the acceptance marker: %+v, %v", persisted, err)
	}
	var snapshot ipc.GetRunResult
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: run.ID}, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.AwaitingAgentSince != nil || len(snapshot.Run.Steps) != 1 || snapshot.Run.Steps[0].Status != types.StepStatusRunning || !snapshot.Run.Steps[0].ResponsePending {
		t.Fatalf("status advertised an unanswered gate: %+v", snapshot.Run)
	}
	wire, _ := json.MarshalIndent(snapshot, "", "  ")
	t.Logf("get_run during transient forge failure:\n%s\npersisted pending_response: %s", wire, *persisted.PendingResponse)
	var refusal ipc.RespondResult
	err = client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: run.ID, Step: types.StepCI, Action: types.ActionApprove, ExpectedGate: ipcGateSnapshot(t, client, run.ID, types.StepCI)}, &refusal)
	if err != nil || refusal.OK || refusal.Code != "gate_superseded" {
		t.Fatalf("second response: %v", err)
	}
	t.Logf("second respond refused: %v", err)
	// R1 also covers shutdown while recovered reconciliation is retrying.
	// Keep the external failure active across a real daemon stop and start;
	// neither its durable acceptance nor its worktree may be consumed.
	client.Close()
	shutdownTestDaemonAndWaitForCleanup(t, p)
	current, err = database.GetRun(run.ID)
	if err != nil || current.Status != types.RunRunning || current.AwaitingAgentSince == nil {
		t.Fatalf("shutdown lost the retrying parked run: %+v, %v", current, err)
	}
	persisted, err = database.GetStepResult(row.ID)
	if err != nil || persisted.PendingResponse == nil || *persisted.PendingResponse != pendingMarker {
		t.Fatalf("shutdown lost the retrying acceptance: %+v, %v", persisted, err)
	}
	if _, err := os.Stat(workDir); err != nil {
		t.Fatalf("shutdown removed the retrying run's worktree: %v", err)
	}
	t.Logf("shutdown during forge retry preserved running gate and worktree; pending_response=%s", *persisted.PendingResponse)
	runTestDaemon(t, p, database, func() []pipeline.Step { return []pipeline.Step{&steps.CIStep{}} }, 20*time.Second)
	client, err = ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: run.ID}, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Run.Status != types.RunRunning || len(snapshot.Run.Steps) != 1 || !snapshot.Run.Steps[0].ResponsePending {
		t.Fatalf("restart lost the retrying accepted gate: %+v", snapshot.Run)
	}
	wire, _ = json.MarshalIndent(snapshot, "", "  ")
	t.Logf("get_run after restarting during transient forge failure:\n%s", wire)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		current, err = database.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reconciliation retry did not finish: %+v", current)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if current.Status != types.RunCompleted || current.ParkedMS != 10000 || current.AwaitingAgentSince != nil {
		t.Fatalf("recovered completion/accounting: %+v", current)
	}
	persisted, err = database.GetStepResult(row.ID)
	if err != nil || persisted.PendingResponse != nil || persisted.Status != types.StepStatusCompleted {
		t.Fatalf("dispatch did not atomically consume the marker: %+v, %v", persisted, err)
	}
	wire, _ = json.MarshalIndent(current, "", "  ")
	t.Logf("persisted run after successful forge retry:\n%s", wire)
	data, _ := os.ReadFile(calls)
	t.Logf("disposable forge calls:\n%s", data)
}
