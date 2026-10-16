package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This persisted park is created by the real comparison guard. Recovery must
// install that same guard; a pass-only replacement cannot detect the mutation.
func TestTargetRewindRecoveryRealGuard(t *testing.T) {
	for _, mode := range []string{"source-mutation", "offline-abort"} {
		t.Run(mode, func(t *testing.T) {
			root, err := os.MkdirTemp("", "d")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			p := paths.WithRoot(root)
			if err = p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { d.Close() })
			claude := writeMockClaude(t, t.TempDir())
			if err = os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+claude+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			repo, base := setupTestGitRepo(t, p, d, "rewind-recovery")
			gate := p.RepoDir(repo.ID)
			remote := filepath.Join(root, "upstream.git")
			gitCmd(t, "", "clone", "--bare", gate, remote)
			gitCmd(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
			gitCmd(t, gate, "remote", "set-url", "origin", remote)
			repo, err = d.UpdateRepoMetadata(repo.ID, remote, "main")
			if err != nil {
				t.Fatal(err)
			}
			gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
			gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "feature")
			head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
			gitCmd(t, repo.WorkingPath, "push", remote, "feature")
			gitCmd(t, repo.WorkingPath, "push", "gate", "feature")
			run, err := d.InsertRun(repo.ID, "feature", head, base)
			if err != nil {
				t.Fatal(err)
			}
			if err = d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			run.Status = types.RunRunning
			worktree := p.WorktreeDir(repo.ID, run.ID)
			gitCmd(t, gate, "worktree", "add", "--detach", worktree, head)
			c := &pipeline.StepContext{
				Ctx: context.Background(), Run: run, Repo: repo, WorkDir: worktree, DB: d,
				Config: config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{}),
				Log:    func(s string) { t.Log(s) }, LogFile: func(s string) { t.Log(s) },
			}
			if _, err = steps.GuardPRContext(c, types.StepRebase); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, repo.WorkingPath, "checkout", "--detach", base)
			gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "advance target")
			target := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
			gitCmd(t, repo.WorkingPath, "push", remote, "HEAD:main")
			decision, err := steps.GuardPRContext(c, types.StepRebase)
			if err != nil || decision.RestartFrom != types.StepRebase {
				t.Fatalf("classify: %+v %v", decision, err)
			}
			rebase, err := d.InsertStepResult(run.ID, types.StepRebase)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.InsertStepResult(run.ID, types.StepCI); err != nil {
				t.Fatal(err)
			}
			if err = d.StartStep(rebase.ID); err != nil {
				t.Fatal(err)
			}
			findings := `{"findings":[{"id":"conflict","severity":"warning","description":"resolve conflict","action":"ask-user"}]}`
			round, err := d.InsertStepRound(rebase.ID, 1, "initial", &findings, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err = d.ParkStepForApproval(run.ID, rebase.ID, types.StepStatusAwaitingApproval, 0, 1, &findings, round.ID); err != nil {
				t.Fatal(err)
			}
			before, err := d.GetPendingTargetRewind(run.ID)
			if err != nil || before == nil {
				t.Fatalf("missing pending baseline: %+v %v", before, err)
			}
			if mode == "source-mutation" {
				gitCmd(t, remote, "update-ref", "refs/heads/feature", target)
			}
			runRecoveryTestDaemon(t, p, d)
			if mode == "offline-abort" {
				client, err := ipc.Dial(p.Socket())
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				deadline := time.Now().Add(15 * time.Second)
				for {
					var slots ipc.SlotsResult
					if err = client.Call(ipc.MethodGetSlots, nil, &slots); err != nil {
						t.Fatal(err)
					}
					if slots.ParkedReleased == 1 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("recovery did not park: %+v", slots)
					}
					time.Sleep(20 * time.Millisecond)
				}
				row, err := d.GetStepResult(rebase.ID)
				if err != nil {
					t.Fatal(err)
				}
				if row.GateIdentity().RoundID != round.ID {
					t.Fatal("recovery replaced the original gate")
				}
				if err = os.Rename(remote, remote+".offline"); err != nil {
					t.Fatal(err)
				}
				var result ipc.RespondResult
				if err = client.Call(ipc.MethodRespond, &ipc.RespondParams{RunID: run.ID, Step: types.StepRebase, Action: types.ActionAbort}, &result); err != nil || !result.OK {
					t.Fatalf("offline abort: %+v %v", result, err)
				}
			}
			terminal := waitForRunTerminalState(t, d, run.ID)
			want := "remote source changed during pending rewind"
			if mode == "offline-abort" {
				want = "aborted by user"
			}
			if terminal.Status != types.RunFailed || terminal.Error == nil || !strings.Contains(*terminal.Error, want) {
				t.Fatalf("recovered run failed incorrectly: %+v", terminal)
			}
			if pending, err := d.GetPendingTargetRewind(run.ID); err != nil || pending != nil {
				t.Fatalf("terminal recovery left pending: %+v %v", pending, err)
			}
			if terminal.TargetAdvanceRewinds != 1 {
				t.Fatal("recovery changed rewind charge")
			}
			row, err := d.GetStepResult(rebase.ID)
			if err != nil || row.PendingResponse != nil || row.Status != types.StepStatusFailed {
				t.Fatalf("recovery gate not settled: %+v %v", row, err)
			}
			rounds, err := d.GetRoundsByStep(rebase.ID)
			if err != nil || len(rounds) != 1 {
				t.Fatalf("unsafe recovery dispatched work: %+v %v", rounds, err)
			}
			t.Logf("real recovery guard: mode=%s error=%s; pending integration abandoned, charge retained", mode, *terminal.Error)
		})
	}
}
