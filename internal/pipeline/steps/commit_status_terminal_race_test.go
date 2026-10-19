package steps

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"modernc.org/sqlite"
)

func TestCommitStatus_OrdinaryFailureSerializesPausedSuccess(t *testing.T) {
	sc, path, p := strictStatusContext(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
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
	env, _ := fakeGH(t, "")
	env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL)
	for key, value := range environmentEntries(t, env) {
		t.Setenv(key, value)
	}
	writeEntered, writeRelease := make(chan struct{}), make(chan struct{})
	var writeOnce sync.Once
	releaseWrite := func() { writeOnce.Do(func() { close(writeRelease) }) }
	defer releaseWrite()
	function := "terminal_failure_" + strings.ToLower(sc.Run.ID)
	// A terminal write can fire the trigger more than once (the target-rewind
	// cleanup trigger updates the same row), so only the first entry signals.
	var enteredOnce sync.Once
	if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		enteredOnce.Do(func() { close(writeEntered) })
		<-writeRelease
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := sc.DB.Close(); err != nil {
		t.Fatal(err)
	}
	barrierDB, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sc.DB = barrierDB
	defer barrierDB.Close()
	control, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(fmt.Sprintf(`CREATE TRIGGER observe_terminal_failure BEFORE UPDATE ON runs WHEN NEW.id=%q AND NEW.status='failed' BEGIN SELECT %s(); END`, sc.Run.ID, function)); err != nil {
		t.Fatal(err)
	}
	stepFailed := make(chan struct{})
	callbackDone := make(chan struct{})
	ci := &transitionStep{name: types.StepCI, execute: func(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		go func() { defer close(callbackDone); ctx.PublishedHeadValidated(p.SHA) }()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			return nil, errors.New("success callback never reached endpoint")
		}
		return nil, errors.New("ordinary CI failure")
	}}
	app := paths.WithRoot(t.TempDir())
	if err := app.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	executor := pipeline.NewExecutor(sc.DB, app, sc.Config, sc.Agent, []pipeline.Step{&PushStep{}, ci}, func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.StepName != nil && *event.StepName == types.StepCI && event.Status != nil && *event.Status == string(types.StepStatusFailed) {
			close(stepFailed)
		}
	})
	executor.SetSkippedSteps([]types.StepName{types.StepPush})
	executor.SetForgeContext(&forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"})
	done := make(chan error, 1)
	go func() { done <- executor.Execute(context.Background(), sc.Run, sc.Repo, ""); close(done) }()
	defer func() { unblock(); releaseWrite(); <-callbackDone; <-done }()
	select {
	case <-stepFailed:
	case <-time.After(10 * time.Second):
		t.Fatal("ordinary failure did not start")
	}
	select {
	case <-writeEntered:
		t.Fatal("terminal DB failure write bypassed the paused reporter's status lock")
	case <-time.After(250 * time.Millisecond):
	}
	unblock()
	<-callbackDone
	select {
	case <-writeEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("terminal failure did not resume after reporter")
	}
	releaseWrite()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ordinary failure completed successfully")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("terminal failure did not complete")
	}
	if err := sc.DB.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	run, err := reopened.GetRun(sc.Run.ID)
	if err != nil || run.Status != types.RunFailed {
		t.Fatalf("durable run outcome = %+v,%v", run, err)
	}
	publication, err := reopened.GetCommitStatusPublication(context.Background(), sc.Run.ID, p)
	if err != nil || publication.LastState != "failure" || publication.StrictCertificate != nil {
		t.Fatalf("durable publication = %+v,%v", publication, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(states) != "[success failure]" {
		t.Fatalf("HTTP final outcome = %v", states)
	}
}

func TestCommitStatus_TerminalPreparationFailureIsDurable(t *testing.T) {
	for _, mode := range []string{"git error", "head moved"} {
		t.Run(mode, func(t *testing.T) {
			sc, path, p := strictStatusContext(t)
			var mu sync.Mutex
			var states []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct{ State string }
				json.NewDecoder(r.Body).Decode(&payload)
				mu.Lock()
				states = append(states, payload.State)
				mu.Unlock()
				w.WriteHeader(201)
			}))
			defer server.Close()
			env, _ := fakeGH(t, "")
			env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL)
			for key, value := range environmentEntries(t, env) {
				t.Setenv(key, value)
			}
			(&PushStep{}).ReportPublishedHeadStatus(sc, "success", "")
			realGit, err := testgit.RealGit()
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			script := "#!/bin/sh\nif [ \"$1\" = status ] && [ \"$2\" = --porcelain ]; then\n"
			if mode == "git error" {
				script += "echo injected-terminal-git-error >&2\nexit 1\n"
			} else {
				script += fmt.Sprintf("%q commit --allow-empty -m terminal-movement >/dev/null || exit 1\n", realGit)
			}
			script += "fi\nexec " + fmt.Sprintf("%q", realGit) + " \"$@\"\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			ci := &transitionStep{name: types.StepCI, execute: func(sc *pipeline.StepContext) (*pipeline.StepOutcome, error) {
				cancel(errors.New(types.RunCancelReasonAbortedByUser))
				return nil, sc.Ctx.Err()
			}}
			app := paths.WithRoot(t.TempDir())
			if err := app.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			var terminalEvent string
			executor := pipeline.NewExecutor(sc.DB, app, sc.Config, sc.Agent, []pipeline.Step{&PushStep{}, ci}, func(event ipc.Event) {
				if event.Type == ipc.EventRunCompleted && event.Status != nil {
					terminalEvent = *event.Status
				}
			})
			executor.SetSkippedSteps([]types.StepName{types.StepPush})
			executor.SetForgeContext(&forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"})
			if err := executor.Execute(ctx, sc.Run, sc.Repo, sc.WorkDir); err == nil {
				t.Fatal("cancellation unexpectedly succeeded")
			}
			run, err := sc.DB.GetRun(sc.Run.ID)
			if err != nil || run.Status != types.RunCancelled || terminalEvent != string(run.Status) {
				t.Fatalf("event/DB terminal outcome disagree: event=%q,run=%+v,error=%v", terminalEvent, run, err)
			}
			for _, state := range []string{"pending", "success", "failure"} {
				(&PushStep{}).ReportPublishedHeadStatus(sc, state, "")
			}
			if err := sc.DB.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := db.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			run, err = reopened.GetRun(sc.Run.ID)
			if err != nil || run.Status != types.RunCancelled {
				t.Fatalf("recovery run = %+v,%v", run, err)
			}
			publication, err := reopened.GetCommitStatusPublication(context.Background(), sc.Run.ID, p)
			if err != nil || publication.LastState != "failure" || publication.StrictCertificate != nil || (publication.PreserveSuccess != nil && *publication.PreserveSuccess) {
				t.Fatalf("preparation error preserved proof: %+v,%v", publication, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if fmt.Sprint(states) != "[success failure]" {
				t.Fatalf("HTTP callbacks after failed preparation = %v", states)
			}
		})
	}
}

func TestCommitStatus_ObservationIsNonterminalAndExecutorCompletionSerializesPausedSuccess(t *testing.T) {
	for _, parked := range []bool{false, true} {
		for _, observed := range []string{"MERGED", "CLOSED", "OPEN"} {
			t.Run(fmt.Sprintf("parked_%v/%s", parked, observed), func(t *testing.T) {
				sc, path, publication := strictStatusContext(t)
				prURL := "https://github.com/test/repo/pull/42"
				sc.Run.PRURL = &prURL
				if observed == "OPEN" {
					if err := sc.DB.UpdateRunPRState(sc.Run.ID, "merged"); err != nil {
						t.Fatal(err)
					}
					if err := sc.DB.UpdateRunStatus(sc.Run.ID, types.RunRunning); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := sc.DB.BindRunPRContext(sc.Run.ID, db.PRContextCandidate{
					LocalHeadSHA: sc.Run.HeadSHA, TargetBranch: "main", TargetSHA: sc.Run.HeadSHA,
					MergeBaseSHA: sc.Run.HeadSHA, DiffDigest: strings.Repeat("a", 64),
				}, types.StepRebase); err != nil {
					t.Fatal(err)
				}
				stepEntered, stepRelease := make(chan struct{}), make(chan struct{})
				var stepOnce sync.Once
				finishStep := func() { stepOnce.Do(func() { close(stepRelease) }) }
				defer finishStep()
				completionDone := make(chan error, 1)
				completionRun := *sc.Run
				executor := pipeline.NewExecutor(sc.DB, paths.WithRoot(t.TempDir()), sc.Config, sc.Agent,
					[]pipeline.Step{&terminalCompletionBarrier{entered: stepEntered, release: stepRelease}}, nil)
				go func() { completionDone <- executor.Execute(context.Background(), &completionRun, sc.Repo, sc.WorkDir) }()
				select {
				case <-stepEntered:
				case <-time.After(10 * time.Second):
					t.Fatal("executor did not enter completion step")
				}
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var payload struct{ State string }
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.State != "success" {
						t.Errorf("unexpected HTTP status: %+v, %v", payload, err)
					}
					close(entered)
					<-release
					run, err := sc.DB.GetRun(sc.Run.ID)
					if err != nil || run.Status != types.RunRunning {
						t.Errorf("PR completion crossed pending HTTP publication: %+v, %v", run, err)
					}
					w.WriteHeader(201)
				}))
				defer server.Close()
				env, _ := fakeGHWithBase(t, prURL, "main")
				env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL, "FAKE_CLI_PR_STATE="+observed,
					"FAKE_CLI_PR_FACTS_JSON="+fmt.Sprintf(`{"merged":true,"state":"closed","head":{"sha":%q},"merge_commit_sha":%q}`, sc.Run.HeadSHA, sc.Run.HeadSHA))
				sc.Env = env
				for key, value := range environmentEntries(t, env) {
					t.Setenv(key, value)
				}
				reporterDone := make(chan struct{})
				go func() {
					defer close(reporterDone)
					(&PushStep{}).ReportPublishedHeadStatus(sc, "success", sc.Run.HeadSHA)
				}()
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatal("reporter did not reach HTTP")
				}
				ciDone := make(chan error, 1)
				stopPolling := errors.New("observation complete")
				ci := (&CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error { return stopPolling })
				go func() {
					var err error
					if parked {
						_, err = ci.ReconcileApprovalGate(sc)
					} else {
						_, err = ci.Execute(sc)
					}
					ciDone <- err
				}()
				select {
				case err := <-ciDone:
					if err != nil && !errors.Is(err, stopPolling) {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("nonterminal observation blocked behind status publication")
				}
				observedRun, err := sc.DB.GetRun(sc.Run.ID)
				if err != nil || observedRun.Status != types.RunRunning {
					t.Fatalf("observation terminalized run: %+v,%v", observedRun, err)
				}
				finishStep()
				select {
				case err := <-completionDone:
					t.Fatalf("executor completion bypassed outstanding HTTP: %v", err)
				case <-time.After(100 * time.Millisecond):
				}
				unblock()
				select {
				case <-reporterDone:
				case <-time.After(10 * time.Second):
					t.Fatal("reporter stuck")
				}
				select {
				case err := <-completionDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("executor completion stuck")
				}
				reopened, err := db.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				run, err := reopened.GetRun(sc.Run.ID)
				stored, saveErr := reopened.GetCommitStatusPublication(context.Background(), sc.Run.ID, publication)
				if saveErr != nil || stored.LastState != "success" {
					t.Fatalf("successful HTTP publication did not save before completion: %+v, %v", stored, saveErr)
				}
				if err != nil || run.Status != types.RunCompleted {
					t.Fatalf("terminal PR outcome not durable: %+v, %v", run, err)
				}
			})
		}
	}
}

func TestCommitStatus_LockDeadlineDoesNotBypassPausedSuccess(t *testing.T) {
	sc, path, publication := strictStatusContext(t)
	entered, release, callbackDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
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
		mu.Lock()
		states = append(states, payload.State)
		mu.Unlock()
		if payload.State == "success" {
			close(entered)
			<-release
		}
		w.WriteHeader(201)
	}))
	defer func() { unblock(); server.Close() }()
	env, _ := fakeGH(t, "")
	env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL, "FAKE_CLI_STATUS_PIPE_CHILD=1", "FAKE_CLI_STATUS_HTTP_TIMEOUT=60s", "FAKE_CLI_STATUS_PIPE_IGNORE_TERM=1")
	for key, value := range environmentEntries(t, env) {
		t.Setenv(key, value)
	}
	failing := make(chan struct{})
	terminal := make(chan struct{}, 1)
	ci := &transitionStep{name: types.StepCI, execute: func(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		go func() { defer close(callbackDone); ctx.PublishedHeadValidated(publication.SHA) }()
		<-entered
		close(failing)
		return nil, errors.New("ordinary failure during status publication")
	}}
	app := paths.WithRoot(t.TempDir())
	if err := app.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	executor := pipeline.NewExecutor(sc.DB, app, sc.Config, sc.Agent, []pipeline.Step{&PushStep{}, ci}, func(event ipc.Event) {
		if event.Type == ipc.EventRunCompleted {
			terminal <- struct{}{}
		}
	})
	executor.SetSkippedSteps([]types.StepName{types.StepPush})
	executor.SetForgeContext(&forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"})
	done := make(chan error, 1)
	go func() { done <- executor.Execute(context.Background(), sc.Run, sc.Repo, "") }()
	select {
	case <-failing:
	case <-time.After(10 * time.Second):
		t.Fatal("failure did not start")
	}
	time.Sleep(32 * time.Second)
	run, err := sc.DB.GetRun(sc.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status == types.RunRunning {
		select {
		case err := <-done:
			done <- err
			t.Errorf("executor abandoned unsettled terminal ownership: %v", err)
		default:
		}
	}
	select {
	case <-terminal:
		if run.Status == types.RunRunning {
			t.Error("unsettled terminal event emitted")
		}
	default:
	}
	unblock()
	<-callbackDone
	select {
	case err := <-done:
		if err == nil {
			t.Error("ordinary failure lost")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("failure settlement did not finish")
	}
	reopened, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	run, err = reopened.GetRun(sc.Run.ID)
	if err != nil || run.Status != types.RunFailed {
		t.Fatalf("durable terminal settlement: %+v,%v", run, err)
	}
	stored, err := reopened.GetCommitStatusPublication(context.Background(), sc.Run.ID, publication)
	if err != nil || stored.LastState != "failure" || stored.StrictCertificate != nil {
		t.Fatalf("durable failure publication: %+v,%v", stored, err)
	}
	sc.DB = reopened
	for _, state := range []string{"pending", "success", "failure"} {
		(&PushStep{}).ReportPublishedHeadStatus(sc, state, publication.SHA)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(states) != "[success failure]" {
		t.Fatalf("HTTP ordering = %v", states)
	}
}

type terminalCompletionBarrier struct{ entered, release chan struct{} }

func (*terminalCompletionBarrier) Name() types.StepName { return types.StepCI }
func (s *terminalCompletionBarrier) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	close(s.entered)
	<-s.release
	return &pipeline.StepOutcome{}, nil
}
