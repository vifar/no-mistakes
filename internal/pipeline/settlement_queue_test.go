package pipeline

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// The settlement queues are global, and the production code serializes every
// drain with the commit status lock. The fixtures below do the same, so a
// drain never races the recovery worker and a teardown never removes an entry
// that a drain is about to pop.
//
// The lock is taken directly rather than through AcquireCommitStatusLock,
// because that helper DRAINS the queue as part of taking it - and a drain of
// an unhealthy settlement is exactly what these fixtures are exercising.
func withSettlementLock(t *testing.T, fn func()) {
	t.Helper()
	CommitStatusLock <- struct{}{}
	defer func() { <-CommitStatusLock }()
	fn()
}

// setTerminalSettlements replaces the queued terminal settlements under the
// lock, so the recovery worker never sees a half-written queue.
func setTerminalSettlements(t *testing.T, settlements []*publicationSettlement) {
	t.Helper()
	withSettlementLock(t, func() {
		terminalSettlementMu.Lock()
		terminalSettlements = settlements
		terminalSettlementMu.Unlock()
	})
}

// queuedTerminalSettlements reads the queue length under the lock.
func queuedTerminalSettlements(t *testing.T) int {
	t.Helper()
	count := 0
	withSettlementLock(t, func() {
		terminalSettlementMu.Lock()
		count = len(terminalSettlements)
		terminalSettlementMu.Unlock()
	})
	return count
}

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
	defer d.Close()
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
	withSettlementLock(t, func() {
		terminalSettlementMu.Lock()
		rawPublicationSettlements = []*publicationSettlement{{
			write: func() error {
				// A write against closed storage always fails.
				_, writeErr := closed.InsertRun("repo", "branch", "head", "base")
				return writeErr
			},
			done:        rawDone,
			unavailable: closed.Closed,
		}}
		terminalSettlementMu.Unlock()
	})
	t.Cleanup(func() {
		withSettlementLock(t, func() {
			terminalSettlementMu.Lock()
			rawPublicationSettlements = nil
			terminalSettlementMu.Unlock()
		})
	})

	// A terminal settlement queued behind it must still be attempted.
	terminalDone := make(chan error, 1)
	setTerminalSettlements(t, []*publicationSettlement{{
		write:       func() error { return nil },
		done:        terminalDone,
		unavailable: d.Closed,
	}})

	// The drain moves past the raw settlement and reaches the one behind it.
	// The raw settlement's own caller is told that its write did not land.
	withSettlementLock(t, func() {
		_ = drainPublicationSettlements()
	})
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
	withSettlementLock(t, func() {
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
	})
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
	setTerminalSettlements(t, []*publicationSettlement{{
		write:       func() error { return nil },
		done:        make(chan error, 1),
		unavailable: d.Closed,
	}})
	t.Cleanup(func() { setTerminalSettlements(t, nil) })
	withSettlementLock(t, func() {
		if err := drainPublicationSettlements(); err != nil {
			t.Fatalf("healthy drain reported an error: %v", err)
		}
	})
	if queued := queuedTerminalSettlements(t); queued != 0 {
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
	setTerminalSettlements(t, []*publicationSettlement{{
		write:       func() error { return errors.New("storage is busy") },
		done:        make(chan error, 1),
		unavailable: d.Closed,
	}})
	t.Cleanup(func() { setTerminalSettlements(t, nil) })
	withSettlementLock(t, func() {
		if err := drainPublicationSettlements(); err == nil {
			t.Fatal("unhealthy settlement reported success")
		}
	})
	if queued := queuedTerminalSettlements(t); queued != 1 {
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
	// The health flag is shared with the recovery worker, so it is atomic.
	var healthy atomic.Bool
	done := make(chan error, 1)
	setTerminalSettlements(t, []*publicationSettlement{{
		write: func() error {
			if !healthy.Load() {
				return errors.New("storage is busy")
			}
			return nil
		},
		done:        done,
		unavailable: d.Closed,
	}})
	t.Cleanup(func() { setTerminalSettlements(t, nil) })
	withSettlementLock(t, func() {
		if err := drainPublicationSettlements(); err == nil {
			t.Fatal("unhealthy settlement reported success")
		}
	})
	healthy.Store(true)
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
