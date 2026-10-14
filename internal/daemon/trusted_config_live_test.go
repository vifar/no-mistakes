package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The independent oracle is the trusted-config contract: both branch starts
// must execute the fresh maintainer command, not the pushed or tracking tree.
// A production repository gate parks before any later base-branch fetch; no
// agent turn or delivery step is invoked. Separate connections preserve concurrency.
func TestTrustedConfigConcurrentStartupLive(t *testing.T) {
	for _, agentName := range []string{"claude", "pi"} {
		t.Run(agentName, func(t *testing.T) {
			p, d := startTestDaemonWithSteps(t, nil)
			repo, _ := setupTestGitRepo(t, p, d, "concurrent-live")
			bare := p.RepoDir(repo.ID)
			head := commitDefaultBranchConfig(t, repo.WorkingPath, "commands:\n  test: echo untrusted-pushed-command\n")
			gitCmd(t, bare, "fetch", "origin", "main:refs/remotes/origin/main")
			fetchHead := filepath.Join(bare, "FETCH_HEAD")
			before, err := os.ReadFile(fetchHead)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fetchHead+".lock", nil, 0600); err != nil {
				t.Fatal(err)
			}
			fresh := commitDefaultBranchConfig(t, repo.WorkingPath, "agent: "+agentName+"\ngates:\n  - name: observe\n    after: rebase\n    command: 'printf fresh-trusted > trusted-observed.txt; exit 1'\n")
			if agentName == "pi" {
				adapter := writeMockClaude(t, t.TempDir())
				if err := os.WriteFile(p.ConfigFile(), []byte("agent: pi\nagent_path_override:\n  pi: "+adapter+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := git.IsolateHooksPath(context.Background(), bare); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bare, "refs", "remotes", "origin", "main.lock"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			branches := []string{"feature/one", "feature/two"}
			for _, branch := range branches {
				gitCmd(t, bare, "update-ref", "refs/heads/"+branch, head)
			}
			errs := make([]error, len(branches))
			var wg sync.WaitGroup
			for i, branch := range branches {
				wg.Add(1)
				go func(i int, branch string) {
					defer wg.Done()
					client, err := ipc.Dial(p.Socket())
					if err != nil {
						errs[i] = err
						return
					}
					defer client.Close()
					params := &ipc.PushReceivedParams{
						Gate:      bare,
						Ref:       "refs/heads/" + branch,
						Old:       strings.Repeat("0", 40),
						New:       head,
						SkipSteps: []types.StepName{types.StepIntent, types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI},
					}
					if agentName == "pi" {
						params.PiProfile = &agentcfg.PiProfile{Model: "openai-codex/gpt-5.4", Effort: agentcfg.EffortHigh}
					}
					var result ipc.PushReceivedResult
					errs[i] = client.Call(ipc.MethodPushReceived, params, &result)
				}(i, branch)
			}
			wg.Wait()
			for i, branch := range branches {
				if errs[i] != nil {
					t.Fatalf("concurrent startup for %s: %v", branch, errs[i])
				}
				run, err := d.GetActiveRun(repo.ID, branch)
				if err != nil || run == nil {
					t.Fatalf("active run for %s: %+v, %v", branch, run, err)
				}
				if agentName == "pi" && (run.PiProfile == nil || run.PiProfile.Model != "openai-codex/gpt-5.4" || run.PiProfile.Effort != agentcfg.EffortHigh) {
					t.Fatalf("accepted Pi profile was not persisted: %+v", run.PiProfile)
				}
				marker := filepath.Join(p.WorktreeDir(repo.ID, run.ID), "trusted-observed.txt")
				deadline := time.Now().Add(10 * time.Second)
				for {
					data, err := os.ReadFile(marker)
					if err == nil && string(data) == "fresh-trusted" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("trusted command did not execute for %s: %q, %v", branch, data, err)
					}
					time.Sleep(20 * time.Millisecond)
				}
				// IPC acceptance precedes asynchronous executor startup. The
				// command output proves it started before checking its status.
				run, err = d.GetActiveRun(repo.ID, branch)
				if err != nil || run == nil || run.Status != types.RunRunning {
					t.Fatalf("active run after trusted command for %s: %+v, %v", branch, run, err)
				}
				t.Logf("IPC accepted branch=%s agent=%s run=%s; production repository gate emitted fresh-trusted from default SHA=%s (pushed SHA=%s)", branch, agentName, run.ID, fresh, head)
			}
			if got := gitOutput(t, bare, "rev-parse", "refs/remotes/origin/main"); got != head {
				t.Fatalf("shared tracking ref moved: %s, want %s", got, head)
			}
			after, err := os.ReadFile(fetchHead)
			if err != nil || string(after) != string(before) {
				t.Fatalf("FETCH_HEAD changed: %q, %v", after, err)
			}
			refs, err := git.Run(context.Background(), bare, "for-each-ref", "--format=%(refname)", "refs/no-mistakes/trusted-config/")
			if err != nil || strings.TrimSpace(refs) != "" {
				t.Fatalf("private refs leaked: %q, %v", refs, err)
			}
			t.Log("locked tracking ref and FETCH_HEAD preserved; per-call private refs removed")
		})
	}
}

// Drive startup recovery through the running daemon and its IPC interface.
// The oracle is the trust contract: a parked run survives a sibling's tracking
// lock, but an unavailable or newly invalid trusted tree cannot use stale data.
// All pipeline steps are production steps; the parked review is never released.
func TestTrustedConfigRecoveryLive(t *testing.T) {
	for _, mode := range []string{"locked-tracking-ref", "unavailable-origin", "fresh-invalid-config"} {
		t.Run(mode, func(t *testing.T) {
			root, err := os.MkdirTemp("", "d")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			p := paths.WithRoot(root)
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			// Recovery constructs the adapter but does not invoke it while parked.
			claude := writeMockClaude(t, t.TempDir())
			if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+claude+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			repo, head := setupTestGitRepo(t, p, d, "trusted-live")
			bare := p.RepoDir(repo.ID)
			gitCmd(t, bare, "fetch", "origin", "main:refs/remotes/origin/main")
			fetchHead := filepath.Join(bare, "FETCH_HEAD")
			before, err := os.ReadFile(fetchHead)
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRun(repo.ID, "feature/recovery", head, head)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, bare, "worktree", "add", "--detach", p.WorktreeDir(repo.ID, run.ID), head)
			parkRunAtReviewGate(t, d, run.ID, steps.AllSteps())
			if err := d.SetRunAwaitingAgent(run.ID); err != nil {
				t.Fatal(err)
			}
			if mode == "unavailable-origin" {
				gitCmd(t, bare, "remote", "set-url", "origin", filepath.Join(root, "missing-origin"))
			} else {
				if mode == "fresh-invalid-config" {
					commitDefaultBranchConfig(t, repo.WorkingPath, "agent: invalid-agent\n")
				}
				if err := os.WriteFile(filepath.Join(bare, "refs", "remotes", "origin", "main.lock"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			runTestDaemon(t, p, d, nil, 35*time.Second)
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var result ipc.GetRunResult
			if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: run.ID}, &result); err != nil {
				t.Fatal(err)
			}
			if mode == "locked-tracking-ref" {
				if result.Run.Status != types.RunRunning || result.Run.AwaitingAgentSince == nil {
					t.Fatalf("parked run lost during locked-ref recovery: %+v", result.Run)
				}
			} else if result.Run.Status != types.RunFailed {
				t.Fatalf("unsafe recovery used stale trusted tree: %+v", result.Run)
			}
			if got := gitOutput(t, bare, "rev-parse", "refs/remotes/origin/main"); got != head {
				t.Fatalf("shared tracking ref moved: %s, want %s", got, head)
			}
			after, err := os.ReadFile(fetchHead)
			if err != nil || string(after) != string(before) {
				t.Fatalf("FETCH_HEAD changed: %q, %v", after, err)
			}
			refs, err := git.Run(context.Background(), bare, "for-each-ref", "--format=%(refname)", "refs/no-mistakes/trusted-config/")
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(refs) != "" {
				t.Fatalf("private refs leaked: %s", refs)
			}
			t.Logf("IPC run status=%s; parked=%v; tracking SHA=%s; FETCH_HEAD preserved; private refs removed", result.Run.Status, result.Run.AwaitingAgentSince != nil, head)
		})
	}
}

// The daemon must read freshly fetched trusted config before accepting a new
// run or a Pi pin. Refusals must preserve any existing validation on the branch.
func TestTrustedConfigStartupRefusalLive(t *testing.T) {
	for _, mode := range []string{"unavailable-origin", "fresh-agent", "pi-trusted-agent", "pi-unavailable-origin"} {
		t.Run(mode, func(t *testing.T) {
			p, d := startTestDaemonWithSteps(t, nil)
			repo, head := setupTestGitRepo(t, p, d, "startup-live")
			bare := p.RepoDir(repo.ID)
			gitCmd(t, bare, "fetch", "origin", "main:refs/remotes/origin/main")
			fetchHead := filepath.Join(bare, "FETCH_HEAD")
			before, err := os.ReadFile(fetchHead)
			if err != nil {
				t.Fatal(err)
			}
			isPi := strings.HasPrefix(mode, "pi-")
			params := &ipc.PushReceivedParams{Gate: bare, Ref: "refs/heads/feature/new", Old: strings.Repeat("0", 40), New: head}
			gitCmd(t, bare, "update-ref", params.Ref, head)
			var active *db.Run
			if isPi {
				if err := os.WriteFile(p.ConfigFile(), []byte("agent: pi\n"), 0600); err != nil {
					t.Fatal(err)
				}
				params.PiProfile = &agentcfg.PiProfile{Model: "openai-codex/gpt-5.4", Effort: agentcfg.EffortHigh}
				active, err = d.InsertRun(repo.ID, "feature/new", head, head)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := pipeline.RecordRunFailure(context.Background(), d, active.ID, "test fixture finished", nil); err != nil {
						t.Errorf("settle owned Pi refusal fixture: %v", err)
					}
				})
				if err := d.UpdateRunStatus(active.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
			}
			wantError := "failed to fetch or resolve trusted default branch"
			if strings.Contains(mode, "unavailable-origin") {
				gitCmd(t, bare, "remote", "set-url", "origin", filepath.Join(p.Root(), "missing-origin"))
				if isPi {
					wantError = "failed to fetch trusted default branch"
				}
			} else {
				yaml := "agent: invalid-agent\n"
				wantError = "invalid-agent"
				if isPi {
					yaml = "agent: claude\n"
					wantError = "Pi run profile requires agent: pi without non-Pi fallbacks"
				}
				commitDefaultBranchConfig(t, repo.WorkingPath, yaml)
				if err := os.WriteFile(filepath.Join(bare, "refs", "remotes", "origin", "main.lock"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var response ipc.PushReceivedResult
			err = client.Call(ipc.MethodPushReceived, params, &response)
			if err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("startup refusal = %v; want %s", err, wantError)
			}
			t.Logf("push_received refusal: %s", err)
			if isPi {
				runs, err := d.GetRunsByRepo(repo.ID)
				if err != nil || len(runs) != 1 || runs[0].ID != active.ID || runs[0].Status != types.RunRunning {
					t.Fatalf("refused pin altered existing validation: %+v, %v", runs, err)
				}
				t.Log("refused Pi pin preserved the existing running validation")
			}
			if got := gitOutput(t, bare, "rev-parse", "refs/remotes/origin/main"); got != head {
				t.Fatalf("shared tracking ref moved: %s, want %s", got, head)
			}
			after, err := os.ReadFile(fetchHead)
			if err != nil || string(after) != string(before) {
				t.Fatalf("FETCH_HEAD changed: %q, %v", after, err)
			}
			refs, err := git.Run(context.Background(), bare, "for-each-ref", "--format=%(refname)", "refs/no-mistakes/trusted-config/")
			if err != nil || strings.TrimSpace(refs) != "" {
				t.Fatalf("private refs leaked: %q, %v", refs, err)
			}
			t.Logf("tracking SHA=%s; FETCH_HEAD preserved; private refs removed", head)
		})
	}
}
