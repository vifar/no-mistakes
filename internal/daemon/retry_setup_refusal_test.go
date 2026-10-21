package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRetrySetupRefusalPreservesSourceOpportunity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("agent stand-ins require POSIX shell")
	}
	for _, queued := range []bool{false, true} {
		for _, fault := range []string{"global", "trusted", "instruction budget", "placement"} {
			t.Run(fmt.Sprintf("queued=%v/%s", queued, fault), func(t *testing.T) {
				f := newRetryFixture(t, retryFixtureOptions{beforeSource: func(t *testing.T, f *retryFixture) {
					writeFakeAgents(t, f.p, "claude", "", "claude")
					if fault == "instruction budget" {
						body, err := os.ReadFile(f.p.ConfigFile())
						if err != nil {
							t.Fatal(err)
						}
						body = append(body, []byte("review:\n  path_instructions:\n"+pathInstructionsYAML("    ", "g", 20))...)
						if err := os.WriteFile(f.p.ConfigFile(), body, 0600); err != nil {
							t.Fatal(err)
						}
					}
				}})
				m := f.manager(t, &countingStep{name: types.StepTest, count: f.executions})
				original, err := os.ReadFile(f.p.ConfigFile())
				if err != nil {
					t.Fatal(err)
				}
				breakSetup := func() {
					switch fault {
					case "global":
						if err := os.WriteFile(f.p.ConfigFile(), []byte("review: [broken\n"), 0600); err != nil {
							t.Fatal(err)
						}
					case "trusted", "instruction budget":
						body := "review: [broken\n"
						if fault == "instruction budget" {
							body = "review:\n  path_instructions:\n" + pathInstructionsYAML("    ", "t", 20)
						}
						if err := os.WriteFile(filepath.Join(f.repo.WorkingPath, ".no-mistakes.yaml"), []byte(body), 0600); err != nil {
							t.Fatal(err)
						}
						gitCmd(t, f.repo.WorkingPath, "add", ".no-mistakes.yaml")
						gitCmd(t, f.repo.WorkingPath, "commit", "-m", "change trusted launch policy")
						gitCmd(t, f.repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
						gitCmd(t, f.repo.WorkingPath, "reset", "--hard", f.submitted)
					case "placement":
						body := string(original) + fmt.Sprintf("worktree_roots:\n  %q: %q\n", f.repo.WorkingPath, f.repo.WorkingPath)
						if err := os.WriteFile(f.p.ConfigFile(), []byte(body), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				var holder *runSlot
				if queued {
					m.sched.limits = func() (int, bool) { return 1, false }
					var ok bool
					holder, ok = m.sched.tryAdmit("capacity-holder")
					if !ok {
						t.Fatal("cannot hold capacity")
					}
					defer holder.Release()
				} else {
					m.afterRetryProof = breakSetup
				}
				link, retryErr := m.HandleRetry(context.Background(), f.repo.ID, "main", f.source.ID, f.callerHead)
				if queued {
					if retryErr != nil {
						t.Fatalf("queue retry: %v", retryErr)
					}
					raw, err := sql.Open("sqlite", f.p.DB())
					if err != nil {
						t.Fatal(err)
					}
					defer raw.Close()
					if _, err := raw.Exec(`CREATE TABLE retry_refusal_observations (run_id TEXT, error TEXT);
 CREATE TRIGGER observe_retry_refusal AFTER UPDATE OF status ON runs WHEN NEW.status='failed'
 BEGIN INSERT INTO retry_refusal_observations VALUES (NEW.id, NEW.error); END`); err != nil {
						t.Fatal(err)
					}
					breakSetup()
					holder.Release()
					m.wg.Wait()
					var failure string
					if err := raw.QueryRow("SELECT error FROM retry_refusal_observations WHERE run_id=?", link.RunID).Scan(&failure); err != nil {
						t.Fatal(err)
					}
					retryErr = errors.New(failure)
					if remaining, err := f.d.GetRun(link.RunID); err != nil || remaining != nil {
						t.Fatalf("refused continuation retained: %+v %v", remaining, err)
					}
				} else if retryErr == nil {
					t.Fatal("invalid setup was accepted")
				}
				want := map[string]string{"global": "yaml", "trusted": "trusted", "instruction budget": "40 entries combined", "placement": "worktree"}[fault]
				if retryErr == nil || !strings.Contains(retryErr.Error(), want) {
					t.Fatalf("wrong %s refusal: %v", fault, retryErr)
				}
				f.requireNothingLaunched(t)
				m.mu.Lock()
				owners, slots := len(m.runs), m.sched.active
				m.mu.Unlock()
				if owners != 0 || slots != 0 {
					t.Fatalf("orphan ownership: handles=%d slots=%d", owners, slots)
				}
				if err := os.WriteFile(f.p.ConfigFile(), original, 0600); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, f.p.RepoDir(f.repo.ID), "update-ref", "refs/heads/main", f.submitted)
				m.afterRetryProof = nil
				accepted, err := m.HandleRetry(context.Background(), f.repo.ID, "main", f.source.ID, f.callerHead)
				if err != nil {
					t.Fatalf("retry opportunity consumed: %v", err)
				}
				if got := waitForRunTerminalState(t, f.d, accepted.RunID); got.Status != types.RunCompleted {
					t.Fatalf("corrected retry status=%s error=%v", got.Status, got.Error)
				}
			})
		}
	}
}

func TestRetryAdmissionWriteRefusalPreservesSourceOpportunity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("agent stand-ins require POSIX shell")
	}
	f := newRetryFixture(t, retryFixtureOptions{beforeSource: func(t *testing.T, f *retryFixture) { writeFakeAgents(t, f.p, "claude", "", "claude") }})
	m := f.manager(t, &countingStep{name: types.StepTest, count: f.executions})
	m.sched.limits = func() (int, bool) { return 1, false }
	holder, ok := m.sched.tryAdmit("capacity-holder")
	if !ok {
		t.Fatal("cannot hold capacity")
	}
	defer holder.Release()
	m.markQueued = func(string, string) (int64, error) { return 0, errors.New("injected queue write refusal") }
	if _, err := m.HandleRetry(context.Background(), f.repo.ID, "main", f.source.ID, f.callerHead); err == nil || !strings.Contains(err.Error(), "injected queue write refusal") {
		t.Fatalf("wrong refusal: %v", err)
	}
	f.requireNothingLaunched(t)
	m.mu.Lock()
	owners := len(m.runs)
	m.mu.Unlock()
	if owners != 0 {
		t.Fatalf("orphan owners=%d", owners)
	}
	m.markQueued = f.d.MarkRunQueued
	holder.Release()
	accepted, err := m.HandleRetry(context.Background(), f.repo.ID, "main", f.source.ID, f.callerHead)
	if err != nil {
		t.Fatal(err)
	}
	if got := waitForRunTerminalState(t, f.d, accepted.RunID); got.Status != types.RunCompleted {
		t.Fatalf("corrected retry status=%s", got.Status)
	}
}

func TestRetrySetupStorageFailureRetainsOwnershipAndEvidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("agent stand-ins require POSIX shell")
	}
	f := newRetryFixture(t, retryFixtureOptions{beforeSource: func(t *testing.T, f *retryFixture) { writeFakeAgents(t, f.p, "claude", "", "claude") }})
	m := f.manager(t, &countingStep{name: types.StepTest, count: f.executions})
	original, err := os.ReadFile(f.p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	m.sched.limits = func() (int, bool) { return 1, false }
	holder, ok := m.sched.tryAdmit("capacity-holder")
	if !ok {
		t.Fatal("cannot hold capacity")
	}
	defer holder.Release()
	link, err := m.HandleRetry(context.Background(), f.repo.ID, "main", f.source.ID, f.callerHead)
	if err != nil {
		t.Fatal(err)
	}
	evidence := f.p.RunEvidenceDir("", link.RunID)
	if err := os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(evidence, "preserve.txt")
	if err := os.WriteFile(marker, []byte("owned evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", f.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(fmt.Sprintf(`CREATE TRIGGER reject_retry_terminal BEFORE UPDATE OF status ON runs WHEN NEW.id='%s' AND NEW.status IN ('failed','cancelled') BEGIN SELECT RAISE(FAIL,'injected settlement failure'); END`, link.RunID)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := raw.Exec(`DROP TRIGGER IF EXISTS reject_retry_terminal`); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(f.p.ConfigFile(), original, 0600); err != nil {
			t.Error(err)
		}
		m.wg.Wait()
	}()
	if err := os.WriteFile(f.p.ConfigFile(), []byte("review: [broken\n"), 0600); err != nil {
		t.Fatal(err)
	}
	holder.Release()
	waitForSchedCondition(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := pipeline.AcquireCommitStatusLock(ctx)
		if err == nil {
			pipeline.ReleaseCommitStatusLock()
		}
		return err != nil && strings.Contains(err.Error(), "injected settlement failure")
	})
	m.mu.Lock()
	owned, active := m.runs[link.RunID], m.sched.active
	m.mu.Unlock()
	if owned == nil || owned.handle == nil || active != 1 {
		t.Fatalf("unsettled ownership released: owned=%+v active=%d", owned, active)
	}
	select {
	case <-owned.handle.done:
		t.Fatal("unsettled handle closed")
	default:
	}
	if row, err := f.d.GetRun(link.RunID); err != nil || row == nil || row.Status.Terminal() {
		t.Fatalf("unsettled row lost: %+v %v", row, err)
	}
	if row, err := f.d.GetRunRetryBySource(f.source.ID); err != nil || row == nil {
		t.Fatalf("unsettled audit lost: %+v %v", row, err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "owned evidence" {
		t.Fatalf("unsettled evidence lost: %q %v", data, err)
	}
	if f.executions.Load() != 0 {
		t.Fatal("unsettled setup executed a step")
	}
	if _, err := raw.Exec(`DROP TRIGGER reject_retry_terminal`); err != nil {
		t.Fatal(err)
	}
	m.wg.Wait()
	f.requireNothingLaunched(t)
	m.mu.Lock()
	owners, slots := len(m.runs), m.sched.active
	m.mu.Unlock()
	if owners != 0 || slots != 0 {
		t.Fatalf("settled ownership retained: owners=%d slots=%d", owners, slots)
	}
}

func TestRetryRefusalCleanupRecoversStorageFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("agent stand-ins require POSIX shell")
	}
	for _, queued := range []bool{false, true} {
		for _, fault := range []string{"read", "delete"} {
			t.Run(fmt.Sprintf("queued=%v/%s", queued, fault), func(t *testing.T) {
				f := newRetryFixture(t, retryFixtureOptions{beforeSource: func(t *testing.T, f *retryFixture) { writeFakeAgents(t, f.p, "claude", "", "claude") }})
				m := f.manager(t, &countingStep{name: types.StepTest, count: f.executions})
				original, err := os.ReadFile(f.p.ConfigFile())
				if err != nil {
					t.Fatal(err)
				}
				raw, err := sql.Open("sqlite", f.p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				var unavailable atomic.Bool
				unavailable.Store(true)
				var reads atomic.Int32
				m.readRetryCleanup = func(id string) (*db.RunRetry, error) {
					attempt := reads.Add(1)
					if fault == "read" && unavailable.Load() {
						return nil, fmt.Errorf("injected cleanup read failure: %w", sql.ErrConnDone)
					}
					if !queued && attempt == 1 {
						return nil, errors.New("injected initial cleanup read refusal")
					}
					return f.d.GetRunRetry(id)
				}
				if fault == "delete" {
					if _, err := raw.Exec(`CREATE TRIGGER reject_retry_delete BEFORE DELETE ON runs WHEN OLD.id != '` + f.source.ID + `' BEGIN SELECT RAISE(FAIL,'injected cleanup delete failure'); END`); err != nil {
						t.Fatal(err)
					}
				}
				restore := func() {
					unavailable.Store(false)
					if _, err := raw.Exec(`DROP TRIGGER IF EXISTS reject_retry_delete`); err != nil {
						t.Error(err)
					}
				}
				defer restore()
				breakSetup := func() {
					if err := os.WriteFile(f.p.ConfigFile(), []byte("review: [broken\n"), 0600); err != nil {
						panic(err)
					}
				}
				var holder *runSlot
				if queued {
					m.sched.limits = func() (int, bool) { return 1, false }
					var ok bool
					holder, ok = m.sched.tryAdmit("capacity-holder")
					if !ok {
						t.Fatal("cannot hold capacity")
					}
					defer holder.Release()
				}
				result := make(chan error, 1)
				go func() {
					_, err := m.HandleRetry(context.Background(), f.repo.ID, "main", f.source.ID, f.callerHead)
					result <- err
				}()
				var link *db.RunRetry
				waitForSchedCondition(t, func() bool {
					var err error
					link, err = f.d.GetRunRetryBySource(f.source.ID)
					return err == nil && link != nil
				})
				if queued {
					if err := <-result; err != nil {
						t.Fatal(err)
					}
					breakSetup()
					holder.Release()
				}
				waitForSchedCondition(t, func() bool {
					ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					defer cancel()
					err := pipeline.AcquireCommitStatusLock(ctx)
					if err == nil {
						pipeline.ReleaseCommitStatusLock()
					}
					return err != nil && strings.Contains(err.Error(), "injected cleanup "+fault+" failure")
				})
				run, err := f.d.GetRun(link.RunID)
				if err != nil || run == nil || !run.Status.Terminal() {
					t.Fatalf("unsettled refusal: %+v %v", run, err)
				}
				if fault == "read" {
					m.mu.Lock()
					owner := m.runs[link.RunID]
					m.mu.Unlock()
					if owner == nil || owner.handle == nil {
						t.Fatal("cleanup read failure lost ownership")
					}
				}
				if f.executions.Load() != 0 {
					t.Fatal("refused setup executed a step")
				}
				restore()
				if !queued {
					if err := <-result; err == nil {
						t.Fatal("invalid setup accepted")
					}
				}
				m.wg.Wait()
				f.requireNothingLaunched(t)
				m.mu.Lock()
				owners, slots := len(m.runs), m.sched.active
				m.mu.Unlock()
				if owners != 0 || slots != 0 {
					t.Fatalf("cleanup retained owners=%d slots=%d", owners, slots)
				}
				if err := os.WriteFile(f.p.ConfigFile(), original, 0600); err != nil {
					t.Fatal(err)
				}
				accepted, err := m.HandleRetry(context.Background(), f.repo.ID, "main", f.source.ID, f.callerHead)
				if err != nil {
					t.Fatalf("source retry unavailable after storage recovery: %v", err)
				}
				if got := waitForRunTerminalState(t, f.d, accepted.RunID); got.Status != types.RunCompleted {
					t.Fatalf("successful continuation: %s %v", got.Status, got.Error)
				}
			})
		}
	}
}
