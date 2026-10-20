package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// A raw publication settlement whose storage is gone for good must leave the
// queue. While it stayed, every later drain hit it first and returned its
// error, so the settlements queued BEHIND it were never attempted and their
// callers waited forever.
func TestRawSettlementWithGoneStorageDoesNotBlockTheQueue(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	// The raw settlement's write targets storage that is closed, so it can
	// never land.
	closed, err := db.OpenReadOnly(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	rawDone := make(chan error, 1)
	rawPublicationSettlements = nil
	terminalSettlementMu.Lock()
	rawPublicationSettlements = append(rawPublicationSettlements, &publicationSettlement{
		write: func() error {
			// A write against closed storage always fails.
			_, writeErr := closed.InsertRun("repo", "branch", "head", "base")
			return writeErr
		}, done: rawDone,
		unavailable: closed.Closed,
	})
	terminalSettlementMu.Unlock()
	t.Cleanup(func() {
		terminalSettlementMu.Lock()
		rawPublicationSettlements = nil
		terminalSettlementMu.Unlock()
	})

	// A terminal settlement queued behind it must still be attempted.
	terminalDone := make(chan error, 1)
	terminalSettlementMu.Lock()
	terminalSettlements = append(terminalSettlements, &publicationSettlement{
		write:       func() error { return nil },
		done:        terminalDone,
		unavailable: d.Closed,
	})
	terminalSettlementMu.Unlock()
	t.Cleanup(func() {
		terminalSettlementMu.Lock()
		terminalSettlements = nil
		terminalSettlementMu.Unlock()
	})

	// The drain moves past the raw settlement and reaches the one behind it.
	_ = drainPublicationSettlements()
	// The raw settlement's own caller is told that its write did not land.
	select {
	case err := <-rawDone:
		if err == nil {
			t.Fatal("raw settlement with gone storage reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("raw settlement with gone storage never released its caller")
	}
	// The settlement behind it is reached, not starved by the one in front.
	select {
	case err := <-terminalDone:
		if err != nil {
			t.Fatalf("terminal settlement behind a gone raw settlement failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal settlement behind a gone raw settlement was never attempted")
	}
	terminalSettlementMu.Lock()
	queuedRaw := len(rawPublicationSettlements)
	queuedTerminal := len(terminalSettlements)
	terminalSettlementMu.Unlock()
	if queuedRaw != 0 {
		t.Fatalf("raw settlement with gone storage stayed queued: %d", queuedRaw)
	}
	if queuedTerminal != 0 {
		t.Fatalf("terminal settlement stayed queued: %d", queuedTerminal)
	}
}

// The drain of a queue whose only settlement is a healthy write leaves the
// queue empty and reports success, so the recovery loop can stop.
func TestDrainOfHealthySettlementsEmptiesTheQueue(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	terminalSettlementMu.Lock()
	terminalSettlements = append(terminalSettlements, &publicationSettlement{
		write:       func() error { return nil },
		done:        make(chan error, 1),
		unavailable: d.Closed,
	})
	terminalSettlementMu.Unlock()
	t.Cleanup(func() {
		terminalSettlementMu.Lock()
		terminalSettlements = nil
		terminalSettlementMu.Unlock()
	})
	if err := drainPublicationSettlements(); err != nil {
		t.Fatalf("healthy drain reported an error: %v", err)
	}
	terminalSettlementMu.Lock()
	queued := len(terminalSettlements)
	terminalSettlementMu.Unlock()
	if queued != 0 {
		t.Fatalf("healthy settlement stayed queued: %d", queued)
	}
}

// A settlement whose write fails against storage that is merely unhealthy
// stays queued: the recovery loop is what retries it, and its caller keeps
// waiting for the outcome to become durable.
func TestUnhealthySettlementStaysQueuedForTheRecoveryLoop(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	terminalSettlementMu.Lock()
	terminalSettlements = append(terminalSettlements, &publicationSettlement{
		write:       func() error { return errors.New("storage is busy") },
		done:        make(chan error, 1),
		unavailable: d.Closed,
	})
	terminalSettlementMu.Unlock()
	t.Cleanup(func() {
		terminalSettlementMu.Lock()
		terminalSettlements = nil
		terminalSettlementMu.Unlock()
	})
	if err := drainPublicationSettlements(); err == nil {
		t.Fatal("unhealthy settlement reported success")
	}
	terminalSettlementMu.Lock()
	queued := len(terminalSettlements)
	terminalSettlementMu.Unlock()
	if queued != 1 {
		t.Fatalf("unhealthy settlement left the queue: %d", queued)
	}
}

// The recovery loop retries a queued settlement, so a settlement that becomes
// healthy later still lands.
func TestRecoveryLoopRetriesAQueuedSettlement(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	healthy := false
	done := make(chan error, 1)
	terminalSettlementMu.Lock()
	terminalSettlements = append(terminalSettlements, &publicationSettlement{
		write: func() error {
			if !healthy {
				return errors.New("storage is busy")
			}
			return nil
		},
		done:        done,
		unavailable: d.Closed,
	})
	terminalSettlementMu.Unlock()
	t.Cleanup(func() {
		terminalSettlementMu.Lock()
		terminalSettlements = nil
		terminalSettlementMu.Unlock()
	})
	if err := drainPublicationSettlements(); err == nil {
		t.Fatal("unhealthy settlement reported success")
	}
	healthy = true
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("retried settlement failed: %v", err)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the recovery loop never retried the queued settlement")
		}
		// Nudge the loop the way the real code paths do.
		_ = AcquireCommitStatusLock(context.Background())
		ReleaseCommitStatusLock()
		time.Sleep(100 * time.Millisecond)
	}
}
