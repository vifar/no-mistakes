package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A queued workload must not inherit another run's settlement failure.
//
// One run's terminal write can fail while the storage still serves every
// other write - a trigger that rejects exactly one run's terminal write. When
// the released slot admits the next run, that run's own write must still be
// attempted and must still succeed. The failing settlement belongs to the run
// that owns it.
func TestQueuedWorkloadDoesNotInheritAnotherRunsSettlementFailure(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo(t.TempDir(), "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	failing, err := d.InsertRun(repo.ID, "failing", "head-f", "base")
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.InsertRun(repo.ID, "other", "head-o", "base")
	if err != nil {
		t.Fatal(err)
	}

	// The storage rejects the failing run's terminal write and nothing else.
	// A trigger that aborts the statement needs no custom function, so it
	// applies to every connection to the database.
	control, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(fmt.Sprintf(`CREATE TRIGGER reject_one BEFORE UPDATE OF status ON runs WHEN NEW.id='%s' BEGIN SELECT RAISE(ABORT, 'live terminal storage unavailable'); END`, failing.ID)); err != nil {
		t.Fatal(err)
	}

	// The failing run's terminal write is queued, exactly as a run whose
	// outcome cannot be recorded leaves it.
	failed := make(chan error, 1)
	go func() {
		failed <- SettleTerminalWrite(d, func() error {
			return d.RecordIneligibleTerminal(failing.ID, "the storage refused this run", types.RunFailed, nil)
		})
	}()
	// Wait for the settlement to be queued.
	deadline := time.Now().Add(10 * time.Second)
	for {
		terminalSettlementMu.Lock()
		queued := len(terminalSettlements)
		terminalSettlementMu.Unlock()
		if queued > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the failing settlement was never queued")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The next run's terminal write is attempted and succeeds: the failing
	// settlement belongs to the run that owns it.
	written := make(chan error, 1)
	go func() {
		written <- WithCommitStatusLock(context.Background(), func() error {
			return d.RecordIneligibleTerminal(other.ID, "this run's own outcome", types.RunFailed, nil)
		})
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("the queued workload inherited another run's settlement failure: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the queued workload never got its own write")
	}
	stored, err := d.GetRun(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != "this run's own outcome" {
		t.Fatalf("the queued workload recorded another run's outcome: %+v", stored)
	}

	// The failing run's own settlement KEEPS WAITING: its run keeps its
	// custody until the outcome is durable, and the storage refuses it. That
	// wait is the failing run's own and must not become another run's
	// outcome.
	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("the failing run's settlement reported success")
		}
	case <-time.After(2 * time.Second):
	}

	// Leave the queue clean for the next test.
	terminalSettlementMu.Lock()
	terminalSettlements = nil
	rawPublicationSettlements = nil
	terminalSettlementMu.Unlock()
}
