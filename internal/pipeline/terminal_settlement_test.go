package pipeline

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"modernc.org/sqlite"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTerminalSettlement_LockReleaseCompletesOwnedDuplicateWrites(t *testing.T) {
	d, p, run, _ := setupTest(t)
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := AcquireCommitStatusLock(context.Background()); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(ReleaseCommitStatusLock) }
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- RecordRunFailure(ctx, d, run.ID, "independent failure", nil) }()
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-results:
		t.Fatalf("settlement abandoned its owner: %v", err)
	default:
	}
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunRunning {
		t.Fatalf("terminal write crossed held lock: %+v,%v", stored, err)
	}
	release()
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("released lock did not settle owned failure")
		}
	}
	reopened, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err = reopened.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != "independent failure" {
		t.Fatalf("reopened settlement: %+v,%v", stored, err)
	}
}

func TestTerminalSettlement_StorageErrorDoesNotEmitCommittedEvent(t *testing.T) {
	d, p, run, repo := setupTest(t)
	before := run.Status
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	terminal := false
	executor := NewExecutor(d, p, nil, nil, nil, func(event ipc.Event) {
		if event.Type == ipc.EventRunCompleted {
			terminal = true
		}
	})
	failure := errors.New("independent failure")
	err := executor.FailRun(run, repo, failure)
	if !errors.Is(err, failure) || err == failure {
		t.Errorf("storage failure not propagated with original identity: %v", err)
	}
	if terminal || run.Status != before {
		t.Fatalf("failed storage produced a committed terminal outcome: event=%v,status=%s", terminal, run.Status)
	}
}

func TestTerminalSettlement_CompletionCannotOverwriteCommittedFailure(t *testing.T) {
	d, p, run, repo := setupTest(t)
	if err := RecordRunFailure(context.Background(), d, run.ID, "daemon shutting down", nil); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(d, p, nil, nil, nil, nil)
	if err := executor.completeRun(context.Background(), run, repo); err == nil {
		t.Error("completion claimed success after committed failure")
	}
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != "daemon shutting down" || run.Status != types.RunFailed {
		t.Fatalf("completion replaced raw terminal decision: %+v,%v", stored, err)
	}
}

func TestTerminalSettlement_StartupCannotResurrectTerminalRun(t *testing.T) {
	d, p, run, repo := setupTest(t)
	if err := RecordRunFailure(context.Background(), d, run.ID, "independent failure", nil); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(d, p, nil, nil, nil, nil)
	_ = executor.Execute(context.Background(), run, repo, t.TempDir())
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != "independent failure" {
		t.Fatalf("startup resurrected committed failure: %+v,%v", stored, err)
	}
}

func TestTerminalSettlement_RegisteredStartupAfterRawSupersession(t *testing.T) {
	d, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	if err := RecordRawShutdown(context.Background(), d, []string{run.ID}, "runtime", func() { cancel(errors.New(db.RawShutdownReason)) }); err != nil {
		t.Fatal(err)
	}
	step := newPassStep(types.StepCI)
	executor := NewExecutor(d, p, nil, nil, []Step{step}, nil)
	_ = executor.Execute(ctx, run, repo, workDir)
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != db.RawShutdownReason || step.callCount() != 0 {
		t.Fatalf("registered executor escaped raw supersession: %+v,%v,calls=%d", stored, err, step.callCount())
	}
}

func TestTerminalSettlement_RawStorageRecoveryFencesCapturedBatch(t *testing.T) {
	d, p, first, _ := setupTest(t)
	second, err := d.InsertRun(first.RepoID, "other", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{first.ID, second.ID}
	for _, id := range ids {
		if err := d.UpdateRunStatus(id, types.RunRunning); err != nil {
			t.Fatal(err)
		}
	}
	var healthy atomic.Bool
	queued := make(chan struct{})
	var queuedOnce sync.Once
	function := "raw_storage_" + first.ID
	if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		if !healthy.Load() {
			terminalSettlementMu.Lock()
			ready := len(terminalSettlements) >= 2
			terminalSettlementMu.Unlock()
			if ready {
				queuedOnce.Do(func() { close(queued) })
			}
			return nil, errors.New("raw storage unavailable")
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
	control, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(fmt.Sprintf(`CREATE TRIGGER raw_storage BEFORE INSERT ON controlled_shutdown_runs WHEN NEW.reason='daemon shutting down' BEGIN SELECT %s(); END`, function)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		healthy.Store(true)
		_ = WithCommitStatusLock(context.Background(), func() error { return nil })
	}()
	if err := RecordRawShutdown(context.Background(), d, ids, "runtime", func() {}); err == nil {
		t.Fatal("raw storage error swallowed")
	}
	invoked := false
	for range 3 {
		if err := WithCommitStatusLock(context.Background(), func() error { invoked = true; return nil }); err == nil {
			t.Fatal("candidate crossed failed raw fence")
		}
	}
	if invoked {
		t.Fatal("preservation candidate admitted on unchanged eligibility")
	}
	for _, id := range ids {
		run, err := d.GetRun(id)
		if err != nil || run.Status != types.RunRunning {
			t.Fatalf("raw failure preempted executor truth: %+v,%v", run, err)
		}
	}
	results := make(chan error, 2)
	for _, id := range ids {
		go func(id string) {
			results <- RecordRunFailure(context.Background(), d, id, "independent provider failure", nil)
		}(id)
	}
	select {
	case <-queued:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal owners never reached retained custody")
	}
	healthy.Store(true)
	for range ids {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("storage recovery needed an outsider to acquire the publication lock")
		}
	}
	reopened, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, id := range ids {
		run, err := reopened.GetRun(id)
		if err != nil || run.Status != types.RunFailed || run.Error == nil || *run.Error != "independent provider failure" {
			t.Fatalf("reopened captured failure: %+v,%v", run, err)
		}
	}
}

func TestTerminalSettlement_TerminalStorageRecoversWithoutNudge(t *testing.T) {
	d, p, run, repo := setupTest(t)
	if err := RecordRawShutdown(context.Background(), d, []string{run.ID}, "runtime", func() {}); err != nil {
		t.Fatal(err)
	}
	var healthy atomic.Bool
	attempts := make(chan struct{}, 8)
	function := "terminal_storage_" + run.ID
	if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		if !healthy.Load() {
			select {
			case attempts <- struct{}{}:
			default:
			}
			return nil, errors.New("terminal storage unavailable")
		}
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	var err error
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
	if _, err := control.Exec(fmt.Sprintf("CREATE TRIGGER terminal_storage BEFORE UPDATE OF status ON runs WHEN NEW.status='failed' BEGIN SELECT %s(); END", function)); err != nil {
		t.Fatal(err)
	}
	defer healthy.Store(true)
	events := make(chan ipc.Event, 8)
	executor := NewExecutor(d, p, nil, nil, nil, func(event ipc.Event) {
		if event.Type == ipc.EventRunCompleted {
			events <- event
		}
	})
	failure := errors.New("independent provider failure")
	done := make(chan error, 1)
	go func() { done <- executor.FailRun(run, repo, failure) }()
	for range 2 {
		select {
		case <-attempts:
		case <-time.After(5 * time.Second):
			t.Fatal("terminal storage did not fail repeatedly")
		}
	}
	select {
	case err := <-done:
		t.Fatalf("terminal owner abandoned failed storage: %v", err)
	default:
	}
	healthy.Store(true)
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatalf("original error lost: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("healthy storage did not resume owned terminal settlement")
	}
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("settlement event absent")
	}
	select {
	case <-events:
		t.Fatal("duplicate terminal event")
	default:
	}
	reopened, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != failure.Error() {
		t.Fatalf("reopened terminal result: %+v,%v", stored, err)
	}
}

func TestTerminalSettlement_CommittedReadbackRecoversWithoutRewriting(t *testing.T) {
	d, p, run, repo := setupTest(t)
	if err := RecordRunFailure(context.Background(), d, run.ID, "actual panic", nil); err != nil {
		t.Fatal(err)
	}
	var healthy atomic.Bool
	attempts := make(chan struct{}, 8)
	function := "terminal_read_" + run.ID
	if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		if !healthy.Load() {
			select {
			case attempts <- struct{}{}:
			default:
			}
			return nil, errors.New("readback unavailable")
		}
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	var err error
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
	if _, err := control.Exec("ALTER TABLE runs RENAME TO settled_runs"); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Exec(fmt.Sprintf("CREATE VIEW runs AS SELECT * FROM settled_runs WHERE %s()=1", function)); err != nil {
		t.Fatal(err)
	}
	defer healthy.Store(true)
	events := make(chan ipc.Event, 2)
	executor := NewExecutor(d, p, nil, nil, nil, func(event ipc.Event) {
		if event.Type == ipc.EventRunCompleted {
			events <- event
		}
	})
	done := make(chan error, 1)
	go func() { done <- executor.ReportSettledRunFailure(context.Background(), run, repo) }()
	for range 2 {
		select {
		case <-attempts:
		case <-time.After(5 * time.Second):
			t.Fatal("readback fault not reached")
		}
	}
	select {
	case err := <-done:
		t.Fatalf("readback owner exited: %v", err)
	default:
	}
	healthy.Store(true)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("readback continuation stalled")
	}
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("missing terminal event")
	}
	select {
	case <-events:
		t.Fatal("duplicate terminal event")
	default:
	}
	if _, err := control.Exec("DROP VIEW runs"); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Exec("ALTER TABLE settled_runs RENAME TO runs"); err != nil {
		t.Fatal(err)
	}
	reopened, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.GetRun(run.ID)
	if err != nil || stored.Error == nil || *stored.Error != "actual panic" {
		t.Fatalf("readback changed terminal reason: %+v,%v", stored, err)
	}
}

func TestTerminalSettlement_ClosedOwnerFailsBehindRawStorageFault(t *testing.T) {
	rawDB, p, run, _ := setupTest(t)
	var healthy atomic.Bool
	function := "closed_owner_raw_" + run.ID
	if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		if !healthy.Load() {
			return nil, errors.New("raw storage unavailable")
		}
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	rawDB.Close()
	var err error
	rawDB, err = db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	defer healthy.Store(true)
	control, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(fmt.Sprintf(`CREATE TRIGGER closed_owner_raw BEFORE INSERT ON controlled_shutdown_runs BEGIN SELECT %s(); END`, function)); err != nil {
		t.Fatal(err)
	}
	if err := RecordRawShutdown(context.Background(), rawDB, []string{run.ID}, "runtime", func() {}); err == nil {
		t.Fatal("raw fault not injected")
	}
	ownerDB, _, _, _ := setupTest(t)
	done := make(chan error, 1)
	go func() {
		done <- SettleTerminalWrite(ownerDB, func() error { t.Error("closed owner callback ran past raw fault"); return nil })
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		terminalSettlementMu.Lock()
		queued := len(terminalSettlements) > 0
		terminalSettlementMu.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal continuation not registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := ownerDB.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed owner reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closed owner hung behind raw storage fault")
	}
	healthy.Store(true)
	deadline = time.Now().Add(5 * time.Second)
	for {
		terminalSettlementMu.Lock()
		pending := len(rawPublicationSettlements) > 0
		terminalSettlementMu.Unlock()
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("raw storage did not recover without a second request")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
