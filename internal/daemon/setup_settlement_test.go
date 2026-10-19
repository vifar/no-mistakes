package daemon

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"modernc.org/sqlite"
)

func TestSetupSettlementRetainsOwnershipUntilStorageRecovers(t *testing.T) {
	for _, mode := range []string{"queued-cancelled", "queued-config", "queued-panic", "unadmitted", "direct-unstarted", "direct-started"} {
		for _, fault := range []string{"raw", "terminal", "readback"} {
			t.Run(mode+"/"+fault, func(t *testing.T) {
				p := paths.WithRoot(t.TempDir())
				if err := p.EnsureDirs(); err != nil {
					t.Fatal(err)
				}
				d, err := db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				repo, head := setupTestGitRepo(t, p, d, "setup-settlement")
				run, err := d.InsertRun(repo.ID, "feature", head, head)
				if err != nil {
					t.Fatal(err)
				}
				var healthy atomic.Bool
				var attempts atomic.Int32
				function := "setup_fault_" + newShutdownToken()
				if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
					if !healthy.Load() {
						attempts.Add(1)
						return nil, errors.New("injected setup storage failure")
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
				defer d.Close()
				defer healthy.Store(true)
				control, err := sql.Open("sqlite", p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer control.Close()
				switch fault {
				case "raw":
					_, err = control.Exec(fmt.Sprintf(`CREATE TRIGGER fail_setup_raw BEFORE INSERT ON controlled_shutdown_runs BEGIN SELECT %s(); END`, function))
				case "terminal":
					_, err = control.Exec(fmt.Sprintf(`CREATE TRIGGER fail_setup_terminal BEFORE UPDATE OF status ON runs WHEN NEW.status IN ('failed','cancelled') BEGIN SELECT %s(); END`, function))
				case "readback":
					_, err = control.Exec(fmt.Sprintf(`ALTER TABLE runs RENAME TO saved_setup_runs;
CREATE VIEW runs AS SELECT * FROM saved_setup_runs WHERE status IN ('pending','running','queued') OR %s()=1;
CREATE TRIGGER setup_update INSTEAD OF UPDATE ON runs BEGIN
 UPDATE saved_setup_runs SET status=NEW.status,error=NEW.error,push_active=NEW.push_active,terminal_head_verified_at=NEW.terminal_head_verified_at,updated_at=NEW.updated_at,worktree_dir=NEW.worktree_dir WHERE id=OLD.id;
END`, function))
				}
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("NM_DEMO", "1")
				if mode == "queued-config" {
					if err := os.WriteFile(p.ConfigFile(), []byte("agent: [broken"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				constructed := make(chan struct{}, 1)
				mgr := NewRunManager(d, p, func() []pipeline.Step {
					constructed <- struct{}{}
					panic("injected setup panic")
				})
				mgr.sched.limits = nil
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				handle := &runHandle{runCtx: ctx, cancel: cancel, done: make(chan struct{})}
				if mode == "queued-cancelled" {
					cancel(&pipeline.ControlledShutdownCause{Instance: mgr.runtimeInstance, Lifecycle: "planned"})
				}
				if err := pipeline.RecordRawShutdown(context.Background(), d, []string{run.ID}, mgr.runtimeInstance, func() {}); (err != nil) != (fault == "raw") {
					t.Fatalf("raw storage fault: %v", err)
				}
				var slot *runSlot
				if mode != "unadmitted" {
					var ok bool
					slot, ok = mgr.sched.tryAdmit(run.ID)
					if !ok {
						t.Fatal("fixture did not acquire slot")
					}
				}
				if strings.HasPrefix(mode, "queued") {
					mgr.mu.Lock()
					mgr.runs[run.ID] = &managedRun{run: run, state: ownershipClaimed, handle: handle}
					mgr.mu.Unlock()
				}
				before := attempts.Load()
				done := make(chan error, 1)
				go func() {
					switch {
					case mode == "unadmitted":
						done <- mgr.failUnadmittedRunSetup(ctx, run, errors.New("queue run: injected failure"))
					case strings.HasPrefix(mode, "queued"):
						mgr.launchQueuedRun(run.ID, slot, handle)
						done <- nil
					default:
						if mode == "direct-unstarted" {
							run.HeadSHA = "invalid-head"
						}
						_, err := mgr.launchRun(ctx, repo, run, config.DefaultGlobalConfig(), "test", nil, slot, nil)
						done <- err
					}
				}()
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
				started := mode == "direct-started" || mode == "queued-panic"
				if started {
					select {
					case <-constructed:
					case <-time.After(10 * time.Second):
						t.Fatal("setup never reached worktree-backed failure")
					}
				}
				wait("repeated storage failures", func() bool { return attempts.Load() >= before+2 })
				select {
				case err := <-done:
					t.Fatalf("setup relinquished custody while storage failed: %v", err)
				default:
				}
				mgr.mu.Lock()
				owned := mgr.runs[run.ID]
				var captured *runHandle
				if owned != nil {
					captured = owned.handle
				}
				mgr.mu.Unlock()
				if owned == nil {
					t.Fatal("setup ownership disappeared before settlement")
				}
				if mode != "unadmitted" && mgr.sched.stats().Active != 1 {
					t.Fatal("setup slot released before settlement")
				}
				if captured != nil {
					select {
					case <-captured.done:
						t.Fatal("setup handle closed before settlement")
					default:
					}
				}
				table := "runs"
				if fault == "readback" {
					table = "saved_setup_runs"
				}
				var worktree sql.NullString
				var status types.RunStatus
				if err := control.QueryRow("SELECT status,worktree_dir FROM "+table+" WHERE id=?", run.ID).Scan(&status, &worktree); err != nil {
					t.Fatal(err)
				}
				if fault == "readback" && status != types.RunFailed {
					t.Fatalf("readback fault did not follow committed failure: %s", status)
				}
				if fault != "readback" && status != types.RunPending {
					t.Fatalf("unsettled write fabricated terminal status: %s", status)
				}
				if started {
					if !worktree.Valid {
						t.Fatal("missing worktree record")
					}
					if _, err := os.Stat(worktree.String); err != nil {
						t.Fatalf("worktree removed before settlement: %v", err)
					}
				}
				cancel(&pipeline.ControlledShutdownCause{Instance: mgr.runtimeInstance, Lifecycle: "planned-during-settlement"})
				healthy.Store(true)
				select {
				case err := <-done:
					if mode == "unadmitted" && err != nil {
						t.Fatal(err)
					}
					if strings.HasPrefix(mode, "direct") && err == nil {
						t.Fatal("setup failure lost its error")
					}
				case <-time.After(10 * time.Second):
					t.Fatal("healthy storage did not finish without a second request")
				}
				stored, err := d.GetRun(run.ID)
				wantReason := db.RawShutdownReason
				switch mode {
				case "queued-config":
					wantReason = "load config:"
				case "queued-panic", "direct-started":
					wantReason = "internal panic: injected setup panic"
				case "unadmitted":
					wantReason = "queue run: injected failure"
				case "direct-unstarted":
					wantReason = "invalid-head"
				}
				if err != nil || stored.Status != types.RunFailed || stored.Error == nil || !strings.Contains(*stored.Error, wantReason) {
					t.Fatalf("raw setup outcome: %+v, %v; want original %q", stored, err, wantReason)
				}
				mgr.mu.Lock()
				remaining := mgr.runs[run.ID]
				mgr.mu.Unlock()
				if remaining != nil {
					t.Fatal("settled setup retained ownership")
				}
				if mode != "unadmitted" && mgr.sched.stats().Active != 0 {
					t.Fatal("settled setup retained its slot")
				}
				if started {
					if _, err := os.Stat(worktree.String); !os.IsNotExist(err) {
						t.Fatalf("settled setup worktree remains: %v", err)
					}
				}
			})
		}
	}
}

func TestSetupSettlementClosedDatabaseRetainsOwnership(t *testing.T) {
	for _, mode := range []string{"queued", "direct", "unadmitted"} {
		t.Run(mode, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRun(repo.ID, "feature", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			mgr := NewRunManager(d, p, nil)
			mgr.sched.limits = nil
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			handle := &runHandle{runCtx: ctx, cancel: cancel, done: make(chan struct{})}
			var slot *runSlot
			if mode != "unadmitted" {
				var ok bool
				slot, ok = mgr.sched.tryAdmit(run.ID)
				if !ok {
					t.Fatal("no slot")
				}
			}
			if mode == "queued" {
				mgr.runs[run.ID] = &managedRun{run: run, state: ownershipClaimed, handle: handle}
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "queued":
				mgr.launchQueuedRun(run.ID, slot, handle)
			case "direct":
				_, err = mgr.launchRun(ctx, repo, run, config.DefaultGlobalConfig(), "test", nil, slot, nil)
			case "unadmitted":
				err = mgr.failUnadmittedRunSetup(ctx, run, errors.New("injected setup failure"))
			}
			if mode != "queued" && (err == nil || !strings.Contains(err.Error(), "closed")) {
				t.Fatalf("closed database not explicit: %v", err)
			}
			owned := mgr.runs[run.ID]
			if owned == nil {
				t.Fatal("closed database released custody")
			}
			if owned.handle != nil {
				select {
				case <-owned.handle.done:
					t.Fatal("closed database closed setup handle")
				default:
				}
			}
			if mode != "unadmitted" && mgr.sched.stats().Active != 1 {
				t.Fatal("closed database released slot")
			}
		})
	}
}
