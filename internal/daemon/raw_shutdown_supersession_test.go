package daemon

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"modernc.org/sqlite"
)

type rawFailureLogPause struct {
	slog.Handler
	once             sync.Once
	entered, release chan struct{}
}

func (h *rawFailureLogPause) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "raw shutdown terminal outcome unsettled" {
		h.once.Do(func() { close(h.entered); <-h.release })
	}
	return h.Handler.Handle(ctx, r)
}

type strictShutdownPass struct{ name types.StepName }

func (s strictShutdownPass) Name() types.StepName { return s.name }
func (s strictShutdownPass) Execute(sc *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	out := &pipeline.StepOutcome{}
	if s.name == types.StepReview {
		out.ReviewApprovedHeadSHA = sc.Run.HeadSHA
	}
	if s.name == types.StepTest {
		out.Findings = fmt.Sprintf(`{"findings":[],"tested_head_sha":%q}`, sc.Run.HeadSHA)
	}
	return out, nil
}

type shutdownWatchStep struct {
	execute func(*pipeline.StepContext) (*pipeline.StepOutcome, error)
}

func (*shutdownWatchStep) Name() types.StepName { return types.StepCI }
func (s *shutdownWatchStep) Execute(sc *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return s.execute(sc)
}

func TestRawShutdown_SupersedesOnlyUncommittedCancellation(t *testing.T) {
	bin := t.TempDir()
	if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, "gh"), "../pipeline/fakecli").CombinedOutput(); err != nil {
		t.Fatalf("fake gh: %v: %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "gh")
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLI_REAL_GIT", realGit)
	if err := os.Link(filepath.Join(bin, "gh"), filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	for _, causeKind := range []string{"planned", "user", "provider", "joined"} {
		for _, first := range []string{"raw", "preservation", "raw rollback", "raw persistent", "terminal persistent"} {
			t.Run(causeKind+"/"+first, func(t *testing.T) {
				rawFirst := first != "preservation"
				failedRaw := first == "raw rollback" || first == "raw persistent"
				persistentRaw := first == "raw persistent"
				failedTerminal := first == "terminal persistent"
				var storageHealthy atomic.Bool
				repeatedFailure := make(chan struct{})
				var repeatOnce sync.Once
				logEntered, logRelease := make(chan struct{}), make(chan struct{})
				var logOnce sync.Once
				releaseLog := func() { logOnce.Do(func() { close(logRelease) }) }
				defer releaseLog()
				if failedRaw {
					previous := slog.Default()
					slog.SetDefault(slog.New(&rawFailureLogPause{Handler: slog.NewTextHandler(io.Discard, nil), entered: logEntered, release: logRelease}))
					defer slog.SetDefault(previous)
				}
				var rawAttempts atomic.Int32
				independent := errors.New("independent provider failure https://operator:secret@forge.example/repo")
				gitMarker := filepath.Join(t.TempDir(), "terminal-preparation")
				t.Setenv("FAKE_CLI_TERMINAL_GIT_MARKER", gitMarker)
				gitEntered, gitRelease := make(chan struct{}), make(chan struct{})
				var gitOnce, gitReleaseOnce sync.Once
				releaseGit := func() { gitReleaseOnce.Do(func() { close(gitRelease) }) }
				defer releaseGit()
				independentFailure := causeKind == "provider" || causeKind == "joined"
				p := paths.WithRoot(t.TempDir())
				if err := p.EnsureDirs(); err != nil {
					t.Fatal(err)
				}
				d, err := db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				repo, head := setupTestGitRepo(t, p, d, "raw-certified")
				run, err := d.InsertRun(repo.ID, "main", head, head)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: "fingerprint", Ref: "refs/heads/main"}); err != nil {
					t.Fatal(err)
				}
				cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
				publication := db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true}
				if err := d.PutCommitStatusPublication(run.ID, publication); err != nil {
					t.Fatal(err)
				}
				terminalEntered, terminalRelease := make(chan struct{}), make(chan struct{})
				var terminalOnce sync.Once
				releaseTerminal := func() { terminalOnce.Do(func() { close(terminalRelease) }) }
				defer releaseTerminal()
				function := "pause_terminal_" + newShutdownToken()
				if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
					close(terminalEntered)
					<-terminalRelease
					return int64(1), nil
				}); err != nil {
					t.Fatal(err)
				}
				faultFunction := "raw_fault_" + newShutdownToken()
				if failedRaw || failedTerminal {
					if err := sqlite.RegisterScalarFunction(faultFunction, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
						attempt := rawAttempts.Add(1)
						if (failedRaw && attempt == 1) || ((persistentRaw || failedTerminal) && !storageHealthy.Load()) {
							if attempt >= 3 {
								repeatOnce.Do(func() { close(repeatedFailure) })
							}
							return nil, errors.New("raw write storage failure")
						}
						return int64(1), nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				d.Close()
				d, err = db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				control, err := sql.Open("sqlite", p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer control.Close()
				if failedRaw {
					if _, err := control.Exec(fmt.Sprintf(`CREATE TRIGGER fail_raw BEFORE INSERT ON controlled_shutdown_runs WHEN NEW.reason='daemon shutting down' BEGIN SELECT %s(); END`, faultFunction)); err != nil {
						t.Fatal(err)
					}
				}
				if failedTerminal {
					if _, err := control.Exec(fmt.Sprintf("CREATE TRIGGER fail_terminal BEFORE UPDATE OF status ON runs WHEN NEW.status='failed' BEGIN SELECT %s(); END", faultFunction)); err != nil {
						t.Fatal(err)
					}
				}
				if first == "preservation" && !independentFailure {
					if _, err := control.Exec(fmt.Sprintf(`CREATE TRIGGER pause_preservation BEFORE UPDATE ON commit_status_publications WHEN NEW.run_id=%q AND NEW.preserve_success=1 AND OLD.preserve_success IS NULL BEGIN SELECT %s(); END`, run.ID, function)); err != nil {
						t.Fatal(err)
					}
				}
				var mu sync.Mutex
				var states []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/terminal-head" {
						gitOnce.Do(func() { close(gitEntered) })
						<-gitRelease
						return
					}
					var payload struct{ State string }
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					mu.Lock()
					states = append(states, payload.State)
					mu.Unlock()
					w.WriteHeader(201)
				}))
				defer func() { releaseGit(); server.Close() }()
				t.Setenv("FAKE_CLI_STATUS_API_URL", server.URL)
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				ready, cancelled, watchRelease, rawDispatched := make(chan struct{}), make(chan error, 1), make(chan struct{}), make(chan struct{})
				var watchOnce, rawOnce sync.Once
				releaseWatch := func() { watchOnce.Do(func() { close(watchRelease) }) }
				defer releaseWatch()
				watcher := &shutdownWatchStep{execute: func(sc *pipeline.StepContext) (*pipeline.StepOutcome, error) {
					(&steps.PushStep{}).ReportPublishedHeadStatus(sc, "success", head)
					stored, err := sc.DB.GetCommitStatusPublication(context.Background(), run.ID, publication)
					if err != nil || stored == nil || stored.StrictCertificate == nil || stored.LastState != "success" {
						return nil, fmt.Errorf("actual success lacked strict proof: %+v,%v", stored, err)
					}
					close(ready)
					<-sc.Ctx.Done()
					cancelled <- context.Cause(sc.Ctx)
					<-watchRelease
					if (independentFailure || failedRaw) && rawFirst {
						if err := os.WriteFile(gitMarker, []byte("armed"), 0600); err != nil {
							return nil, err
						}
					}
					if causeKind == "provider" {
						return nil, independent
					}
					if causeKind == "joined" {
						return nil, errors.Join(independent, sc.Ctx.Err())
					}
					return nil, sc.Ctx.Err()
				}}
				core := []pipeline.Step{strictShutdownPass{types.StepReview}, strictShutdownPass{types.StepTest}, strictShutdownPass{types.StepDocument}, strictShutdownPass{types.StepLint}, watcher, &steps.PushStep{}}
				mgr := NewRunManager(d, p, nil)
				mgr.runtimeStartedAt = time.Now().UTC()
				executor := pipeline.NewExecutor(d, p, cfg, recoveredRunTestAgent{}, core, mgr.broadcast)
				executor.SetRuntimeInstance(mgr.runtimeInstance)
				executor.SetForgeContext(&forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"})
				registerTestRunCancel(mgr, run.ID, func(cause error) {
					cancel(cause)
					if cause != nil && cause.Error() == "daemon shutting down" {
						rawOnce.Do(func() { close(rawDispatched) })
					}
				})
				mgr.runs[run.ID].executor = executor
				mgr.wg.Add(1)
				finished := make(chan error, 1)
				go func() { defer mgr.wg.Done(); finished <- executor.Execute(ctx, run, repo, repo.WorkingPath) }()
				defer func() {
					storageHealthy.Store(true)
					releaseGit()
					releaseWatch()
					releaseTerminal()
					releaseLog()
					if failedRaw {
						_ = pipeline.WithCommitStatusLock(context.Background(), func() error { return nil })
					}
				}()
				select {
				case <-ready:
				case err := <-finished:
					t.Fatalf("watcher never certified: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("watcher did not start")
				}
				request := ipc.ShutdownParams{ExpectedPID: os.Getpid(), ExpectedStartedAt: mgr.runtimeStartedAt.Format(time.RFC3339Nano), ExpectedInstance: mgr.runtimeInstance}
				plannedDone := make(chan error, 1)
				if causeKind == "planned" {
					go func() { _, err := mgr.prepareControlledShutdown(context.Background(), request); plannedDone <- err }()
				} else {
					if err := mgr.HandleCancel(run.ID); err != nil {
						t.Fatal(err)
					}
				}
				var firstCause error
				select {
				case firstCause = <-cancelled:
				case <-time.After(time.Second):
					t.Fatal("cancellation did not reach watcher")
				}
				if causeKind == "planned" {
					planned, ok := firstCause.(*pipeline.ControlledShutdownCause)
					if !ok {
						t.Fatalf("planned provenance missing: %v", firstCause)
					}
					matches, err := d.ControlledShutdownMatches(run.ID, planned.Instance, planned.Lifecycle, planned.Error())
					if err != nil || !matches {
						t.Fatalf("planned cancellation preceded durable binding: %v,%v", matches, err)
					}
				}
				subscription, err := mgr.Subscribe(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				defer subscription.Close()
				if first == "preservation" {
					releaseWatch()
					if !independentFailure {
						select {
						case <-terminalEntered:
						case <-time.After(time.Second):
							t.Fatal("terminal transaction did not pause")
						}
					}
				}
				if first == "preservation" {
					releaseTerminal()
					select {
					case err := <-finished:
						finished <- err
					case <-time.After(time.Second):
						t.Fatal("preservation did not commit")
					}
				}
				if (independentFailure || failedRaw) && rawFirst {
					releaseWatch()
					select {
					case <-gitEntered:
					case <-time.After(5 * time.Second):
						t.Fatal("independent failure did not reach terminal Git preparation")
					}
				}
				rawDone := make(chan struct{})
				go func() { mgr.Shutdown(); close(rawDone) }()
				select {
				case <-rawDispatched:
				case <-time.After(time.Second):
					t.Fatal("raw shutdown did not reach captured cancellation")
				}
				if failedRaw {
					select {
					case <-logEntered:
					case <-time.After(5 * time.Second):
						t.Fatal("raw rollback did not reach owned handoff")
					}
					releaseGit()
					if persistentRaw {
						select {
						case <-repeatedFailure:
						case <-time.After(5 * time.Second):
							t.Fatal("owned raw fence did not survive repeated storage failure")
						}
						stored, err := d.GetRun(run.ID)
						if err != nil || stored.Status != types.RunRunning {
							t.Fatalf("unsettled storage fabricated terminal truth: %+v,%v", stored, err)
						}
						select {
						case err := <-finished:
							t.Fatalf("executor relinquished unsettled custody: %v", err)
						default:
						}
						if ack, err := mgr.prepareControlledShutdown(context.Background(), request); err == nil || ack != nil {
							t.Fatalf("persistent raw storage failure acknowledged planned shutdown: %+v,%v", ack, err)
						}
						storageHealthy.Store(true)

					}
					select {
					case err := <-finished:
						finished <- err
					case <-time.After(10 * time.Second):
						t.Fatal("candidate did not settle behind failed raw write")
					}
					releaseLog()
				}
				if independentFailure && rawFirst {
					if err := pipeline.WithCommitStatusLock(context.Background(), func() error {
						revoked, err := d.RawShutdownSuperseded(run.ID)
						if err != nil {
							return err
						}
						if !revoked {
							return fmt.Errorf("raw supersession not committed")
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					sc := &pipeline.StepContext{Ctx: context.Background(), DB: d, Run: run, Repo: repo, Config: cfg, WorkDir: repo.WorkingPath, ForgeContext: &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}}
					for _, state := range []string{"pending", "success", "failure"} {
						(&steps.PushStep{}).ReportPublishedHeadStatus(sc, state, head)
					}
					releaseGit()
				}
				if context.Cause(ctx) != firstCause {
					t.Fatal("raw shutdown replaced immutable first cause")
				}
				releaseTerminal()
				releaseWatch()
				if failedTerminal {
					select {
					case <-repeatedFailure:
					case <-time.After(5 * time.Second):
						t.Fatal("terminal storage did not fail repeatedly")
					}
					stored, err := d.GetRun(run.ID)
					if err != nil || stored.Status != types.RunRunning {
						t.Fatalf("failed terminal storage fabricated outcome: %+v,%v", stored, err)
					}
					select {
					case err := <-finished:
						t.Fatalf("terminal owner exited before recovery: %v", err)
					default:
					}
					if ack, err := mgr.prepareControlledShutdown(context.Background(), request); err == nil || ack != nil {
						t.Fatalf("unsettled terminal storage acknowledged shutdown: %+v,%v", ack, err)
					}
					storageHealthy.Store(true)
				}
				select {
				case err := <-finished:
					if independentFailure && !errors.Is(err, independent) {
						t.Errorf("independent error identity lost: %v", err)
					}
					if err == nil {
						t.Fatal("cancelled watcher completed successfully")
					}
				case <-time.After(10 * time.Second):
					t.Fatal("terminal decision did not finish")
				}
				select {
				case <-rawDone:
				case <-time.After(time.Second):
					t.Fatal("raw shutdown did not drain")
				}
				if causeKind == "planned" {
					if err := <-plannedDone; err == nil && rawFirst {
						t.Error("raw supersession produced planned ACK")
					}
				}
				reopened, err := db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				stored, err := reopened.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				pub, err := reopened.GetCommitStatusPublication(context.Background(), run.ID, publication)
				if err != nil {
					t.Fatal(err)
				}
				expectedStatus := types.RunFailed
				expectedStates := "[pending success failure]"
				if first == "preservation" && !independentFailure {
					expectedStatus = types.RunCancelled
					expectedStates = "[pending success]"
				}
				if stored.Status != expectedStatus {
					t.Errorf("terminal status=%s,want %s", stored.Status, expectedStatus)
				}
				if independentFailure && (stored.Error == nil || (!strings.Contains(*stored.Error, "independent provider failure") || strings.Contains(*stored.Error, "secret"))) {
					t.Errorf("real failure replaced: %+v", stored)
				}
				if rawFirst && !independentFailure && (stored.Error == nil || *stored.Error != "daemon shutting down" || pub.StrictCertificate != nil || (pub.PreserveSuccess != nil && *pub.PreserveSuccess)) {
					t.Errorf("raw shutdown retained eligibility or planned reason: %+v,%+v", stored, pub)
				}
				if first == "preservation" && !independentFailure && (pub.StrictCertificate == nil || pub.PreserveSuccess == nil || !*pub.PreserveSuccess) {
					t.Errorf("committed preservation changed: %+v", pub)
				}
				eventCtx, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				event, ok := subscription.Next(eventCtx)
				if !ok || event.StateRev != mgr.StateRev(run.ID) || (event.Type != ipc.EventStreamGap && (event.Type != ipc.EventRunCompleted || event.Status == nil || *event.Status != string(expectedStatus))) {
					t.Errorf("public terminal outcome: %+v,%v", event, ok)
				}
				sc := &pipeline.StepContext{Ctx: context.Background(), DB: reopened, Run: stored, Repo: repo, Config: cfg, WorkDir: repo.WorkingPath, ForgeContext: &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}}
				for _, state := range []string{"pending", "success", "failure"} {
					(&steps.PushStep{}).ReportPublishedHeadStatus(sc, state, head)
				}
				mu.Lock()
				defer mu.Unlock()
				if fmt.Sprint(states) != expectedStates {
					t.Errorf("HTTP outcome=%v,want %s", states, expectedStates)
				}
			})
		}
	}
}
