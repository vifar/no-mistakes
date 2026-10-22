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
	"os/exec"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"modernc.org/sqlite"
)

// Issue #64's oracle: cancellation cannot replace acknowledged SUCCESS while
// exact execution proof is unchanged. Storage failure supplies no new verdict.
func cancellationStatusOracle(t *testing.T, sc *pipeline.StepContext, initialState string) func(...string) {
	t.Helper()
	var mu sync.Mutex
	var posted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ State string }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		mu.Lock()
		posted = append(posted, payload.State)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	env, _ := fakeGH(t, "")
	env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL)
	for key, value := range environmentEntries(t, env) {
		t.Setenv(key, value)
	}
	(&PushStep{}).ReportPublishedHeadStatus(sc, initialState, "")
	return func(want ...string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if !reflect.DeepEqual(posted, want) {
			t.Fatalf("status POSTs=%v, want %v", posted, want)
		}
	}
}

func cancelWatcher(t *testing.T, sc *pipeline.StepContext) error {
	t.Helper()
	return cancelWatcherWithReason(t, sc, types.RunCancelReasonAbortedByUser)
}

func cancelWatcherWithReason(t *testing.T, sc *pipeline.StepContext, reason string) error {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ci := &transitionStep{name: types.StepCI, execute: func(s *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		cancel(errors.New(reason))
		return nil, s.Ctx.Err()
	}}
	app := paths.WithRoot(t.TempDir())
	if err := app.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	enrollExecutorGate(t, app, sc.Repo.ID)
	executor := pipeline.NewExecutor(sc.DB, app, sc.Config, sc.Agent, []pipeline.Step{&PushStep{}, ci}, nil)
	executor.SetSkippedSteps([]types.StepName{types.StepPush})
	executor.SetForgeContext(&forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"})
	return executor.Execute(ctx, sc.Run, sc.Repo, sc.WorkDir)
}

func assertCancelledCertificate(t *testing.T, sc *pipeline.StepContext, path string, p db.CommitStatusPublication, preserved bool) {
	t.Helper()
	reopened, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	run, err := reopened.GetRun(sc.Run.ID)
	if err != nil || run.Status != types.RunCancelled || run.Error == nil || *run.Error != types.RunCancelReasonAbortedByUser {
		t.Fatalf("cancelled outcome/reason did not survive reopen: %+v, %v", run, err)
	}
	publication, err := reopened.GetCommitStatusPublication(context.Background(), run.ID, p)
	if err != nil || publication == nil {
		t.Fatalf("publication=%+v,%v", publication, err)
	}
	got := publication.PreserveSuccess != nil && *publication.PreserveSuccess && publication.StrictCertificate != nil
	wantState := "failure"
	if preserved {
		wantState = "success"
	}
	if got != preserved || publication.LastState != wantState {
		t.Fatalf("durable status=%+v, want preserved=%v", publication, preserved)
	}
}

func TestCancelCertifiedWatcherKeepsPostedSuccess(t *testing.T) {
	sc, path, p := strictStatusContext(t)
	assertPosts := cancellationStatusOracle(t, sc, "success")
	if err := cancelWatcher(t, sc); err == nil {
		t.Fatal("cancel unexpectedly succeeded")
	}
	for _, state := range []string{"pending", "success", "failure"} {
		(&PushStep{}).ReportPublishedHeadStatus(sc, state, "")
	}
	assertPosts("success")
	assertCancelledCertificate(t, sc, path, p, true)
}

func TestCancelCertifiedWatcherRetriesDurableDecision(t *testing.T) {
	sc, path, p := strictStatusContext(t)
	assertPosts := cancellationStatusOracle(t, sc, "success")
	var healthy atomic.Bool
	attempts := make(chan struct{}, 16)
	function := "cancel_retry_" + sc.Run.ID
	if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		if !healthy.Load() {
			select {
			case attempts <- struct{}{}:
			default:
			}
			return nil, errors.New("temporary terminal storage outage")
		}
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := sc.DB.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sc.DB = database
	control, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(fmt.Sprintf(`CREATE TRIGGER cancel_storage BEFORE UPDATE OF status ON runs WHEN NEW.status='cancelled' BEGIN SELECT %s(); END`, function)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cancelWatcher(t, sc) }()
	defer func() {
		healthy.Store(true)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("settlement owner did not exit")
		}
	}()
	for range 2 {
		select {
		case <-attempts:
		case <-time.After(10 * time.Second):
			t.Fatal("terminal write did not retry")
		}
	}
	select {
	case err := <-done:
		done <- err
		t.Fatalf("abandoned undurable cancellation: %v", err)
	default:
	}
	run, err := database.GetRun(sc.Run.ID)
	if err != nil || run.Status != types.RunRunning {
		t.Fatalf("fault committed premature outcome: %+v,%v", run, err)
	}
	assertPosts("success")
	healthy.Store(true)
	select {
	case err := <-done:
		done <- err
		if err == nil {
			t.Fatal("cancel unexpectedly succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("healthy storage did not settle cancellation")
	}
	assertPosts("success")
	assertCancelledCertificate(t, sc, path, p, true)
}

func TestCancelCertificateBindsExactValidation(t *testing.T) {
	for _, binding := range []string{"head", "config", "push generation", "gate execution"} {
		t.Run(binding, func(t *testing.T) {
			sc, path, p := strictStatusContext(t)
			assertPosts := cancellationStatusOracle(t, sc, "success")
			stored, err := sc.DB.GetCommitStatusPublication(context.Background(), sc.Run.ID, p)
			if err != nil || stored.StrictCertificate == nil {
				t.Fatalf("missing certificate: %+v,%v", stored, err)
			}
			// Change the acknowledged certificate's binding; all other durable proof
			// stays intact. A certificate for another validation must never authorize it.
			var certificate db.StatusCertificate
			if err := json.Unmarshal([]byte(*stored.StrictCertificate), &certificate); err != nil {
				t.Fatal(err)
			}
			switch binding {
			case "head":
				certificate.SHA = "another-head"
			case "config":
				certificate.ConfigDigest = "another-config"
			case "push generation":
				certificate.PushGeneration++
			case "gate execution":
				certificate.ProofDigest = "another-execution"
			}
			encoded, err := json.Marshal(certificate)
			if err != nil {
				t.Fatal(err)
			}
			raw := string(encoded)
			stored.StrictCertificate = &raw
			if err := sc.DB.PutCommitStatusPublication(sc.Run.ID, *stored); err != nil {
				t.Fatal(err)
			}
			if err := cancelWatcher(t, sc); err == nil {
				t.Fatal("cancel unexpectedly succeeded")
			}
			assertPosts("success", "failure")
			assertCancelledCertificate(t, sc, path, p, false)
		})
	}
}

func TestCancelAfterRepairRevokesCertificate(t *testing.T) {
	sc, path, p := strictStatusContext(t)
	assertPosts := cancellationStatusOracle(t, sc, "success")
	ci, err := sc.DB.InsertStepResult(sc.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.DB.StartStepFixRound(ci.ID, 3); err != nil {
		t.Fatal(err)
	}
	assertRevokedBeforeCancel(t, sc, p)
	if err := cancelWatcher(t, sc); err == nil {
		t.Fatal("cancel unexpectedly succeeded")
	}
	assertPosts("success", "failure")
	assertCancelledCertificate(t, sc, path, p, false)
}

func TestCancelAfterValidationResetRevokesCertificate(t *testing.T) {
	sc, path, p := strictStatusContext(t)
	assertPosts := cancellationStatusOracle(t, sc, "success")
	if err := sc.DB.ResetStepsFrom(sc.Run.ID, types.StepReview.Order()); err != nil {
		t.Fatal(err)
	}
	assertRevokedBeforeCancel(t, sc, p)
	if err := cancelWatcher(t, sc); err == nil {
		t.Fatal("cancel unexpectedly succeeded")
	}
	assertPosts("success", "failure")
	assertCancelledCertificate(t, sc, path, p, false)
}

func TestCancelAfterReplacingPostRevokesCertificate(t *testing.T) {
	sc, path, p := strictStatusContext(t)
	assertPosts := cancellationStatusOracle(t, sc, "success")
	(&PushStep{}).ReportPublishedHeadStatus(sc, "pending", "")
	assertRevokedBeforeCancel(t, sc, p)
	if err := cancelWatcher(t, sc); err == nil {
		t.Fatal("cancel unexpectedly succeeded")
	}
	assertPosts("success", "pending", "failure")
	assertCancelledCertificate(t, sc, path, p, false)
}

func assertRevokedBeforeCancel(t *testing.T, sc *pipeline.StepContext, p db.CommitStatusPublication) {
	t.Helper()
	stored, err := sc.DB.GetCommitStatusPublication(context.Background(), sc.Run.ID, p)
	if err != nil || stored == nil || stored.StrictCertificate != nil || stored.PreserveSuccess != nil {
		t.Fatalf("certificate survived revocation: %+v,%v", stored, err)
	}
}

// The process really is git: its shell alias signals its parent Git process.
// An operator abort racing that signal must not turn uncertainty into proof.
func TestCancelSignalledGitCannotPreserveSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix signal journey")
	}
	sc, path, p := strictStatusContext(t)
	assertPosts := cancellationStatusOracle(t, sc, "success")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ci := &transitionStep{name: types.StepCI, execute: func(s *pipeline.StepContext) (*pipeline.StepOutcome, error) {
		_, err := git.Run(s.Ctx, s.WorkDir, "-c", "alias.issue64-signal=!kill -KILL $PPID", "issue64-signal")
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != -1 {
			t.Errorf("real Git was not killed by a signal: %v", err)
		}
		cancel(errors.New(types.RunCancelReasonAbortedByUser))
		combined := errors.Join(err, s.Ctx.Err())
		if pipeline.CancellationOnly(combined, context.Cause(ctx)) {
			t.Error("signal became qualifying cancellation")
		}
		return nil, combined
	}}
	app := paths.WithRoot(t.TempDir())
	if err := app.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	enrollExecutorGate(t, app, sc.Repo.ID)
	executor := pipeline.NewExecutor(sc.DB, app, sc.Config, sc.Agent, []pipeline.Step{&PushStep{}, ci}, nil)
	executor.SetSkippedSteps([]types.StepName{types.StepPush})
	executor.SetForgeContext(&forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"})
	if err := executor.Execute(ctx, sc.Run, sc.Repo, sc.WorkDir); err == nil {
		t.Fatal("signalled command succeeded")
	}
	reopened, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	run, err := reopened.GetRun(sc.Run.ID)
	if err != nil || run.Status != types.RunFailed || run.Error == nil || *run.Error == types.RunCancelReasonAbortedByUser {
		t.Fatalf("signal lost uncertain failure: %+v,%v", run, err)
	}
	publication, err := reopened.GetCommitStatusPublication(context.Background(), run.ID, p)
	if err != nil || publication == nil || publication.StrictCertificate != nil || publication.LastState != "failure" {
		t.Fatalf("signal preserved certificate: %+v,%v", publication, err)
	}
	assertPosts("success", "failure")
}

func TestCancelSupersededWatcherStillPostsFailure(t *testing.T) {
	sc, path, p := strictStatusContext(t)
	assertPosts := cancellationStatusOracle(t, sc, "success")
	if err := cancelWatcherWithReason(t, sc, types.RunCancelReasonSuperseded); err == nil {
		t.Fatal("supersession unexpectedly succeeded")
	}
	reopened, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	run, err := reopened.GetRun(sc.Run.ID)
	if err != nil || run.Status != types.RunCancelled || run.Error == nil || *run.Error != types.RunCancelReasonSuperseded {
		t.Fatalf("supersession outcome=%+v,%v", run, err)
	}
	publication, err := reopened.GetCommitStatusPublication(context.Background(), run.ID, p)
	if err != nil || publication == nil || publication.StrictCertificate != nil || publication.LastState != "failure" {
		t.Fatalf("supersession retained success: %+v,%v", publication, err)
	}
	assertPosts("success", "failure")
}

func TestCancelPendingWatcherStillPostsFailure(t *testing.T) {
	sc, path, p := strictStatusContext(t)
	assertPosts := cancellationStatusOracle(t, sc, "pending")
	if err := cancelWatcher(t, sc); err == nil {
		t.Fatal("cancel unexpectedly succeeded")
	}
	assertPosts("pending", "failure")
	assertCancelledCertificate(t, sc, path, p, false)
}
