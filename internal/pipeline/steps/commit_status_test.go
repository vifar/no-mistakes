package steps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type statusRequest struct{ Path, State, Context, Description, TargetURL string }

func TestCommitStatusTerminalReportsEveryOwnedSHA(t *testing.T) {
	for _, retired := range []bool{false, true} {
		t.Run(fmt.Sprint(retired), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			sc.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
			for _, sha := range []string{head, base} {
				if err := sc.DB.PutCommitStatusPublication(sc.Run.ID, db.CommitStatusPublication{SHA: sha, Host: "github.com", Repo: "test/repo", Validated: true, LastState: "success"}); err != nil {
					t.Fatal(err)
				}
			}
			if retired {
				newer, err := sc.DB.InsertRun(sc.Repo.ID, "other-feature", head, base)
				if err != nil {
					t.Fatal(err)
				}
				if err := sc.DB.PutCommitStatusPublication(newer.ID, db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true, LastState: "success"}); err != nil {
					t.Fatal(err)
				}
				if err := sc.DB.UpdateRunStatus(newer.ID, types.RunCompleted); err != nil {
					t.Fatal(err)
				}
			}
			sc.Run.Status = types.RunFailed
			if err := sc.DB.UpdateRunStatus(sc.Run.ID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var requests []statusRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct{ State, Context string }
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if r.Method != "POST" || payload.Context != "no-mistakes" {
					t.Errorf("unexpected status request: %s %+v", r.Method, payload)
				}
				mu.Lock()
				requests = append(requests, statusRequest{Path: r.URL.Path, State: payload.State})
				mu.Unlock()
				w.WriteHeader(http.StatusCreated)
			}))
			defer server.Close()
			env, _ := fakeGH(t, "")
			sc.Env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL)
			(&PushStep{}).ReportPublishedHeadStatus(sc, "failure", "")
			ids, err := sc.DB.CommitStatusRecoveryRunIDs(sc.Ctx)
			if err != nil || len(ids) != 0 {
				t.Fatalf("unfinished terminal publications = %v, %v", ids, err)
			}
			want := []statusRequest{}
			if !retired {
				want = append(want, statusRequest{Path: "/repos/test/repo/statuses/" + head, State: "failure"})
			}
			want = append(want, statusRequest{Path: "/repos/test/repo/statuses/" + base, State: "failure"})
			mu.Lock()
			defer mu.Unlock()
			if fmt.Sprint(requests) != fmt.Sprint(want) {
				t.Fatalf("terminal requests = %+v, want %+v", requests, want)
			}
		})
	}
}

func TestCommitStatusRemoteConfirmationHasOwnDeadline(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	publication := db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true}
	if err := sc.DB.PutCommitStatusAttempt(sc.Run.ID, db.CommitStatusAttempt{CommitStatusPublication: publication, Target: "https://github.com/test/repo", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	marker := filepath.Join(t.TempDir(), "remote-started")
	sc.Env = fakeCLIEnv(binDir, map[string]string{"FAKE_CLI_MODE": "git-status-confirmation-stalled", "FAKE_CLI_STARTED_FILE": marker, "FAKE_CLI_INHERITED_PIPE_CHILD": "1"})
	sc.Ctx = context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p, err := reconcileCommitStatusAttempt(sc)
		if err == nil || p != nil {
			t.Errorf("stalled confirmation = %+v, %v; want unpublished with error", p, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(commitStatusOperationTimeout + 3*time.Second):
		t.Fatal("remote confirmation was not bounded independently of the caller")
	}
	if out, err := os.ReadFile(marker); err != nil || string(out) != "ls-remote" {
		t.Fatalf("confirmation command marker = %q, %v", out, err)
	}
	if err := acquireCommitStatusLock(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-commitStatusLock
}

func TestPushCommitStatusRecordingErrorsOnlyWarn(t *testing.T) {
	for _, table := range []string{"commit_status_attempts", "commit_status_publications"} {
		t.Run(table, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			remote := t.TempDir()
			gitCmd(t, remote, "init", "--bare", ".")
			gitCmd(t, dir, "config", "url."+remote+".insteadOf", "https://github.com/test/repo")
			sc := newTestContext(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			dbPath := filepath.Join(t.TempDir(), "status.db")
			database, err := db.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			sc.DB = database
			sc.Repo, err = database.InsertRepo(dir, "https://github.com/test/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			sc.Run, err = database.InsertRun(sc.Repo.ID, "refs/heads/feature", head, base)
			if err != nil {
				t.Fatal(err)
			}
			sc.Repo.URLsVerified = true
			sc.Env, _ = fakeGH(t, "")
			sc.Env = append(sc.Env, "FAKE_CLI_PR_FACTS_LIST_JSON=[[]]")
			sc.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
			testgit.BindRunComparison(t, database, sc.Run, dir, "main", "", "test/repo", "feature")
			if err := database.UpdateRunReviewApprovedHeadSHA(sc.Run.ID, head); err != nil {
				t.Fatal(err)
			}
			faults, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer faults.Close()
			if _, err := faults.Exec("CREATE TRIGGER reject_status BEFORE INSERT ON " + table + " BEGIN SELECT RAISE(FAIL, 'status recording unavailable'); END"); err != nil {
				t.Fatal(err)
			}
			var warnings []string
			sc.Log = func(message string) { warnings = append(warnings, message) }
			if err := publishRunHead(sc, head, "", nil); err != nil {
				t.Fatalf("status recording error failed Push: %v", err)
			}
			if got := gitCmd(t, remote, "rev-parse", "refs/heads/feature"); got != head {
				t.Fatalf("remote head = %s, want %s", got, head)
			}
			run, err := database.GetRun(sc.Run.ID)
			if err != nil || run.LastPushedSHA == nil || *run.LastPushedSHA != head {
				t.Fatalf("successful push not settled: %+v, %v", run, err)
			}
			if !strings.Contains(strings.Join(warnings, "\n"), "warning: no-mistakes commit status") || !strings.Contains(strings.Join(warnings, "\n"), "status recording unavailable") {
				t.Fatalf("status recording failure not logged: %v", warnings)
			}
		})
	}
}

func TestCommitStatusLateAttemptAfterRepairStaysRetired(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sc.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
	newer, err := sc.DB.InsertRun(sc.Repo.ID, "other-feature", head, base)
	if err != nil {
		t.Fatal(err)
	}
	original := db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true, LastState: "success"}
	repair := original
	repair.SHA = base
	for _, p := range []db.CommitStatusPublication{original, repair} {
		if err := sc.DB.PutCommitStatusPublication(newer.ID, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := sc.DB.PutCommitStatusAttempt(sc.Run.ID, db.CommitStatusAttempt{CommitStatusPublication: original, Target: "https://github.com/test/repo", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	attempt, err := sc.DB.GetCommitStatusAttempt(sc.Run.ID)
	if err != nil || attempt != nil {
		t.Fatalf("late attempt = %+v, %v; want retired", attempt, err)
	}
	if confirmed, err := sc.DB.ConfirmCommitStatusAttempt(sc.Run.ID, original); err != nil || confirmed {
		t.Fatalf("late confirmation = %v, %v; want refused", confirmed, err)
	}
	for _, p := range []db.CommitStatusPublication{original, repair} {
		stored, err := sc.DB.GetCommitStatusPublication(sc.Ctx, newer.ID, p)
		if err != nil || stored == nil || stored.LastState != "success" {
			t.Fatalf("newer ownership for %s = %+v, %v", p.SHA, stored, err)
		}
	}
	if err := sc.DB.UpdateRunStatus(sc.Run.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	if err := sc.DB.UpdateRunStatus(newer.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	ids, err := sc.DB.CommitStatusRecoveryRunIDs(sc.Ctx)
	if err != nil || len(ids) != 0 {
		t.Fatalf("recovery selected obsolete ownership: %v, %v", ids, err)
	}
	env, logFile := fakeGH(t, "")
	sc.Env = env
	(&PushStep{}).ReportPublishedHeadStatus(sc, "failure", "")
	if out, err := os.ReadFile(logFile); err == nil && len(out) != 0 {
		t.Fatalf("late attempt invoked gh: %s", out)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestCommitStatusLockWaitHonorsContext(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sc.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
	publication := db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true}
	if err := sc.DB.PutCommitStatusPublication(sc.Run.ID, publication); err != nil {
		t.Fatal(err)
	}
	if err := sc.DB.PutCommitStatusAttempt(sc.Run.ID, db.CommitStatusAttempt{CommitStatusPublication: publication, Target: "https://github.com/test/repo", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"confirmation", "verified confirmation", "reporting"} {
		for _, ending := range []string{"cancel", "deadline"} {
			t.Run(operation+"/"+ending, func(t *testing.T) {
				if err := acquireCommitStatusLock(context.Background()); err != nil {
					t.Fatal(err)
				}
				defer func() { <-commitStatusLock }()
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()
				bounded := *sc
				bounded.Ctx = ctx
				done, started := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(done)
					close(started)
					if operation != "reporting" {
						var err error
						if operation == "confirmation" {
							_, err = reconcileCommitStatusAttempt(&bounded)
						} else {
							_, err = confirmVerifiedCommitStatus(&bounded, head, "https://github.com/test/repo", "refs/heads/feature")
						}
						if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
							t.Errorf("confirmation error = %v", err)
						}
					} else {
						commitStatusReporter(&bounded, &publication)(ctx, "failure", "", bounded.Log)
					}
				}()
				<-started
				if ending == "cancel" {
					cancel()
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("cancelled reporter remained blocked on shared lock")
				}
			})
		}
	}
}

func TestCommitStatusCIReadinessSurvivesPostFailure(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sc.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
	publication := db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", CIEligible: true, LastState: "pending"}
	if err := sc.DB.PutCommitStatusPublication(sc.Run.ID, publication); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var states []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ State, Context string }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if r.Method != "POST" || r.URL.Path != "/repos/test/repo/statuses/"+head || payload.Context != "no-mistakes" {
			t.Errorf("unexpected status request: %s %s %+v", r.Method, r.URL.Path, payload)
		}
		mu.Lock()
		defer mu.Unlock()
		states = append(states, payload.State)
		if len(states) == 1 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	env, _ := fakeGH(t, "")
	sc.Env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL)
	report := commitStatusReporter(sc, &publication)
	report(sc.Ctx, "success", head, sc.Log)
	sc.Run.Status = types.RunCompleted
	(&PushStep{}).ReportPublishedHeadStatus(sc, "success", "")
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(states) != "[success success]" {
		t.Fatalf("states = %v, want success retried after exact-head readiness", states)
	}
}

func TestCommitStatusSerializesRecoveryAndNewPublication(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sc.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
	publication := db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true, LastState: "success"}
	if err := sc.DB.PutCommitStatusPublication(sc.Run.ID, publication); err != nil {
		t.Fatal(err)
	}
	newer, err := sc.DB.InsertRun(sc.Repo.ID, "other-feature", head, base)
	if err != nil {
		t.Fatal(err)
	}
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare", ".")
	target := "https://github.com/test/repo"
	gitCmd(t, dir, "config", "url."+remote+".insteadOf", target)
	gitCmd(t, dir, "push", target, head+":refs/heads/other-feature")
	attempt := db.CommitStatusAttempt{CommitStatusPublication: publication, Target: target, Ref: "refs/heads/other-feature"}
	attempt.LastState = ""
	if err := sc.DB.PutCommitStatusAttempt(newer.ID, attempt); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var mu sync.Mutex
	var states []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ State, Context string }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if r.Method != "POST" || r.URL.Path != "/repos/test/repo/statuses/"+head || payload.Context != "no-mistakes" {
			t.Errorf("unexpected status request: %s %s %+v", r.Method, r.URL.Path, payload)
		}
		if payload.State == "failure" {
			close(entered)
			<-release
		}
		mu.Lock()
		states = append(states, payload.State)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	defer unblock()
	env, _ := fakeGH(t, "")
	sc.Env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL)
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		(&PushStep{}).ReportPublishedHeadStatus(sc, "failure", "")
	}()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("recovery POST did not reach the endpoint")
	}
	select {
	case commitStatusLock <- struct{}{}:
		<-commitStatusLock
		t.Fatal("recovery POST did not hold the shared status lock")
	default:
	}
	newContext := *sc
	newContext.Run = newer
	newContext.Shared = &pipeline.RunShared{}
	newDone, started := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(newDone)
		close(started)
		beginPublishedCommitStatus(&newContext)("success")
	}()
	<-started
	unblock()
	for _, done := range []chan struct{}{oldDone, newDone} {
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("status reporter did not finish")
		}
	}
	(&PushStep{}).ReportPublishedHeadStatus(sc, "failure", "")
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(states) != "[failure pending success]" {
		t.Fatalf("completed status POSTs = %v, want older failure before newer success", states)
	}
}

func TestCommitStatusNewerPublicationOwnsStatus(t *testing.T) {
	for _, path := range []string{"attempt", "persisted", "cached"} {
		for _, scope := range []string{"same", "repair head", "other SHA", "other repo", "other host"} {
			t.Run(path+"/"+scope, func(t *testing.T) {
				dir, base, head := setupGitRepo(t)
				remote := t.TempDir()
				gitCmd(t, remote, "init", "--bare", ".")
				target := "https://github.com/test/repo"
				gitCmd(t, dir, "config", "url."+remote+".insteadOf", target)
				gitCmd(t, dir, "push", target, base+":refs/heads/feature")
				sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
				sc.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
				publication := db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true}
				var mu sync.Mutex
				var requests []statusRequest
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var payload struct{ State, Context string }
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					if r.Method != "POST" || payload.Context != "no-mistakes" {
						t.Errorf("unexpected status request: %s %+v", r.Method, payload)
					}
					mu.Lock()
					requests = append(requests, statusRequest{Path: r.URL.Path, State: payload.State})
					mu.Unlock()
					w.WriteHeader(http.StatusCreated)
				}))
				defer server.Close()
				env, _ := fakeGH(t, "")
				sc.Env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL)
				var cachedReport func(context.Context, string, string, func(string))
				if path == "attempt" {
					if err := sc.DB.PutCommitStatusAttempt(sc.Run.ID, db.CommitStatusAttempt{CommitStatusPublication: publication, Target: target, Ref: "refs/heads/feature"}); err != nil {
						t.Fatal(err)
					}
					(&PushStep{}).ReportPublishedHeadStatus(sc, "failure", "")
					mu.Lock()
					posted := len(requests)
					mu.Unlock()
					if posted != 0 {
						t.Fatal("failed push posted a status before remote publication")
					}
				} else {
					if err := sc.DB.PutCommitStatusPublication(sc.Run.ID, publication); err != nil {
						t.Fatal(err)
					}
					if path == "cached" {
						cachedReport = commitStatusReporter(sc, &publication)
					}
				}
				newer, err := sc.DB.InsertRun(sc.Repo.ID, "other-feature", head, base)
				if err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "push", target, head+":refs/heads/feature")
				newPublication := publication
				switch scope {
				case "other SHA":
					newPublication.SHA = base
				case "other repo":
					newPublication.Repo = "fork/repo"
				case "other host":
					newPublication.Host = "enterprise.example.com"
				}
				if err := sc.DB.PutCommitStatusPublication(newer.ID, newPublication); err != nil {
					t.Fatal(err)
				}
				newContext := *sc
				newContext.Run = newer
				newContext.Shared = &pipeline.RunShared{}
				newReport := commitStatusReporter(&newContext, &newPublication)
				newReport(sc.Ctx, "pending", "", sc.Log)
				newReport(sc.Ctx, "success", "", sc.Log)
				repairSHA := ""
				if scope == "repair head" {
					gitCmd(t, dir, "commit", "--allow-empty", "-m", "repair")
					repairSHA = gitCmd(t, dir, "rev-parse", "HEAD")
					gitCmd(t, dir, "push", target, repairSHA+":refs/heads/other-feature")
					repair := publication
					repair.SHA = repairSHA
					if err := sc.DB.PutCommitStatusAttempt(newer.ID, db.CommitStatusAttempt{CommitStatusPublication: repair, Target: target, Ref: "refs/heads/other-feature"}); err != nil {
						t.Fatal(err)
					}
					beginPublishedCommitStatus(&newContext)("success")
				}
				if err := sc.DB.UpdateRunStatus(sc.Run.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
				sc.Run.Status = types.RunFailed
				if err := sc.DB.UpdateRunStatus(newer.ID, types.RunCompleted); err != nil {
					t.Fatal(err)
				}
				if scope == "same" || scope == "repair head" {
					if path == "attempt" {
						if err := sc.DB.PutCommitStatusAttempt(sc.Run.ID, db.CommitStatusAttempt{CommitStatusPublication: publication, Target: target, Ref: "refs/heads/feature"}); err != nil {
							t.Fatal(err)
						}
					} else if err := sc.DB.PutCommitStatusPublication(sc.Run.ID, publication); err != nil {
						t.Fatal(err)
					}
				}
				ids, err := sc.DB.CommitStatusRecoveryRunIDs(sc.Ctx)
				if err != nil {
					t.Fatal(err)
				}
				if scope == "same" || scope == "repair head" {
					if len(ids) != 0 {
						t.Fatalf("recovery selected retired ownership: %v", ids)
					}
				} else if len(ids) != 1 || ids[0] != sc.Run.ID {
					t.Fatalf("recovery selection = %v; want only older run", ids)
				}
				(&PushStep{}).ReportPublishedHeadStatus(sc, "failure", "")
				if scope == "same" || scope == "repair head" {
					beginPublishedCommitStatus(sc)("success")
					if cachedReport != nil {
						cachedReport(sc.Ctx, "success", head, sc.Log)
						cachedReport(sc.Ctx, "failure", "", sc.Log)
					}
				}
				if path == "attempt" && (scope == "same" || scope == "repair head") {
					p, err := sc.DB.GetCommitStatusPublication(sc.Ctx, sc.Run.ID, publication)
					if err != nil || p != nil {
						t.Fatalf("old attempt credited with newer push: %+v, %v", p, err)
					}
				}
				mu.Lock()
				defer mu.Unlock()
				want := []statusRequest{{Path: "/repos/" + newPublication.Repo + "/statuses/" + newPublication.SHA, State: "pending"}, {Path: "/repos/" + newPublication.Repo + "/statuses/" + newPublication.SHA, State: "success"}}
				if scope == "repair head" {
					want = append(want, statusRequest{Path: "/repos/test/repo/statuses/" + repairSHA, State: "pending"}, statusRequest{Path: "/repos/test/repo/statuses/" + repairSHA, State: "success"})
				}
				if scope != "same" && scope != "repair head" {
					if path == "attempt" {
						want = append(want, statusRequest{Path: "/repos/test/repo/statuses/" + head, State: "pending"})
					}
					want = append(want, statusRequest{Path: "/repos/test/repo/statuses/" + head, State: "failure"})
				}
				if fmt.Sprint(requests) != fmt.Sprint(want) {
					t.Fatalf("status requests = %+v, want %+v", requests, want)
				}
			})
		}
	}
}

func TestPipeline_GitHubCommitStatuses(t *testing.T) {
	for _, scenario := range []string{"pass", "push reply lost", "fail after push", "cancel after push", "no push", "forgejo", "bitbucket", "repair head", "confirmation timeout then repair", "posting fails", "already upstream", "attempt already upstream", "fork", "formatter head", "recovered failure", "CI repair readiness", "Test no-go exception", "Test inconclusive exception", "skipped Test", "late Document", "late Lint", "stale tested head"} {
		t.Run(scenario, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			remote := t.TempDir()
			gitCmd(t, remote, "init", "--bare", ".")
			gitCmd(t, dir, "config", "url."+remote+".insteadOf", "https://github.com/test/repo")
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			gitCmd(t, dir, "remote", "set-url", "origin", "https://github.com/test/repo")
			sctx.Repo.URLsVerified = true
			if scenario == "already upstream" || scenario == "attempt already upstream" {
				gitCmd(t, dir, "push", "origin", "HEAD:refs/heads/feature")
			}
			if scenario == "fork" {
				sctx.Repo.ForkURL = "https://github.com/fork/repo"
				gitCmd(t, dir, "config", "url."+remote+".insteadOf", sctx.Repo.ForkURL)
			}
			if scenario == "formatter head" {
				sctx.Config.Commands.Format = "printf formatted > feature.txt"
			}

			var mu sync.Mutex
			var requests []statusRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					State       string `json:"state"`
					Context     string `json:"context"`
					Description string `json:"description"`
					TargetURL   string `json:"target_url"`
				}
				if r.Method != "POST" {
					t.Errorf("method = %s, want POST", r.Method)
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				mu.Lock()
				requests = append(requests, statusRequest{r.URL.Path, payload.State, payload.Context, payload.Description, payload.TargetURL})
				mu.Unlock()
				if scenario == "posting fails" {
					http.Error(w, "status service unavailable", 503)
					return
				}
				w.WriteHeader(201)
				fmt.Fprint(w, `{}`)
			}))
			defer server.Close()
			env, ghLog := fakeGH(t, "")
			env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL, "FAKE_CLI_PR_FACTS_LIST_JSON=[[]]")
			for key, value := range environmentEntries(t, env) {
				t.Setenv(key, value)
			}
			if scenario == "push reply lost" || scenario == "confirmation timeout then repair" {
				gh, err := exec.LookPath("gh")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Link(gh, filepath.Join(filepath.Dir(gh), "git")); err != nil {
					t.Fatal(err)
				}
				if scenario == "push reply lost" {
					t.Setenv("FAKE_CLI_MODE", "gh-git-push-reply-lost")
				} else {
					t.Setenv("FAKE_CLI_MODE", "gh-git-status-confirmation-timeout")
					t.Setenv("FAKE_CLI_STATUS_CONFIRMATION_STATE", filepath.Join(t.TempDir(), "confirmation-state"))
				}
				realGit, err := testgit.RealGit()
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("FAKE_CLI_REAL_GIT", realGit)
			}
			provider := scm.ProviderGitHub
			if scenario == "forgejo" {
				provider = scm.ProviderForgejo
			}
			if scenario == "bitbucket" {
				provider = scm.ProviderBitbucket
			}
			reviewHeads := []string{}
			testHeads := []string{}
			lateHead := ""
			var steps []pipeline.Step
			for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
				name := name
				steps = append(steps, &transitionStep{name: name, execute: func(sc *pipeline.StepContext) (*pipeline.StepOutcome, error) {
					o := &pipeline.StepOutcome{}
					if name == types.StepTest {
						testHeads = append(testHeads, sc.Run.HeadSHA)
						o.Findings = fmt.Sprintf(`{"findings":[],"tested_head_sha":%q}`, sc.Run.HeadSHA)
						if scenario == "stale tested head" {
							o.Findings = fmt.Sprintf(`{"findings":[],"tested_head_sha":%q}`, base)
						}
						if scenario == "skipped Test" {
							o.Skipped = true
						}
						if strings.HasPrefix(scenario, "Test ") {
							verdict := strings.TrimSuffix(strings.TrimPrefix(scenario, "Test "), " exception")
							o.Findings = fmt.Sprintf(`{"findings":[],"verdict":%q}`, verdict)
							o.NeedsApproval = true
						}
					}
					if name == types.StepReview {
						if lateHead != "" {
							mu.Lock()
							posted := len(requests)
							mu.Unlock()
							if posted != 0 {
								t.Fatal("late head posted before revalidation")
							}
						}
						reviewHeads = append(reviewHeads, sc.Run.HeadSHA)
						o.ReviewApprovedHeadSHA = sc.Run.HeadSHA
					}
					if (scenario == "late Document" && name == types.StepDocument || scenario == "late Lint" && name == types.StepLint) && lateHead == "" {
						if err := os.WriteFile(filepath.Join(dir, "late.txt"), []byte("late edit"), 0600); err != nil {
							t.Fatal(err)
						}
						gitCmd(t, dir, "add", "late.txt")
						gitCmd(t, dir, "commit", "-m", "late gate edit")
						lateHead = gitCmd(t, dir, "rev-parse", "HEAD")
						if err := recordAgentFixHead(sc, name, lateHead); err != nil {
							return nil, err
						}
					}
					return o, nil
				}})
			}
			if scenario != "no push" {
				steps = append(steps, &PushStep{})
			}
			uncertified := scenario == "skipped Test" || scenario == "stale tested head" || strings.HasPrefix(scenario, "Test ")
			ciCalls := 0
			repaired := ""
			steps = append(steps, &transitionStep{name: types.StepCI, execute: func(sc *pipeline.StepContext) (*pipeline.StepOutcome, error) {
				ciCalls++
				if ciCalls == 1 && scenario != "no push" && scenario != "forgejo" && scenario != "bitbucket" && scenario != "confirmation timeout then repair" {
					mu.Lock()
					n := len(requests)
					mu.Unlock()
					wantAtCI := 2
					if uncertified {
						wantAtCI = 1
					}
					if n != wantAtCI {
						t.Fatalf("requests at CI entry = %d, want pending then success before CI", n)
					}
				}
				if uncertified {
					sc.PublishedHeadValidated(head)
				}
				if scenario == "fail after push" {
					return nil, errors.New("CI failed")
				}
				if scenario == "cancel after push" {
					return nil, errors.New(types.RunCancelReasonAbortedByUser)
				}
				if (scenario == "repair head" || scenario == "CI repair readiness" || scenario == "confirmation timeout then repair") && ciCalls == 1 {
					if err := os.WriteFile(filepath.Join(dir, "repair.txt"), []byte("repair"), 0600); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, dir, "add", "repair.txt")
					gitCmd(t, dir, "commit", "-m", "repair")
					repaired = gitCmd(t, dir, "rev-parse", "HEAD")
					sc.Run.HeadSHA = repaired
					if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, repaired); err != nil {
						t.Fatal(err)
					}
					if scenario == "CI repair readiness" {
						if err := publishRunHead(sc, repaired, "", nil); err != nil {
							return nil, err
						}
						mu.Lock()
						before := len(requests)
						mu.Unlock()
						if before != 3 {
							t.Fatalf("requests before readiness = %d, want initial pending/success and repair pending", before)
						}
						sc.PublishedHeadValidated(head) // stale readiness must not certify the repair
						mu.Lock()
						afterStale := len(requests)
						mu.Unlock()
						if afterStale != before {
							t.Fatal("stale head certified repair")
						}
						sc.PublishedHeadValidated(repaired)
						return &pipeline.StepOutcome{}, nil
					}
					return &pipeline.StepOutcome{RestartFrom: types.StepReview}, nil
				}
				return &pipeline.StepOutcome{}, nil
			}})
			if scenario == "attempt already upstream" {
				if err := sctx.DB.PutCommitStatusAttempt(sctx.Run.ID, db.CommitStatusAttempt{CommitStatusPublication: db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true}, Target: "https://github.com/test/repo", Ref: "refs/heads/feature"}); err != nil {
					t.Fatal(err)
				}
			}
			appPaths := paths.WithRoot(t.TempDir())
			if err := appPaths.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			gateReady := make(chan struct{}, 1)
			exec := pipeline.NewExecutor(sctx.DB, appPaths, sctx.Config, sctx.Agent, steps, func(event ipc.Event) {
				if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepTest && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
					gateReady <- struct{}{}
				}
			})
			exec.SetForgeContext(&forgecontext.Context{Provider: provider, Host: "github.com"})
			// The synthetic Review/CI steps above model validation and repair,
			// not forge discovery. Supply their comparison receipts from real
			// Git heads at the executor's existing comparison boundary.
			exec.SetPRContextGuard(func(sc *pipeline.StepContext, _ types.StepName) (pipeline.PRContextDecision, error) {
				candidate, err := testgit.RunComparisonCandidate(sc.Run, dir, "main", base, "", scm.RepoPath(sc.Repo.PushURL()), "feature")
				if err != nil {
					return pipeline.PRContextDecision{}, err
				}
				_, err = sc.DB.AdvanceRunPRContext(sc.Run.ID, candidate)
				return pipeline.PRContextDecision{Target: pipeline.PRTargetSelection{SourceRepo: candidate.SourceRepo, SourceBranch: candidate.SourceBranch, TargetBranch: "main"}}, err
			})
			var err error
			if strings.HasPrefix(scenario, "Test ") {
				// Gate readiness is an event contract, not a timing assertion. The
				// deadline only detects a hang, including slow git under the race suite.
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				done := make(chan error, 1)
				go func() {
					defer close(done)
					done <- exec.Execute(ctx, sctx.Run, sctx.Repo, dir)
				}()
				defer func() {
					cancel()
					for range done {
					}
				}()
				select {
				case <-gateReady:
				case err := <-done:
					t.Fatalf("executor exited before Test approval: %v", err)
				case <-ctx.Done():
					t.Fatal("Test did not park for approval")
				}
				if _, respondErr := exec.RespondWithOverrides(types.StepTest, types.ActionApprove, nil, nil, nil, nil, "accept live validation exception", gateSnapshot(exec)); respondErr != nil {
					t.Fatal(respondErr)
				}
				select {
				case err = <-done:
				case <-ctx.Done():
					t.Fatal("approved run did not complete")
				}
			} else {
				err = exec.Execute(context.Background(), sctx.Run, sctx.Repo, dir)
			}
			wantError := scenario == "push reply lost" || scenario == "fail after push" || scenario == "cancel after push"
			if (err != nil) != wantError {
				t.Fatalf("Execute = %v, wantError %v", err, wantError)
			}
			if scenario == "formatter head" {
				repaired = gitCmd(t, dir, "rev-parse", "HEAD")
			}
			if scenario == "recovered failure" {
				// A fresh executor has no in-memory reporter. Change its mutable head to
				// prove the failure update still uses the persisted publication SHA.
				sctx.Run.HeadSHA = base
				recovered := pipeline.NewExecutor(sctx.DB, appPaths, sctx.Config, sctx.Agent, []pipeline.Step{&PushStep{}, &transitionStep{name: types.StepCI, execute: func(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
					return nil, errors.New("recovered monitor failed")
				}}}, nil)
				recovered.SetSkippedSteps([]types.StepName{types.StepPush})
				recovered.SetForgeContext(&forgecontext.Context{Provider: provider, Host: "github.com"})
				if err := recovered.Execute(context.Background(), sctx.Run, sctx.Repo, dir); err == nil {
					t.Fatal("recovered failure did not fail")
				}
			}
			if scenario == "confirmation timeout then repair" {
				state, err := os.ReadFile(os.Getenv("FAKE_CLI_STATUS_CONFIRMATION_STATE"))
				if err != nil || string(state) != "timed-out" {
					t.Fatalf("confirmation timeout state = %q, %v", state, err)
				}
				for _, sha := range []string{head, repaired} {
					p, err := sctx.DB.GetCommitStatusPublication(context.Background(), sctx.Run.ID, db.CommitStatusPublication{SHA: sha, Host: "github.com", Repo: "test/repo"})
					if err != nil || p == nil || p.LastState != "success" {
						t.Fatalf("verified publication %s was lost after repair: %+v, %v", sha, p, err)
					}
				}
			}
			mu.Lock()
			got := append([]statusRequest(nil), requests...)
			mu.Unlock()
			var want []statusRequest
			if scenario != "no push" && scenario != "forgejo" && scenario != "bitbucket" {
				want = []statusRequest{{Path: "/repos/test/repo/statuses/" + head, State: "pending"}, {Path: "/repos/test/repo/statuses/" + head, State: "success"}}
				if wantError || scenario == "recovered failure" {
					want = append(want, statusRequest{Path: want[0].Path, State: "failure"})
				}
				if scenario == "repair head" || scenario == "CI repair readiness" || scenario == "confirmation timeout then repair" {
					want = append(want, statusRequest{Path: "/repos/test/repo/statuses/" + repaired, State: "pending"}, statusRequest{Path: "/repos/test/repo/statuses/" + repaired, State: "success"})
				}
			}
			if scenario == "push reply lost" {
				want = []statusRequest{{Path: "/repos/test/repo/statuses/" + head, State: "pending"}, {Path: "/repos/test/repo/statuses/" + head, State: "failure"}}
				if ciCalls != 0 {
					t.Fatal("CI ran after a lost push response")
				}
			}
			if uncertified {
				want = []statusRequest{{Path: "/repos/test/repo/statuses/" + head, State: "pending"}, {Path: "/repos/test/repo/statuses/" + head, State: "failure"}}
			}
			if scenario == "formatter head" {
				want = []statusRequest{{Path: "/repos/test/repo/statuses/" + repaired, State: "pending"}, {Path: "/repos/test/repo/statuses/" + repaired, State: "success"}}
			}
			if strings.HasPrefix(scenario, "late ") {
				if len(reviewHeads) != 2 || reviewHeads[0] != head || reviewHeads[1] != lateHead || len(testHeads) != 2 || testHeads[0] != head || testHeads[1] != lateHead {
					t.Fatalf("reviewed %v, tested %v, want original %s then late %s", reviewHeads, testHeads, head, lateHead)
				}
				want = []statusRequest{{Path: "/repos/test/repo/statuses/" + lateHead, State: "pending"}, {Path: "/repos/test/repo/statuses/" + lateHead, State: "success"}}
			}
			if scenario == "fork" {
				for i := range want {
					want[i].Path = "/repos/fork/repo/statuses/" + head
				}
			}
			// Posting failures may be retried at terminal completion. The run must
			// nevertheless complete and expose the failure in its persisted logs.
			if scenario == "posting fails" {
				if len(got) < 2 {
					t.Fatalf("requests = %#v, want attempted pending and success", got)
				}
				logs, readErr := os.ReadFile(filepath.Join(appPaths.RunLogDir(sctx.Run.ID), "push.log"))
				if readErr != nil || !strings.Contains(string(logs), "commit status") || !strings.Contains(string(logs), "warning") {
					t.Fatalf("posting failure log = %s (%v)", logs, readErr)
				}
				return
			}
			if len(got) != len(want) {
				logs, _ := os.ReadFile(filepath.Join(appPaths.RunLogDir(sctx.Run.ID), "push.log"))
				gh, _ := os.ReadFile(ghLog)
				t.Fatalf("requests = %#v, want %#v; push logs %s; gh logs %s", got, want, logs, gh)
			}
			for i := range want {
				if got[i].Path != want[i].Path || got[i].State != want[i].State || got[i].Context != "no-mistakes" || got[i].Description == "" || got[i].TargetURL != "https://github.com/"+strings.Replace(strings.TrimPrefix(got[i].Path, "/repos/"), "/statuses/", "/commit/", 1) {
					t.Errorf("request %d = %#v, want %#v with context no-mistakes and description", i, got[i], want[i])
				}
			}
		})
	}
}

func TestCommitStatusPluginOwnershipSuppressesOrdinaryPosting(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sc.Config.ProviderPlugins = config.ProviderPlugins{"custom": {Command: "unavailable-plugin", Hosts: []string{"github.com"}}}
	sc.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
	env, logFile := fakeGH(t, "")
	sc.Env = env
	publication := db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true}
	if err := sc.DB.PutCommitStatusPublication(sc.Run.ID, publication); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	sc.Log = func(message string) { warnings = append(warnings, message) }
	// Even an already-installed built-in reporter must yield to plugin ownership.
	commitStatusReporter(sc, &publication)(sc.Ctx, "failure", "", sc.Log)
	(&PushStep{}).ReportPublishedHeadStatus(sc, "failure", "")
	update := beginPublishedCommitStatus(sc)
	update("success")
	if out, err := os.ReadFile(logFile); err == nil && len(out) != 0 {
		t.Fatalf("plugin-owned ordinary posting invoked gh: %s", out)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, "\n"), "does not support commit statuses") {
		t.Fatalf("missing suppression warning: %v", warnings)
	}
}
