package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

type publishedPanicStep struct{ execute func(*pipeline.StepContext) }

func (*publishedPanicStep) Name() types.StepName { return types.StepReview }
func (s *publishedPanicStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.execute(ctx)
	return nil, nil
}

func TestDaemonPanic_SerializesPausedSuccessAndRecoversFailure(t *testing.T) {
	bin := t.TempDir()
	if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, "gh"), "../pipeline/fakecli").CombinedOutput(); err != nil {
		t.Fatalf("fake gh: %v: %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "gh")
	entered, release, panicking, callbackDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var mu sync.Mutex
	var states []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ State string }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.State == "success" {
			close(entered)
			<-release
		}
		mu.Lock()
		states = append(states, payload.State)
		mu.Unlock()
		w.WriteHeader(201)
	}))
	defer server.Close()
	defer unblock()
	t.Setenv("FAKE_CLI_STATUS_API_URL", server.URL)
	step := &publishedPanicStep{execute: func(sc *pipeline.StepContext) {
		if err := sc.DB.PutCommitStatusPublication(sc.Run.ID, db.CommitStatusPublication{SHA: sc.Run.HeadSHA, Host: "github.com", Repo: "test/repo", Validated: true, CIEligible: true}); err != nil {
			panic(err)
		}
		go func() { defer close(callbackDone); (&steps.PushStep{}).ReportPublishedHeadStatus(sc, "success", "") }()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			panic("success never reached endpoint")
		}
		close(panicking)
		panic("published CI panic")
	}}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{step, &steps.PushStep{}} })
	_, head := setupTestGitRepo(t, p, d, "panic-status")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: p.RepoDir("panic-status"), Ref: "refs/heads/main", Old: "0000000000000000000000000000000000000000", New: head}, &result); err != nil {
		t.Fatal(err)
	}
	select {
	case <-panicking:
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not reach actual panic")
	}
	// The callback is still blocked in the real HTTP endpoint. Panic recovery
	// must wait for its publication lock before changing the terminal outcome.
	time.Sleep(250 * time.Millisecond)
	run, err := d.GetRun(result.RunID)
	if err != nil || run.Status != types.RunRunning {
		t.Fatalf("panic terminal write bypassed paused reporter: %+v,%v", run, err)
	}
	unblock()
	<-callbackDone
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		publication, err := d.GetCommitStatusPublication(context.Background(), result.RunID, db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo"})
		if err == nil && publication != nil && publication.LastState == "failure" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	shutdownTestDaemonAndWaitForCleanup(t, p)
	reopened, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	run, err = reopened.GetRun(result.RunID)
	if err != nil || run.Status != types.RunFailed || run.Error == nil {
		t.Fatalf("recovered panic outcome: %+v,%v", run, err)
	}
	publication, err := reopened.GetCommitStatusPublication(context.Background(), result.RunID, db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo"})
	if err != nil || publication == nil || publication.LastState != "failure" || publication.StrictCertificate != nil {
		t.Fatalf("recovered panic publication: %+v,%v", publication, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(states) != "[success failure]" {
		t.Fatalf("panic HTTP outcome = %v", states)
	}
}

func TestRecoverOnStartup_ReplacesPublishedSuccessWithFailure(t *testing.T) {
	for _, scenario := range []string{"active", "terminal failed", "terminal missing clone", "terminal cancelled", "CI interrupted", "terminal uncertified", "terminal certified", "plugin"} {
		for _, profileOnly := range []bool{false, true} {
			if scenario == "plugin" && profileOnly {
				continue
			}
			t.Run(fmt.Sprintf("%s_profile_only_%v", scenario, profileOnly), func(t *testing.T) {
				p := paths.WithRoot(t.TempDir())
				if err := p.EnsureDirs(); err != nil {
					t.Fatal(err)
				}
				d, err := db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				repo, err := d.InsertRepoWithID("status-recovery", t.TempDir(), "https://github.com/test/repo", "main")
				if err != nil {
					t.Fatal(err)
				}
				run, err := d.InsertRun(repo.ID, "feature", "mutable-head", "base")
				if err != nil {
					t.Fatal(err)
				}
				if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				if err := d.PutCommitStatusPublication(run.ID, db.CommitStatusPublication{SHA: "pushed-head", Host: "github.com", Repo: "test/repo", Validated: true}); err != nil {
					t.Fatal(err)
				}
				binDir := t.TempDir()
				cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, "gh"), "../pipeline/fakecli")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("build fake gh: %v: %s", err, out)
				}
				t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
				t.Setenv("FAKE_CLI_MODE", "gh")
				var mu sync.Mutex
				var states []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var payload struct{ State, Context string }
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					if r.Method != "POST" || r.URL.Path != "/repos/test/repo/statuses/pushed-head" || payload.Context != "no-mistakes" {
						t.Errorf("request = %s %s %+v", r.Method, r.URL.Path, payload)
					}
					mu.Lock()
					states = append(states, payload.State)
					mu.Unlock()
					w.WriteHeader(http.StatusCreated)
				}))
				defer server.Close()
				t.Setenv("FAKE_CLI_STATUS_API_URL", server.URL)
				var forge *forgecontext.Context
				if profileOnly {
					profileDir := t.TempDir()
					if err := os.WriteFile(filepath.Join(profileDir, "hosts.yml"), []byte("github.com:\n  user: profile-user\n  oauth_token: synthetic-profile-token\n"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(p.ConfigFile(), []byte(fmt.Sprintf("forge_profiles:\n  github.com:\n    gh_config_dir: %q\n    expected_login: profile-user\n", profileDir)), 0600); err != nil {
						t.Fatal(err)
					}
					t.Setenv("GH_CONFIG_DIR", t.TempDir())
					for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST", "GH_REPO"} {
						t.Setenv(key, "synthetic-ambient-value")
					}
					t.Setenv("FAKE_CLI_STATUS_REQUIRED_GH_CONFIG_DIR", profileDir)
					global, err := config.LoadGlobal(p.ConfigFile())
					if err != nil {
						t.Fatal(err)
					}
					forge, err = forgecontext.Resolve(context.Background(), global.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
					if err != nil {
						t.Fatal(err)
					}
				}
				(&steps.PushStep{}).ReportPublishedHeadStatus(&pipeline.StepContext{Ctx: context.Background(), DB: d, Run: run, Repo: repo, ForgeContext: forge}, "success", "")
				publication, err := d.GetCommitStatusPublication(context.Background(), run.ID, db.CommitStatusPublication{SHA: "pushed-head", Host: "github.com", Repo: "test/repo"})
				if err != nil || publication.LastState != "success" {
					t.Fatalf("initial publication = %+v, %v", publication, err)
				}
				wantStates := []string{"success", "failure"}
				wantStatus := types.RunFailed
				switch scenario {
				case "terminal failed", "terminal missing clone":
					if scenario == "terminal missing clone" {
						if err := os.Remove(repo.WorkingPath); err != nil {
							t.Fatal(err)
						}
					}
					if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
						t.Fatal(err)
					}
				case "terminal cancelled", "CI interrupted":
					wantStatus = types.RunCancelled
					if scenario == "CI interrupted" {
						wantStatus = types.RunCIMonitorInterrupted
						wantStates = []string{"success"}
					}
					if err := d.UpdateRunStatus(run.ID, wantStatus); err != nil {
						t.Fatal(err)
					}
				case "terminal uncertified", "terminal certified":
					wantStatus = types.RunCompleted
					publication.Validated = scenario == "terminal certified"
					publication.LastState = "pending"
					if err := d.PutCommitStatusPublication(run.ID, *publication); err != nil {
						t.Fatal(err)
					}
					if err := d.UpdateRunStatus(run.ID, wantStatus); err != nil {
						t.Fatal(err)
					}
					if publication.Validated {
						wantStates = []string{"success", "success"}
					}
				case "plugin":
					if err := os.WriteFile(p.ConfigFile(), []byte("provider_plugins:\n  custom:\n    command: unavailable-plugin\n    hosts: [github.com]\n"), 0600); err != nil {
						t.Fatal(err)
					}
					wantStates = []string{"success"}
					ghLog := filepath.Join(t.TempDir(), "gh.log")
					t.Setenv("FAKE_CLI_LOG", ghLog)
					defer func() {
						if out, err := os.ReadFile(ghLog); err == nil {
							t.Errorf("plugin-owned recovery invoked gh: %s", out)
						} else if !os.IsNotExist(err) {
							t.Error(err)
						}
					}()
				}
				mgr := NewRunManager(d, p, nil)
				recoveredRuns := recoverOnStartup(d, p, mgr, worktrees.New(p, nil))
				reportRecoveredCommitStatuses(context.Background(), d, p, mgr, recoveredRuns, recoveryCommitStatusTimeout)
				recovered, err := d.GetRun(run.ID)
				if err != nil || recovered.Status != wantStatus {
					t.Fatalf("recovered run = %+v, %v", recovered, err)
				}
				mu.Lock()
				defer mu.Unlock()
				if fmt.Sprint(states) != fmt.Sprint(wantStates) {
					t.Fatalf("states = %v, want %v", states, wantStates)
				}
			})
		}
	}
}

func TestStatusRecoveryBounds(t *testing.T) {
	for _, scenario := range []string{"deadline", "ipc"} {
		t.Run(scenario, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			repo, err := d.InsertRepoWithID("slow-status", t.TempDir(), "https://github.com/test/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			var runs []*db.Run
			paths := make(map[string]bool)
			for i := 0; i < 10; i++ {
				run, err := d.InsertRun(repo.ID, fmt.Sprintf("feature-%d", i), fmt.Sprintf("pushed-head-%d", i), "base")
				if err != nil {
					t.Fatal(err)
				}
				if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				if err := d.PutCommitStatusPublication(run.ID, db.CommitStatusPublication{SHA: run.HeadSHA, Host: "github.com", Repo: "test/repo", Validated: true, LastState: "success"}); err != nil {
					t.Fatal(err)
				}
				runs = append(runs, run)
				paths["/repos/test/repo/statuses/"+run.HeadSHA] = true
			}
			binDir := t.TempDir()
			cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, "gh"), "../pipeline/fakecli")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build fake gh: %v: %s", err, out)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("FAKE_CLI_MODE", "gh")
			requested := make(chan struct{}, 10)
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct{ State, Context string }
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if r.Method != "POST" || !paths[r.URL.Path] || payload.State != "failure" || payload.Context != "no-mistakes" {
					t.Errorf("request = %s %s %+v", r.Method, r.URL.Path, payload)
				}
				requested <- struct{}{}
				<-release
			}))
			defer server.Close()
			defer close(release)
			t.Setenv("FAKE_CLI_STATUS_API_URL", server.URL)
			if scenario == "deadline" {
				mgr := NewRunManager(d, p, nil)
				for _, run := range runs {
					if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
						t.Fatal(err)
					}
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					reportRecoveredCommitStatuses(context.Background(), d, p, mgr, runs, 500*time.Millisecond)
				}()
				select {
				case <-requested:
				case <-time.After(3 * time.Second):
					t.Fatal("recovery never reached status endpoint")
				}
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("reporter exceeded its overall deadline")
				}
				if len(requested) != 0 {
					t.Fatal("reporter continued posting after its overall deadline")
				}
			} else {
				exited := make(chan error, 1)
				go func() { exited <- RunWithResources(p, d) }()
				t.Cleanup(func() {
					if client, err := ipc.Dial(p.Socket()); err == nil {
						_ = client.Call(ipc.MethodShutdown, testShutdownParams(t, p), nil)
						client.Close()
					}
				})
				waitForDaemonReadyOrExit(t, p, exited)
				select {
				case <-requested:
				case <-time.After(3 * time.Second):
					t.Fatal("recovery never reached status endpoint")
				}
				client, err := ipc.Dial(p.Socket())
				if err != nil {
					t.Fatal(err)
				}
				var health ipc.HealthResult
				if err := client.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, time.Second); err != nil {
					t.Fatal(err)
				}
				if err := client.Call(ipc.MethodShutdown, testShutdownParams(t, p), nil); err != nil {
					t.Fatal(err)
				}
				client.Close()
				select {
				case err := <-exited:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("shutdown did not cancel and join recovery reporter")
				}
			}
			for _, run := range runs {
				publication, err := d.GetCommitStatusPublication(context.Background(), run.ID, db.CommitStatusPublication{SHA: run.HeadSHA, Host: "github.com", Repo: "test/repo"})
				if err != nil || publication == nil || publication.LastState != "success" {
					t.Fatalf("failed posting must preserve the last posted state: %+v, %v", publication, err)
				}
			}
		})
	}
}

func TestRecoverCommitStatusPushAttempt(t *testing.T) {
	for _, scenario := range []string{"pushed", "not pushed", "already upstream without attempt"} {
		t.Run(scenario, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			gitCmd(t, dir, "init")
			gitCmd(t, dir, "config", "user.name", "Test")
			gitCmd(t, dir, "config", "user.email", "test@example.com")
			if err := os.WriteFile(filepath.Join(dir, "file"), []byte("base"), 0600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "file")
			gitCmd(t, dir, "commit", "-m", "base")
			base := gitOutput(t, dir, "rev-parse", "HEAD")
			remote := t.TempDir()
			gitCmd(t, remote, "init", "--bare")
			target := "https://github.com/test/repo"
			gitCmd(t, dir, "config", "url."+remote+".insteadOf", target)
			gitCmd(t, dir, "push", target, "HEAD:refs/heads/feature")
			if err := os.WriteFile(filepath.Join(dir, "file"), []byte("pushed"), 0600); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "commit", "-am", "pushed")
			head := gitOutput(t, dir, "rev-parse", "HEAD")
			repo, err := d.InsertRepoWithID("attempt", dir, target, "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRun(repo.ID, "feature", base, base)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "already upstream without attempt" {
				if err := d.PutCommitStatusAttempt(run.ID, db.CommitStatusAttempt{CommitStatusPublication: db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true}, Target: target, Ref: "refs/heads/feature"}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "not pushed" {
				gitCmd(t, dir, "push", target, "HEAD:refs/heads/feature")
			}
			// Reopen the database before any publication is confirmed or reported.
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			d, err = db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if _, err := d.RecoverStaleRunsExcept("crashed after push", nil); err != nil {
				t.Fatal(err)
			}
			binDir := t.TempDir()
			if out, err := exec.Command("go", "build", "-o", filepath.Join(binDir, "gh"), "../pipeline/fakecli").CombinedOutput(); err != nil {
				t.Fatalf("build fake gh: %v: %s", err, out)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("FAKE_CLI_MODE", "gh")
			var mu sync.Mutex
			var states []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct{ State, Context string }
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if r.URL.Path != "/repos/test/repo/statuses/"+head || payload.Context != "no-mistakes" {
					t.Errorf("wrong publication: %s %+v", r.URL.Path, payload)
				}
				mu.Lock()
				states = append(states, payload.State)
				mu.Unlock()
				w.WriteHeader(http.StatusCreated)
			}))
			defer server.Close()
			t.Setenv("FAKE_CLI_STATUS_API_URL", server.URL)
			reportRecoveredCommitStatuses(context.Background(), d, p, NewRunManager(d, p, nil), nil, 3*time.Second)
			mu.Lock()
			defer mu.Unlock()
			if scenario == "pushed" {
				if fmt.Sprint(states) != "[failure]" {
					t.Fatalf("states = %v", states)
				}
				attempt, err := d.GetCommitStatusAttempt(run.ID)
				if err != nil || attempt != nil {
					t.Fatalf("confirmed attempt = %+v, %v", attempt, err)
				}
				publication, err := d.GetCommitStatusPublication(context.Background(), run.ID, db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo"})
				if err != nil || publication == nil || publication.SHA != head || publication.LastState != "failure" {
					t.Fatalf("publication = %+v, %v", publication, err)
				}
			} else if len(states) != 0 {
				t.Fatalf("posted an unowned SHA: %v", states)
			}
		})
	}
}
