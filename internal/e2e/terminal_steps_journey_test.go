//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Drive the actual CLI/daemon through the issue #102 legacy database shape.
// The fake agent and forge are external boundaries; Git, DB and recovery are real.
func TestAxiTerminalStalePushRecoveryJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-stale", "seed.txt", "seed\n", "seed stale init")
	init := h.AddWorktree("init-stale")
	if out, err := h.RunInDir(init, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	submitted := h.CommitChange("feature/stale-push", "feature.txt", "unsafe\n", "feature")
	operator := h.AddWorktree("feature/stale-push")
	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "preserve unpublished fixes when cancelling")
	if err != nil || !strings.Contains(gateOut, "sync-1") {
		t.Fatalf("review: %v\n%s", err, gateOut)
	}
	if out, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err != nil {
		t.Fatalf("fix: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(operator, "axi", "abort"); err != nil {
		t.Fatalf("abort: %v\n%s", err, out)
	} else {
		t.Logf("ordinary abort:\n%s", out)
	}
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run, err := database.GetActiveRun(h.repoID(), "feature/stale-push")
	if err != nil || run != nil {
		t.Fatalf("active run after abort: %+v %v", run, err)
	}
	runs, err := database.GetRunsByRepo(h.repoID())
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range runs {
		if candidate.Branch == "feature/stale-push" {
			run = candidate
			break
		}
	}
	if run == nil || run.Status != types.RunCancelled || run.HeadSHA == submitted {
		t.Fatalf("wrong legacy fixture: %+v", run)
	}
	if run.LastPushedSHA != nil {
		t.Fatalf("aborted unreviewed head acquired a push receipt: %s", *run.LastPushedSHA)
	}
	if out, err := h.runGit(context.Background(), h.UpstreamDir, "show-ref", "--verify", "refs/heads/feature/stale-push"); err == nil || len(out) != 0 && !strings.Contains(string(out), "not a valid ref") {
		t.Fatalf("unreviewed branch reached upstream or absence could not be established: %v %s", err, out)
	}
	t.Log("real upstream has no feature/stale-push branch; unreviewed head was not published")
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	anchor, err := h.runGit(context.Background(), gateDir, "rev-parse", custody.RecoveryRef(run.ID))
	if err != nil || strings.TrimSpace(string(anchor)) != run.HeadSHA {
		t.Fatalf("recovery ref: %s %v", anchor, err)
	}
	rows, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		switch row.Status {
		case types.StepStatusRunning, types.StepStatusFixing, types.StepStatusAwaitingApproval, types.StepStatusFixReview:
			t.Fatalf("ordinary cancellation left an executing row: %+v", row)
		}
	}
	// Make the unpublished commit reachable only through its recovery anchor.
	if out, err := h.runGit(context.Background(), gateDir, "update-ref", "refs/heads/feature/stale-push", submitted); err != nil {
		t.Fatalf("restore submitted gate head: %v %s", err, out)
	}
	t.Logf("cancelled run %s: all executing/parked rows closed; recovery anchor=%s; operator and gate branch=%s", run.ID, run.HeadSHA, submitted)
	// Re-introduce the old persisted defect after quiescence, with no executor.
	step, err := database.InsertStepResult(run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	status, err := h.RunInDir(operator, "axi", "status")
	if err != nil || !strings.Contains(status, "recover_custody") || strings.Contains(status, "continue_active_run") {
		t.Fatalf("status: %v\n%s", err, status)
	}
	t.Logf("status with PID-less orphan:\n%s", status)
	pid := os.Getpid()
	if err := database.SetStepAgentActivity(step.ID, "live process", &pid); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunPushActive(run.ID, true); err != nil {
		t.Fatal(err)
	}
	blocked, blockedErr := h.RunInDir(operator, "axi", "sync", "--recover")
	t.Logf("recovery blocked by stale push marker/live PID (exit %v):\n%s", blockedErr, blocked)
	for _, want := range []string{run.ID, step.ID, "push_active", "cleanup_terminal_steps", "axi abort --run " + run.ID} {
		if !strings.Contains(blocked, want) {
			t.Fatalf("recovery diagnostic missing %q: %s", want, blocked)
		}
	}
	refused, refusedErr := h.RunInDir(operator, "axi", "abort", "--run", run.ID)
	t.Logf("cleanup refuses live PID (exit %v):\n%s", refusedErr, refused)
	if refusedErr == nil || !strings.Contains(refused, step.ID) || !strings.Contains(refused, run.ID) {
		t.Fatalf("live PID cleanup not refused with row identity: %v %s", refusedErr, refused)
	}
	stillRunning, err := database.GetStepResult(step.ID)
	if err != nil || stillRunning.Status != types.StepStatusRunning || stillRunning.CompletedAt != nil {
		t.Fatalf("live row changed: %+v %v", stillRunning, err)
	}
	if err := database.SetStepAgentActivity(step.ID, "process stopped", nil); err != nil {
		t.Fatal(err)
	}
	out, err := h.RunInDir(operator, "axi", "abort", "--run", run.ID)
	if err != nil {
		t.Fatalf("legacy abort: %v\n%s", err, out)
	}
	if !strings.Contains(out, "aborted: false") || !strings.Contains(out, "steps_closed: 1") {
		t.Fatalf("cleanup fabricated a new cancellation: %s", out)
	}
	t.Logf("terminal cleanup:\n%s", out)
	row, err := database.GetStepResult(step.ID)
	if err != nil || row.CompletedAt == nil || row.Status != types.StepStatusFailed {
		t.Fatalf("push row not closed: %+v %v", row, err)
	}
	again, err := h.RunInDir(operator, "axi", "abort", "--run", run.ID)
	if err != nil || !strings.Contains(again, "aborted: false") {
		t.Fatalf("repeat abort: %v\n%s", err, again)
	}
	t.Logf("repeat abort:\n%s", again)
	recovered, err := h.RunInDir(operator, "axi", "sync", "--recover")
	if err != nil {
		t.Fatalf("recover: %v\n%s", err, recovered)
	}
	t.Logf("recovery from anchor:\n%s", recovered)
	final, err := database.GetRun(run.ID)
	if err != nil || final.CustodyReturnedAt == nil {
		t.Fatalf("custody: %+v %v", final, err)
	}
	if got := h.WorktreeRefSHA("feature/stale-push"); got != run.HeadSHA {
		t.Fatalf("local head=%s want %s", got, run.HeadSHA)
	}
	t.Logf("status offers recovery; abort closes step %s; repeat abort is a no-op; sync --recover returns custody at %s", step.ID, run.HeadSHA)
}

// The production daemon's minute timer, not a direct helper call, owns this sweep.
// Old database timestamps model legacy rows without changing the product clock.
func TestTerminalStepMaintenanceLiveJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("daemon", "start"); err != nil {
		t.Fatalf("start isolated daemon: %v\n%s", err, out)
	}
	p := paths.WithRoot(h.NMHome)
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(h.WorkDir, h.UpstreamDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", p.DB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	type seeded struct {
		name string
		run  *db.Run
		step *db.StepResult
	}
	var cases []seeded
	for _, name := range []string{"quiet", "recent database", "live process", "unverifiable PID", "recent log", "unreadable log", "active native-free", "certified watcher"} {
		run, err := database.InsertRun(repo.ID, name, "unpublished-head", "base")
		if err != nil {
			t.Fatal(err)
		}
		step, err := database.InsertStepResult(run.ID, types.StepPush)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.StartStep(step.ID); err != nil {
			t.Fatal(err)
		}
		if name == "active native-free" {
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := database.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
				t.Fatal(err)
			}
		}
		stamp := time.Now().Add(-5 * time.Minute).Unix()
		if name == "recent database" {
			stamp = time.Now().Unix()
		}
		if _, err := raw.Exec("UPDATE step_results SET started_at=?, round_started_at=?, last_activity_at=?, findings_json=? WHERE id=?", stamp, stamp, stamp, `[{"id":"retained-evidence"}]`, step.ID); err != nil {
			t.Fatal(err)
		}
		if name == "live process" || name == "unverifiable PID" {
			pid := os.Getpid()
			if name == "unverifiable PID" {
				pid = 0
			}
			if _, err := raw.Exec("UPDATE step_results SET agent_pid=? WHERE id=?", pid, step.ID); err != nil {
				t.Fatal(err)
			}
		}
		if name == "recent log" || name == "unreadable log" || name == "quiet" {
			log := filepath.Join(p.RunLogDir(run.ID), "push.log")
			if err := os.MkdirAll(filepath.Dir(log), 0700); err != nil {
				t.Fatal(err)
			}
			if name == "unreadable log" {
				if err := os.Mkdir(log, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(log, []byte("retained step log\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if name == "quiet" {
					old := time.Unix(stamp, 0)
					if err := os.Chtimes(log, old, old); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		if name == "certified watcher" {
			if _, err := raw.Exec("INSERT INTO commit_status_publications (run_id,sha,host,repo,last_state,preserve_success,strict_certificate) VALUES (?,?,?,?,?,?,?)", run.ID, "unpublished-head", "fixture.test", "owner/repo", "success", 1, "held-certificate"); err != nil {
				t.Fatal(err)
			}
		}
		cases = append(cases, seeded{name, run, step})
	}
	deadline := time.Now().Add(80 * time.Second)
	for {
		row, err := database.GetStepResult(cases[0].step.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status == types.StepStatusFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("production daemon did not sweep the quiet legacy row within one minute plus scheduling allowance")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, tc := range cases {
		row, err := database.GetStepResult(tc.step.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := types.StepStatusRunning
		if tc.name == "quiet" {
			want = types.StepStatusFailed
		}
		if row.FindingsJSON == nil {
			t.Fatalf("sweep lost findings for %s", tc.name)
		}
		t.Logf("production sweep: %s: run=%s step=%s status=%s completed=%t findings=%s", tc.name, tc.run.ID, row.ID, row.Status, row.CompletedAt != nil, *row.FindingsJSON)
		if row.Status != want || (row.CompletedAt != nil) != (tc.name == "quiet") || row.FindingsJSON == nil || !strings.Contains(*row.FindingsJSON, "retained-evidence") {
			t.Fatalf("sweep violated protection/preservation for %s: %+v", tc.name, row)
		}
	}
	log, err := os.ReadFile(filepath.Join(p.RunLogDir(cases[0].run.ID), "push.log"))
	if err != nil || string(log) != "retained step log\n" {
		t.Fatalf("cleanup lost step log: %q %v", log, err)
	}
	// Retire the synthetic active row after proving that PID absence did not
	// authorize a sweep; normal daemon stop refuses any unsettled active run.
	for _, tc := range cases {
		if tc.name == "active native-free" {
			if err := database.UpdateRunStatus(tc.run.ID, types.RunCancelled); err != nil {
				t.Fatal(err)
			}
		}
	}
	if out, err := h.Run("daemon", "stop"); err != nil {
		t.Fatalf("stop isolated daemon: %v\n%s", err, out)
	} else {
		t.Logf("isolated daemon stopped:\n%s", out)
	}
}

// Stall the OS observation used by maintenance, then abort an unrelated live
// run through the real CLI. The ps shim is the external boundary; it forwards
// every other request to the actual system ps found on PATH.
func TestTerminalStepBlockedProbeAllowsLiveAbortJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-probe", "seed.txt", "seed\n", "seed")
	init := h.AddWorktree("init-probe")
	if out, err := h.RunInDir(init, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange("feature/abort-probe", "feature.txt", "unsafe\n", "feature")
	operator := h.AddWorktree("feature/abort-probe")
	if out, err := h.RunInDir(operator, "axi", "run", "--intent", "exercise responsive cancellation"); err != nil || !strings.Contains(out, "sync-1") {
		t.Fatalf("park live run: %v\n%s", err, out)
	}
	p := paths.WithRoot(h.NMHome)
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run, err := database.InsertRun(h.repoID(), "legacy-probe", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	realPS, err := exec.LookPath("ps")
	if err != nil {
		t.Fatal(err)
	}
	// An exited but not yet waited-for child is a real zombie. The OS probe
	// will establish it is dead, allowing the conditional database write.
	child := exec.Command("sh", "-c", "exit 0")
	child.Dir = h.HomeDir
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Wait()
	pid := child.Process.Pid
	zombieDeadline := time.Now().Add(2 * time.Second)
	for {
		state, err := exec.Command(realPS, "-p", strconv.Itoa(pid), "-o", "stat=").Output()
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
			break
		}
		if time.Now().After(zombieDeadline) {
			t.Fatalf("disposable child did not reach zombie state: %v %s", err, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := database.SetStepAgentActivity(step.ID, "probe exited child", &pid); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", p.DB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	old := time.Now().Add(-10 * time.Minute).Unix()
	if _, err := raw.Exec("UPDATE step_results SET started_at=?, round_started_at=?, last_activity_at=? WHERE id=?", old, old, old, step.ID); err != nil {
		t.Fatal(err)
	}
	entered := filepath.Join(h.NMHome, "probe-entered")
	release := filepath.Join(h.NMHome, "probe-release")
	defer func() { _ = os.WriteFile(release, nil, 0600) }()
	shim := "#!/bin/sh\nif [ \"$*\" = " + shellQuote("-p "+strconv.Itoa(pid)+" -o stat=") + " ]; then\n  : > " + shellQuote(entered) + "\n  while [ ! -f " + shellQuote(release) + " ]; do sleep 0.05; done\nfi\nexec " + shellQuote(realPS) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(h.BinDir, "ps"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(75 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("maintenance never entered the blocking OS probe")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// This update lands after the daemon read its stale snapshot and while
	// its genuine process-state observation is held at the OS boundary.
	fresh := time.Now().Unix()
	if _, err := raw.Exec("UPDATE step_results SET last_activity_at=? WHERE id=?", fresh, step.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, h.NMBin, "axi", "abort")
	command.Dir = operator
	command.Env = os.Environ()
	started := time.Now()
	out, err := command.CombinedOutput()
	elapsed := time.Since(started)
	t.Logf("active abort while maintenance OS probe is blocked: elapsed=%s exit=%v\n%s", elapsed, err, out)
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err != nil || !strings.Contains(string(out), "aborted: true") || !strings.Contains(string(out), "run_status: cancelled") {
		t.Fatalf("active cancellation stalled behind maintenance: %v %s", err, out)
	}
	if out, err := h.Run("daemon", "stop"); err != nil {
		t.Fatalf("stop isolated daemon: %v\n%s", err, out)
	} else {
		t.Logf("isolated daemon stopped after probe release:\n%s", out)
	}
	row, err := database.GetStepResult(step.ID)
	if err != nil || row.Status != types.StepStatusRunning || row.CompletedAt != nil || row.LastActivityAt == nil || *row.LastActivityAt != fresh {
		t.Fatalf("stale sweep overwrote activity changed during OS probe: %+v %v", row, err)
	}
	t.Logf("conditional-write fence retained step %s running with activity=%d after its dead-PID snapshot was invalidated", row.ID, fresh)
}

// Stop the actual daemon while its maintenance process observation is blocked.
// The external ps boundary stalls; the product must cancel it and retain the
// row because an interrupted observation cannot prove that a process is dead.
func TestTerminalMaintenanceProbeShutdownLiveJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("daemon", "start"); err != nil {
		t.Fatalf("start isolated daemon: %v\n%s", err, out)
	}
	p := paths.WithRoot(h.NMHome)
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(h.WorkDir, h.UpstreamDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "blocked-probe-shutdown", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	if err := database.SetStepAgentActivity(step.ID, "real live process", &pid); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", p.DB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	old := time.Now().Add(-10 * time.Minute).Unix()
	if _, err := raw.Exec("UPDATE step_results SET started_at=?, round_started_at=?, last_activity_at=? WHERE id=?", old, old, old, step.ID); err != nil {
		t.Fatal(err)
	}
	realPS, err := exec.LookPath("ps")
	if err != nil {
		t.Fatal(err)
	}
	entered := filepath.Join(h.NMHome, "shutdown-probe-entered")
	shim := "#!/bin/sh\nif [ \"$*\" = " + shellQuote("-p "+strconv.Itoa(pid)+" -o stat=") + " ]; then\n  echo $$ > " + shellQuote(entered) + "\n  exec sleep 30\nfi\nexec " + shellQuote(realPS) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(h.BinDir, "ps"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(75 * time.Second)
	var probePID int
	for {
		if data, err := os.ReadFile(entered); err == nil {
			probePID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("maintenance did not enter the blocked OS probe")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, h.NMBin, "daemon", "stop")
	command.Dir = h.WorkDir
	command.Env = os.Environ()
	started := time.Now()
	out, err := command.CombinedOutput()
	t.Logf("daemon stop with maintenance ps blocked: elapsed=%s exit=%v\n%s", time.Since(started), err, out)
	if err != nil {
		t.Fatalf("shutdown waited for the blocked observation: %v %s", err, out)
	}
	state, err := exec.Command(realPS, "-p", strconv.Itoa(probePID), "-o", "stat=").Output()
	if err == nil && strings.TrimSpace(string(state)) != "" {
		t.Fatalf("shutdown left its probe child %d alive: %s", probePID, state)
	}
	row, err := database.GetStepResult(step.ID)
	if err != nil || row.Status != types.StepStatusRunning || row.CompletedAt != nil || row.AgentPID == nil || *row.AgentPID != pid {
		t.Fatalf("cancelled observation changed the protected row: %+v %v", row, err)
	}
	t.Logf("protected step %s remains running with live PID %d; blocked probe child %d is gone", row.ID, pid, probePID)
}

// Startup cleanup must distinguish an administrative interruption from a real
// Document refusal while retaining the latter's uncommitted owner work.
func TestTerminalDocumentRetentionLiveJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	p := paths.WithRoot(h.NMHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(h.WorkDir, h.UpstreamDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	var interruptedDir, refusedDir string
	for _, actualFailure := range []bool{false, true} {
		branch := "interrupted-document"
		if actualFailure {
			branch = "refused-document"
		}
		run, err := database.InsertRun(repo.ID, branch, h.WorktreeRefSHA("main"), h.WorktreeRefSHA("main"))
		if err != nil {
			t.Fatal(err)
		}
		dir := p.WorktreeDir(repo.ID, run.ID)
		if out, err := h.runGit(context.Background(), h.WorkDir, "worktree", "add", "--detach", dir, "main"); err != nil {
			t.Fatalf("create disposable real worktree: %v %s", err, out)
		}
		if err := database.SetRunWorktreeDir(run.ID, dir); err != nil {
			t.Fatal(err)
		}
		step, err := database.InsertStepResult(run.ID, types.StepDocument)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.StartStep(step.ID); err != nil {
			t.Fatal(err)
		}
		if actualFailure {
			if err := database.FailStep(step.ID, "cannot safely discard owner work", 0); err != nil {
				t.Fatal(err)
			}
			refusedDir = dir
		} else {
			interruptedDir = dir
		}
		if err := database.RecordIneligibleTerminal(run.ID, "independent storage recovery failure", types.RunFailed, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "owner.txt"), []byte("uncommitted owner work\n"), 0600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-30 * 24 * time.Hour)
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := h.Run("daemon", "start"); err != nil {
		t.Fatalf("start isolated daemon: %v\n%s", err, out)
	}
	if _, err := os.Stat(interruptedDir); !os.IsNotExist(err) {
		t.Fatalf("administrative Document interruption falsely retained expired worktree: %v", err)
	}
	owner, err := os.ReadFile(filepath.Join(refusedDir, "owner.txt"))
	if err != nil || string(owner) != "uncommitted owner work\n" {
		t.Fatalf("genuine Document refusal lost owner work: %q %v", owner, err)
	}
	t.Log("real daemon startup removed the administratively interrupted Document checkout and preserved exact uncommitted bytes after a genuine Document refusal")
	if out, err := h.Run("daemon", "stop"); err != nil {
		t.Fatalf("stop isolated daemon: %v\n%s", err, out)
	} else {
		t.Logf("isolated daemon stopped:\n%s", out)
	}
}
