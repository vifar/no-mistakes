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

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"modernc.org/sqlite"
)

func holdShutdownPastDrainDeadline(t *testing.T, mgr *RunManager, d *db.DB) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() { mgr.Shutdown(); close(done) }()
	select {
	case <-done:
		t.Fatal("shutdown authorized teardown before durable settlement")
	case <-time.After(31 * time.Second):
	}
	if d.Closed() {
		t.Fatal("shutdown closed storage before durable settlement")
	}
	if _, err := d.GetActiveRuns(); err != nil {
		t.Fatalf("shutdown storage unavailable: %v", err)
	}
	return done
}

func awaitSettlementShutdown(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not finish after storage recovered")
	}
}

func TestShutdownWaitsForOrdinaryIndependentFailureSettlement(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, head := setupTestGitRepo(t, p, d, "shutdown-drain")
	var healthy atomic.Bool
	healthy.Store(true)
	var attempts atomic.Int32
	function := "shutdown_terminal_" + newShutdownToken()
	if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
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
	control, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(fmt.Sprintf(`CREATE TRIGGER fail_shutdown_terminal BEFORE UPDATE OF status ON runs WHEN NEW.status IN ('failed','cancelled') BEGIN SELECT %s(); END`, function)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_DEMO", "1")
	const original = "independent executing step failure"
	mgr := NewRunManager(d, p, func() []pipeline.Step {
		return []pipeline.Step{&shutdownWatchStep{execute: func(*pipeline.StepContext) (*pipeline.StepOutcome, error) { return nil, errors.New(original) }}}
	})
	defer func() { healthy.Store(true); mgr.Shutdown(); d.Close() }()
	healthy.Store(false)
	id, err := mgr.startRun(context.Background(), repo, "main", head, head, "test", nil, "intent", "", false, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for attempts.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("independent failure did not enter durable settlement")
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, err := d.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	done := holdShutdownPastDrainDeadline(t, mgr, d)
	if !testRunOwned(mgr, id) {
		t.Fatal("shutdown drain deadline released ordinary custody")
	}
	if _, err := os.Stat(run.WorktreePath()); err != nil {
		t.Fatalf("unsettled worktree removed: %v", err)
	}
	stored, err := d.GetRun(id)
	if err != nil || stored.Status.Terminal() {
		t.Fatalf("unsettled terminal truth: %+v, %v", stored, err)
	}
	healthy.Store(true)
	awaitSettlementShutdown(t, done)
	stored, err = d.GetRun(id)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || !strings.Contains(*stored.Error, original) {
		t.Fatalf("shutdown discarded original independent failure: %+v, %v", stored, err)
	}
	if testRunOwned(mgr, id) {
		t.Fatal("settled shutdown retained custody")
	}
	if _, err := os.Stat(run.WorktreePath()); !os.IsNotExist(err) {
		t.Fatalf("settled worktree remains: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err = reopened.GetRun(id)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || !strings.Contains(*stored.Error, original) {
		t.Fatalf("teardown lost durable cause: %+v, %v", stored, err)
	}
}
