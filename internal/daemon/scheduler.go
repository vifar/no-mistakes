package daemon

import (
	"context"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// slotScheduler enforces daemon.max_concurrent_runs. It owns only counters and
// ordering; the durable truth for a run that has not started is its 'queued'
// row, which the manager reloads at startup.
//
// Start order is: runs that resumed from a parked gate (priority waiters,
// first come first served), then new queued runs first in first out. A run
// holds a slot only while it executes pipeline work (see pipeline.RunSlot).
//
// Nothing here blocks on pipeline work and no method holds mu across I/O, so
// the control plane (status, respond, abort, gate_context, launch) can call in
// at any time.
type slotScheduler struct {
	// limits reads the live machine limits. It is called outside mu on every
	// admission, yield and dispatch so a config edit applies to the next
	// scheduling decision without a daemon restart.
	limits func() (max int, hold bool)

	// start launches a queued run that has just been given a slot. The slot is
	// already held: the callee owns releasing it.
	start func(runID string, slot *runSlot)

	// admitMu serialises admission so the in-memory queue order is exactly the
	// order of the queued_at stamps written to the database.
	admitMu sync.Mutex

	mu     sync.Mutex
	max    int
	hold   bool
	active int
	// yielded counts live runs that gave their slot back for a wait (a parked
	// gate, the CI poll) and have not taken one again.
	yielded int
	prio    []*slotWaiter // resumed runs waiting for a slot, oldest first
	queued  []string      // new runs not yet started, oldest first
}

type slotWaiter struct {
	runID   string
	ready   chan struct{}
	granted bool
}

func newSlotScheduler(limits func() (int, bool)) *slotScheduler {
	return &slotScheduler{limits: limits}
}

func (s *slotScheduler) refresh() {
	if s.limits == nil {
		return
	}
	max, hold := s.limits()
	if max < 0 {
		max = 0
	}
	s.mu.Lock()
	s.max, s.hold = max, hold
	s.mu.Unlock()
}

func (s *slotScheduler) capacityLocked() bool { return s.max == 0 || s.active < s.max }

// tryAdmit grants a slot to a new run only when capacity is free and nothing
// is waiting ahead of it, so a launch can never jump the queue.
func (s *slotScheduler) tryAdmit(runID string) (*runSlot, bool) {
	s.refresh()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.capacityLocked() || len(s.prio) > 0 || len(s.queued) > 0 {
		return nil, false
	}
	s.active++
	return &runSlot{s: s, runID: runID, held: true}, true
}

// forceAcquire gives a recovered run its slot even beyond the cap. Runs that
// were already executing when the daemon restarted are never queued behind
// new work; they count against the cap so new launches wait for them.
func (s *slotScheduler) forceAcquire(runID string) *runSlot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active++
	return &runSlot{s: s, runID: runID, held: true}
}

// enqueue appends a run to the start order and starts whatever now fits.
func (s *slotScheduler) enqueue(runID string) {
	s.mu.Lock()
	s.queued = append(s.queued, runID)
	s.mu.Unlock()
	s.dispatch()
}

// restoreQueue seeds the start order from the database at startup. ids must
// already be in start order.
func (s *slotScheduler) restoreQueue(ids []string) {
	s.mu.Lock()
	s.queued = append(s.queued[:0], ids...)
	s.mu.Unlock()
}

// removeQueued drops a not-yet-started run from the start order. It reports
// whether the run was still queued; false means it was already dispatched.
func (s *slotScheduler) removeQueued(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, id := range s.queued {
		if id == runID {
			s.queued = append(s.queued[:i], s.queued[i+1:]...)
			return true
		}
	}
	return false
}

// dispatch hands free slots to waiters: resumed runs first, then the oldest
// queued new run.
func (s *slotScheduler) dispatch() {
	s.refresh()
	for {
		s.mu.Lock()
		if !s.capacityLocked() {
			s.mu.Unlock()
			return
		}
		if len(s.prio) > 0 {
			w := s.prio[0]
			s.prio = s.prio[1:]
			s.active++
			w.granted = true
			close(w.ready)
			s.mu.Unlock()
			continue
		}
		if len(s.queued) > 0 {
			id := s.queued[0]
			s.queued = s.queued[1:]
			s.active++
			s.mu.Unlock()
			s.start(id, &runSlot{s: s, runID: id, held: true})
			continue
		}
		s.mu.Unlock()
		return
	}
}

// annotate stamps queue state onto a run snapshot: a new queued run gets its
// place in the start order (behind every resumed run), and a resumed run that
// is waiting for a slot is flagged and given its place too.
func (s *slotScheduler) annotate(info *ipc.RunInfo) {
	if s == nil || info == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	total := len(s.prio) + len(s.queued)
	for i, w := range s.prio {
		if w.runID == info.ID {
			info.QueuePosition, info.QueueTotal, info.WaitingForSlot = i+1, total, true
			return
		}
	}
	for i, id := range s.queued {
		if id == info.ID {
			info.QueuePosition, info.QueueTotal = len(s.prio)+i+1, total
			return
		}
	}
}

// slotStats is a point-in-time view of the scheduler for status surfaces.
type slotStats struct {
	Max     int  // daemon.max_concurrent_runs (0 = unlimited)
	Hold    bool // daemon.hold_slot_while_parked
	Active  int  // slots in use
	Queued  int  // runs waiting for a slot (new queued runs plus resumed runs)
	Yielded int  // live runs that released their slot for a wait
}

func (s *slotScheduler) stats() slotStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slotStats{Max: s.max, Hold: s.hold, Active: s.active, Queued: len(s.queued) + len(s.prio), Yielded: s.yielded}
}

// runSlot is one run's claim on a scheduler slot. It is safe for concurrent
// use, and every method is idempotent.
type runSlot struct {
	s     *slotScheduler
	runID string

	mu      sync.Mutex
	held    bool
	yielded bool // gave the slot back for a wait and is still alive
}

var _ interface {
	Yield()
	Acquire(context.Context) error
} = (*runSlot)(nil)

// release frees a held slot. yield marks the run as alive-but-waiting (counted
// in slotStats.Yielded) rather than finished.
func (t *runSlot) release(yield bool) {
	t.mu.Lock()
	wasHeld, wasYielded := t.held, t.yielded
	if !wasHeld && !(wasYielded && !yield) {
		t.mu.Unlock()
		return
	}
	t.held = false
	t.yielded = yield
	t.mu.Unlock()
	t.s.mu.Lock()
	if wasHeld {
		t.s.active--
	}
	switch {
	case yield && !wasYielded:
		t.s.yielded++
	case !yield && wasYielded:
		t.s.yielded--
	}
	t.s.mu.Unlock()
	t.s.dispatch()
}

// Release gives the slot back for good. The run goroutine defers it, so a
// completion, failure, cancellation, abort or panic all free the slot.
func (t *runSlot) Release() { t.release(false) }

// Yield gives the slot back across a wait that does no work, unless
// daemon.hold_slot_while_parked asks for the old hold-through behaviour.
func (t *runSlot) Yield() {
	t.s.refresh()
	t.s.mu.Lock()
	hold := t.s.hold
	t.s.mu.Unlock()
	if hold {
		return
	}
	t.release(true)
}

// Acquire takes a slot again for a run that resumed work. It jumps ahead of
// every queued new run and waits behind earlier resumed runs.
func (t *runSlot) Acquire(ctx context.Context) error {
	t.mu.Lock()
	held := t.held
	t.mu.Unlock()
	if held {
		return nil
	}
	s := t.s
	s.refresh()
	s.mu.Lock()
	if s.capacityLocked() && len(s.prio) == 0 {
		s.active++
		s.mu.Unlock()
		t.setHeld()
		return nil
	}
	w := &slotWaiter{runID: t.runID, ready: make(chan struct{})}
	s.prio = append(s.prio, w)
	s.mu.Unlock()

	select {
	case <-w.ready:
		t.setHeld()
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		if w.granted {
			// Granted in the same instant the wait was cancelled: hand it back.
			s.active--
			s.mu.Unlock()
			s.dispatch()
			return context.Cause(ctx)
		}
		for i, queued := range s.prio {
			if queued == w {
				s.prio = append(s.prio[:i], s.prio[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		return context.Cause(ctx)
	}
}

func (t *runSlot) setHeld() {
	t.mu.Lock()
	wasYielded := t.yielded
	t.held, t.yielded = true, false
	t.mu.Unlock()
	if wasYielded {
		t.s.mu.Lock()
		t.s.yielded--
		t.s.mu.Unlock()
	}
}
