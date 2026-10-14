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
	if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		close(writeEntered)
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
