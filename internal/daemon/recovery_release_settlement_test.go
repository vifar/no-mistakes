package daemon

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"modernc.org/sqlite"
)

func TestRecoveryReleaseSettlesBeforeRelinquishingCustody(t *testing.T) {
	for _, mode := range []string{"live-gate", "recovered-gate", "ci-monitor"} {
		for _, fault := range []string{"delete", "invalidate", "closed"} {
			t.Run(mode+"/"+fault, func(t *testing.T) {
				p := paths.WithRoot(t.TempDir())
				if err := p.EnsureDirs(); err != nil {
					t.Fatal(err)
				}
				d, err := db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				repo, head := setupTestGitRepo(t, p, d, "release-settlement")
				var healthy atomic.Bool
				healthy.Store(true)
				var attempts atomic.Int32
				function := "release_fault_" + newShutdownToken()
				if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
					if !healthy.Load() {
						attempts.Add(1)
						return nil, errors.New("injected recovery release failure")
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
				trigger := fmt.Sprintf(`CREATE TRIGGER fail_recovery_release BEFORE DELETE ON controlled_shutdown_runs BEGIN SELECT %s(); END`, function)
				if fault == "invalidate" {
					trigger = fmt.Sprintf(`CREATE TRIGGER fail_recovery_invalidation BEFORE INSERT ON status_validation_epochs BEGIN SELECT %s(); END`, function)
				}
				if _, err := control.Exec(trigger); err != nil {
					t.Fatal(err)
				}
				t.Setenv("NM_DEMO", "1")
				entered := make(chan struct{})
				var step pipeline.Step = shutdownApprovalStep{}
				if mode == "ci-monitor" {
					step = &shutdownWatchStep{execute: func(sc *pipeline.StepContext) (*pipeline.StepOutcome, error) {
						url := "https://github.com/test/repo/pull/7"
						if err := sc.DB.UpdateRunPRURL(sc.Run.ID, url); err != nil {
							return nil, err
						}
						sc.Run.PRURL = &url
						close(entered)
						<-sc.Ctx.Done()
						return nil, sc.Ctx.Err()
					}}
				}
				mgr := NewRunManager(d, p, func() []pipeline.Step { return []pipeline.Step{step} })
				mgr.runtimeStartedAt = time.Now().UTC()
				t.Cleanup(func() {
					healthy.Store(true)
					if !d.Closed() {
						mgr.Shutdown()
					}
					d.Close()
				})
				var run *db.Run
				if mode == "recovered-gate" {
					run, err = d.InsertRun(repo.ID, "main", head, head)
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
					candidate, err := testgit.RunComparisonCandidate(run, worktree, "main", head, "", "", "")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := d.BindRunPRContext(run.ID, candidate, types.StepReview); err != nil {
						t.Fatal(err)
					}
					if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, head); err != nil {
						t.Fatal(err)
					}
					sr, err := d.InsertStepResult(run.ID, types.StepDocument)
					if err != nil {
						t.Fatal(err)
					}
					if err := d.StartStep(sr.ID); err != nil {
						t.Fatal(err)
					}
					findings := `{"findings":[],"summary":"needs approval"}`
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
					mgr.resumeRecoveredRun(recoveredRunPlan{run: run, repo: repo, workDir: worktree, gateDir: p.RepoDir(repo.ID), cfg: config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{}), agent: recoveredRunTestAgent{}, steps: []pipeline.Step{step}})
				} else {
					id, err := mgr.startRun(context.Background(), repo, "main", head, head, "test", nil, "intent", "", false, "", "", nil)
					if err != nil {
						t.Fatal(err)
					}
					run, err = d.GetRun(id)
					if err != nil {
						t.Fatal(err)
					}
				}
				wait := func(label string, condition func() bool) {
					t.Helper()
					deadline := time.Now().Add(10 * time.Second)
					for !condition() {
						if time.Now().After(deadline) {
							t.Fatalf("timeout: %s", label)
						}
						time.Sleep(10 * time.Millisecond)
					}
				}
				if mode == "ci-monitor" {
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("CI monitor never started")
					}
				} else {
					wait("registered approval gate", func() bool {
						mgr.mu.Lock()
						owned := mgr.runs[run.ID]
						mgr.mu.Unlock()
						return owned != nil && owned.executor != nil && owned.executor.CurrentGate().Nonce != "" && mgr.StateRev(run.ID) > 0
					})
				}
				run, err = d.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				epoch, err := d.StatusValidationEpoch(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				mgr.mu.Lock()
				handle := mgr.runs[run.ID].handle
				mgr.mu.Unlock()
				healthy.Store(false)
				request := ipc.ShutdownParams{ExpectedPID: os.Getpid(), ExpectedStartedAt: mgr.runtimeStartedAt.Format(time.RFC3339Nano), ExpectedInstance: mgr.runtimeInstance}
				bounded, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				ack, prepareErr := mgr.prepareControlledShutdown(bounded, request)
				cancel()
				if prepareErr == nil || ack != nil {
					t.Fatalf("unsettled release acknowledged: %+v, %v", ack, prepareErr)
				}
				wait("repeated release failures", func() bool { return attempts.Load() >= 2 })
				if !testRunOwned(mgr, run.ID) {
					t.Fatal("release failure relinquished run custody")
				}
				mgr.mu.Lock()
				left := mgr.leftForRecovery[run.ID]
				mgr.mu.Unlock()
				if left {
					t.Fatal("unsettled release marked safe for recovery")
				}
				select {
				case <-handle.done:
					t.Fatal("unsettled release closed its handle")
				default:
				}
				if _, err := os.Stat(run.WorktreePath()); err != nil {
					t.Fatalf("recovery worktree removed: %v", err)
				}
				var shutdownRecords int
				if err := control.QueryRow("SELECT count(*) FROM controlled_shutdown_runs WHERE run_id=?", run.ID).Scan(&shutdownRecords); err != nil || shutdownRecords != 1 {
					t.Fatalf("failed release lost its durable shutdown record: %d, %v", shutdownRecords, err)
				}
				if fault == "closed" {
					if err := d.Close(); err != nil {
						t.Fatal(err)
					}
					exited := make(chan struct{})
					go func() { mgr.wg.Wait(); close(exited) }()
					select {
					case <-exited:
					case <-time.After(5 * time.Second):
						t.Fatal("closed database did not fail the release explicitly")
					}
					if mgr.releaseShutdownForRecovery(run.ID) {
						t.Fatal("closed database reported a settled release")
					}
					if !testRunOwned(mgr, run.ID) {
						t.Fatal("closed database lost custody")
					}
					select {
					case <-handle.done:
						t.Fatal("closed database released its handle")
					default:
					}
					mgr.mu.Lock()
					left = mgr.leftForRecovery[run.ID]
					mgr.mu.Unlock()
					if left {
						t.Fatal("closed database marked recovery settled")
					}
					t.Cleanup(func() { close(handle.done) })
					return
				}
				var shutdownDone <-chan struct{}
				if mode == "live-gate" && fault == "delete" {
					shutdownDone = holdShutdownPastDrainDeadline(t, mgr, d)
					if !testRunOwned(mgr, run.ID) {
						t.Fatal("shutdown drain deadline relinquished recovery-release custody")
					}
				}
				healthy.Store(true)
				if shutdownDone != nil {
					awaitSettlementShutdown(t, shutdownDone)
				}
				select {
				case <-handle.done:
				case <-time.After(10 * time.Second):
					t.Fatal("release did not recover without another initiating request")
				}
				if testRunOwned(mgr, run.ID) {
					t.Fatal("settled recovery release retained live ownership")
				}
				mgr.mu.Lock()
				left = mgr.leftForRecovery[run.ID]
				mgr.mu.Unlock()
				if !left {
					t.Fatal("settled release did not record recoverability")
				}
				if _, err := os.Stat(run.WorktreePath()); err != nil {
					t.Fatalf("settled recovery removed worktree: %v", err)
				}
				if err := control.QueryRow("SELECT count(*) FROM controlled_shutdown_runs WHERE run_id=?", run.ID).Scan(&shutdownRecords); err != nil || shutdownRecords != 0 {
					t.Fatalf("shutdown record remains: %d, %v", shutdownRecords, err)
				}
				after, err := d.StatusValidationEpoch(run.ID)
				increments := int64(1)
				if shutdownDone != nil {
					increments++
				}
				if err != nil || after != epoch+increments {
					t.Fatalf("release did not atomically invalidate proof: epoch %d -> %d, %v", epoch, after, err)
				}
				retryCtx, retryCancel := context.WithTimeout(context.Background(), time.Second)
				defer retryCancel()
				ack, err = mgr.prepareControlledShutdown(retryCtx, request)
				if shutdownDone != nil {
					if err == nil || ack != nil {
						t.Fatal("raw shutdown permitted a planned acknowledgement")
					}
				} else if err != nil || ack == nil || !ack.OK {
					t.Fatalf("matching shutdown retry rejected settled release: %+v, %v", ack, err)
				}
				preserved := make(map[string]struct{})
				if mode != "ci-monitor" {
					parked, err := d.GetRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := pipeline.ValidateRecoveredRun(d, parked, []pipeline.Step{step}); err != nil {
						t.Fatalf("settled park is not recoverable: %v", err)
					}
					preserved[run.ID] = struct{}{}
				}
				if _, err := d.RecoverStaleRunsExcept("daemon crashed during execution", preserved); err != nil {
					t.Fatal(err)
				}
				stored, err := d.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "ci-monitor" {
					if stored.Status != types.RunCIMonitorInterrupted {
						t.Fatalf("monitor restart status: %s", stored.Status)
					}
				} else if stored.Status != types.RunRunning || stored.AwaitingAgentSince == nil {
					t.Fatalf("park lost restart eligibility: %+v", stored)
				}
			})
		}
	}
}
