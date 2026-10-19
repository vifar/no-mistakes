package daemon

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"modernc.org/sqlite"
)

func TestSetupFailureProvenanceSurvivesPublicationDelay(t *testing.T) {
	for _, boundary := range []string{"unadmitted", "unstarted", "started"} {
		for _, shutdown := range []string{"operator", "planned", "raw"} {
			for _, provenance := range []string{"independent", "joined", "cancellation"} {
				t.Run(boundary+"/"+shutdown+"/"+provenance, func(t *testing.T) {
					p := paths.WithRoot(t.TempDir())
					if err := p.EnsureDirs(); err != nil {
						t.Fatal(err)
					}
					d, err := db.Open(p.DB())
					if err != nil {
						t.Fatal(err)
					}
					defer d.Close()
					repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo", "main")
					if err != nil {
						t.Fatal(err)
					}
					run, err := d.InsertRun(repo.ID, "feature", "head", "base")
					if err != nil {
						t.Fatal(err)
					}
					mgr := NewRunManager(d, p, nil)
					ctx, cancel := context.WithCancelCause(context.Background())
					defer cancel(nil)
					if boundary != "unadmitted" {
						mgr.runs[run.ID] = &managedRun{run: run, state: ownershipClaimed, handle: &runHandle{runCtx: ctx, cancel: cancel, done: make(chan struct{})}}
					}
					var cause error = errors.New(types.RunCancelReasonAbortedByUser)
					if shutdown != "operator" {
						cause = &pipeline.ControlledShutdownCause{Instance: mgr.runtimeInstance, Lifecycle: "planned"}
						if err := d.PrepareControlledShutdown([]string{run.ID}, mgr.runtimeInstance, "planned", cause.Error()); err != nil {
							t.Fatal(err)
						}
					}
					var setupErr error = errors.New("independent config parsing failure")
					switch provenance {
					case "joined":
						setupErr = errors.Join(setupErr, context.Canceled)
					case "cancellation":
						setupErr = fmt.Errorf("setup interrupted: %w", context.Canceled)
					}
					pipeline.CommitStatusLock <- struct{}{}
					locked := true
					defer func() {
						if locked {
							pipeline.ReleaseCommitStatusLock()
						}
					}()
					done := make(chan error, 1)
					go func() {
						if boundary == "unadmitted" {
							done <- mgr.failUnadmittedRunSetup(ctx, run, setupErr)
						} else {
							done <- mgr.failRunSetup(context.Background(), run.ID, boundary == "started", setupErr)
						}
					}()
					select {
					case err := <-done:
						t.Fatalf("publication delay did not retain settlement: %v", err)
					case <-time.After(20 * time.Millisecond):
					}
					cancel(cause)
					if shutdown == "raw" {
						if err := d.SupersedeUncommittedCancellations([]string{run.ID}, mgr.runtimeInstance); err != nil {
							t.Fatal(err)
						}
					}
					stored, err := d.GetRun(run.ID)
					if err != nil || stored.Status != types.RunPending {
						t.Fatalf("unsettled outcome: %+v, %v", stored, err)
					}
					pipeline.ReleaseCommitStatusLock()
					locked = false
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("settlement did not finish")
					}
					wantStatus, wantMessage := types.RunFailed, setupErr.Error()
					if provenance == "cancellation" {
						wantStatus, wantMessage = types.RunCancelled, cause.Error()
						if shutdown == "raw" {
							wantStatus, wantMessage = types.RunFailed, db.RawShutdownReason
						}
					}
					stored, err = d.GetRun(run.ID)
					if err != nil || stored.Status != wantStatus || stored.Error == nil || *stored.Error != wantMessage {
						t.Fatalf("settlement = %+v, %v; want %s %q", stored, err, wantStatus, wantMessage)
					}
				})
			}
		}
	}
}

func TestQueuedReadFailureSurvivesShutdownAndStorageRecovery(t *testing.T) {
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
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var armed, healthy atomic.Bool
	var attempts atomic.Int32
	readFunction, terminalFunction := "queued_read_"+newShutdownToken(), "queued_terminal_"+newShutdownToken()
	const original = "independent queued run read failure"
	if err := sqlite.RegisterScalarFunction(readFunction, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		if armed.CompareAndSwap(true, false) {
			cancel(pipeline.ErrDaemonShutdown)
			return nil, errors.New(original)
		}
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.RegisterScalarFunction(terminalFunction, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		if !healthy.Load() {
			attempts.Add(1)
			return nil, errors.New("terminal storage unavailable")
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
	if _, err := control.Exec(fmt.Sprintf(`ALTER TABLE runs RENAME TO saved_queue_runs;
CREATE VIEW runs AS SELECT * FROM saved_queue_runs WHERE %s()=1;
CREATE TRIGGER forward_queue_terminal INSTEAD OF UPDATE ON runs BEGIN
 UPDATE saved_queue_runs SET status=NEW.status,error=NEW.error,push_active=NEW.push_active,terminal_head_verified_at=NEW.terminal_head_verified_at,updated_at=NEW.updated_at WHERE id=OLD.id;
END;
CREATE TRIGGER fail_queue_terminal BEFORE UPDATE OF status ON saved_queue_runs WHEN NEW.status IN ('failed','cancelled') BEGIN SELECT %s(); END`, readFunction, terminalFunction)); err != nil {
		t.Fatal(err)
	}
	mgr := NewRunManager(d, p, nil)
	mgr.sched.limits = nil
	handle := &runHandle{runCtx: ctx, cancel: cancel, done: make(chan struct{})}
	mgr.runs[run.ID] = &managedRun{run: run, state: ownershipClaimed, handle: handle}
	slot, ok := mgr.sched.tryAdmit(run.ID)
	if !ok {
		t.Fatal("no slot")
	}
	armed.Store(true)
	done := make(chan struct{})
	go func() { mgr.launchQueuedRun(run.ID, slot, handle); close(done) }()
	defer func() {
		healthy.Store(true)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("queued cleanup did not finish")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for attempts.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no terminal retry")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mgr.mu.Lock()
	owned := mgr.runs[run.ID]
	mgr.mu.Unlock()
	if owned == nil || owned.handle != handle || mgr.sched.stats().Active != 1 {
		t.Fatal("read failure relinquished custody")
	}
	select {
	case <-handle.done:
		t.Fatal("read failure closed handle before settlement")
	default:
	}
	healthy.Store(true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("healthy storage did not settle queued read failure")
	}
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || !strings.Contains(*stored.Error, original) {
		t.Fatalf("queued cause overwritten: %+v, %v", stored, err)
	}
	mgr.mu.Lock()
	owned = mgr.runs[run.ID]
	mgr.mu.Unlock()
	if owned != nil || mgr.sched.stats().Active != 0 {
		t.Fatal("settled read failure retained custody")
	}
}
