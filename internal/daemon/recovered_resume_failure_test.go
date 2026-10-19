package daemon

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"modernc.org/sqlite"
)

func TestRecoveredResumeIndependentFailureSurvivesCancellationAndStorageRecovery(t *testing.T) {
	for _, stage := range []string{"run read", "reconciliation"} {
		for _, cancellation := range []string{"operator", "planned", "raw"} {
			t.Run(stage+"/"+cancellation, func(t *testing.T) {
				p := paths.WithRoot(t.TempDir())
				if err := p.EnsureDirs(); err != nil {
					t.Fatal(err)
				}
				d, err := db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				repo, head := setupTestGitRepo(t, p, d, "resume-failure")
				run, err := d.InsertRun(repo.ID, "main", head, head)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				worktree := p.WorktreeDir(repo.ID, run.ID)
				if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, head); err != nil {
					t.Fatal(err)
				}
				if err := d.SetRunWorktreeDir(run.ID, worktree); err != nil {
					t.Fatal(err)
				}
				sr, err := d.InsertStepResult(run.ID, types.StepDocument)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.StartStep(sr.ID); err != nil {
					t.Fatal(err)
				}
				findings := `{"findings":[],"summary":"durable gate"}`
				if err := d.SetStepFindings(sr.ID, findings); err != nil {
					t.Fatal(err)
				}
				if _, err := d.InsertReviewStepRound(sr.ID, 1, "initial", &findings, nil, head, 1); err != nil {
					t.Fatal(err)
				}
				if err := parkGateFixture(d, sr.ID, types.StepStatusAwaitingApproval, 1); err != nil {
					t.Fatal(err)
				}
				if err := d.SetRunAwaitingAgent(run.ID); err != nil {
					t.Fatal(err)
				}
				run, err = d.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				var armed, healthy, registered atomic.Bool
				healthy.Store(true)
				var resumeFailures, terminalFailures atomic.Int32
				failureText := "independent Resume " + stage + " failure"
				resumeFunction := "resume_failure_" + newShutdownToken()
				if err := sqlite.RegisterScalarFunction(resumeFunction, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
					if armed.Load() {
						resumeFailures.Add(1)
						return nil, errors.New(failureText)
					}
					return int64(1), nil
				}); err != nil {
					t.Fatal(err)
				}
				terminalFunction := "resume_terminal_" + newShutdownToken()
				if err := sqlite.RegisterScalarFunction(terminalFunction, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
					if !healthy.Load() {
						terminalFailures.Add(1)
						return nil, errors.New("terminal storage temporarily unavailable")
					}
					return int64(1), nil
				}); err != nil {
					t.Fatal(err)
				}
				d.Close()
				d, err = db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				control, err := sql.Open("sqlite", p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer control.Close()
				table := "runs"
				if stage == "run read" {
					if _, err := control.Exec(fmt.Sprintf(`ALTER TABLE runs RENAME TO saved_resume_runs;
CREATE VIEW runs AS SELECT * FROM saved_resume_runs WHERE %s()=1;
CREATE TRIGGER forward_resume_update INSTEAD OF UPDATE ON runs BEGIN
 UPDATE saved_resume_runs SET status=NEW.status,error=NEW.error,head_sha=NEW.head_sha,push_active=NEW.push_active,terminal_head_verified_at=NEW.terminal_head_verified_at,updated_at=NEW.updated_at WHERE id=OLD.id;
END`, resumeFunction)); err != nil {
						t.Fatal(err)
					}
					table = "saved_resume_runs"
				} else {
					if _, err := control.Exec(fmt.Sprintf(`CREATE TRIGGER fail_resume_reconciliation BEFORE INSERT ON status_validation_epochs BEGIN SELECT %s(); END`, resumeFunction)); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := control.Exec(fmt.Sprintf(`CREATE TRIGGER fail_resume_terminal BEFORE UPDATE OF status ON %s WHEN NEW.status IN ('failed','cancelled') BEGIN SELECT %s(); END`, table, terminalFunction)); err != nil {
					t.Fatal(err)
				}
				mgr := NewRunManager(d, p, nil)
				var once sync.Once
				mgr.sched.limits = func() (int, bool) { once.Do(func() { registered.Store(true); armed.Store(true) }); return 0, false }
				returned, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				mgr.afterExecute = func(string) { close(returned); <-release }
				defer func() {
					armed.Store(false)
					healthy.Store(true)
					unblock()
					exited := make(chan struct{})
					go func() { mgr.wg.Wait(); close(exited) }()
					select {
					case <-exited:
					case <-time.After(10 * time.Second):
						t.Error("recovered failure cleanup did not finish")
					}
					d.Close()
				}()
				mgr.resumeRecoveredRun(recoveredRunPlan{run: run, repo: repo, workDir: worktree, gateDir: p.RepoDir(repo.ID), cfg: config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{}), agent: recoveredRunTestAgent{}, steps: []pipeline.Step{shutdownApprovalStep{}}})
				select {
				case <-returned:
				case <-time.After(5 * time.Second):
					t.Fatal("Resume did not reach the injected independent early return")
				}
				if !registered.Load() || resumeFailures.Load() != 1 {
					t.Fatalf("fixture did not fail Resume after successful registration: registered=%v failures=%d", registered.Load(), resumeFailures.Load())
				}
				mgr.mu.Lock()
				owned := mgr.runs[run.ID]
				mgr.mu.Unlock()
				if owned == nil || owned.executor == nil || owned.executor.CurrentGate().Nonce == "" {
					t.Fatal("initial recovered gate was not registered")
				}
				armed.Store(false)
				var cause error = errors.New(types.RunCancelReasonAbortedByUser)
				switch cancellation {
				case "planned":
					cause = &pipeline.ControlledShutdownCause{Instance: mgr.runtimeInstance, Lifecycle: "planned"}
					if err := d.PrepareControlledShutdown([]string{run.ID}, mgr.runtimeInstance, "planned", cause.Error()); err != nil {
						t.Fatal(err)
					}
				case "raw":
					cause = pipeline.ErrDaemonShutdown
					if err := pipeline.RecordRawShutdown(context.Background(), d, []string{run.ID}, mgr.runtimeInstance, func() {}); err != nil {
						t.Fatal(err)
					}
				}
				owned.handle.cancel(cause)
				healthy.Store(false)
				unblock()
				deadline := time.Now().Add(5 * time.Second)
				for terminalFailures.Load() < 2 {
					if time.Now().After(deadline) {
						t.Fatal("terminal failure did not retain a storage recovery continuation")
					}
					time.Sleep(10 * time.Millisecond)
				}
				select {
				case <-owned.handle.done:
					t.Fatal("failed storage relinquished recovered custody")
				default:
				}
				if !testRunOwned(mgr, run.ID) {
					t.Fatal("recovered ownership lost before settlement")
				}
				if _, err := os.Stat(worktree); err != nil {
					t.Fatalf("unsettled recovered worktree removed: %v", err)
				}
				var status types.RunStatus
				if err := control.QueryRow("SELECT status FROM "+table+" WHERE id=?", run.ID).Scan(&status); err != nil || status != types.RunRunning {
					t.Fatalf("failed storage fabricated terminal status: %s, %v", status, err)
				}
				var shutdownDone <-chan struct{}
				if stage == "run read" && cancellation == "raw" {
					shutdownDone = holdShutdownPastDrainDeadline(t, mgr, d)
					if !testRunOwned(mgr, run.ID) {
						t.Fatal("shutdown drain deadline relinquished recovered custody")
					}
				}
				healthy.Store(true)
				if shutdownDone != nil {
					awaitSettlementShutdown(t, shutdownDone)
				}
				select {
				case <-owned.handle.done:
				case <-time.After(10 * time.Second):
					t.Fatal("healthy storage did not settle the original failure automatically")
				}
				stored, err := d.GetRun(run.ID)
				if err != nil || stored.Status != types.RunFailed || stored.Error == nil || !strings.Contains(*stored.Error, failureText) || strings.Contains(*stored.Error, cause.Error()) {
					t.Fatalf("independent Resume failure overwritten by %s: %+v, %v", cancellation, stored, err)
				}
				if testRunOwned(mgr, run.ID) {
					t.Fatal("settled recovered failure retained ownership")
				}
				if _, err := os.Stat(worktree); !os.IsNotExist(err) {
					t.Fatalf("settled recovered worktree remains: %v", err)
				}
				original := *stored.Error
				if _, err := d.RecoverStaleRuns("daemon restarted"); err != nil {
					t.Fatal(err)
				}
				reopened, err := db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				stored, err = reopened.GetRun(run.ID)
				if err != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != original {
					t.Fatalf("restart lost original terminal truth: %+v, %v", stored, err)
				}
			})
		}
	}
}
