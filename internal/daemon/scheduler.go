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
type slotScheduler struct {
	// limits reads the live machine limits. It is called outside mu on every
	// admission, yield and dispatch so a config edit applies to the next
	// scheduling decision without a daemon restart.
	limits func() (max int, hold bool)

	// start launches a queued run that has just been given a slot. The slot is
	// already held: the callee owns releasing it.
	start func(runID string, slot *runSlot) (func(), bool)

	// admitMu serialises admission so the in-memory queue order is exactly the
	// order of the queued_at stamps written to the database.
	admitMu sync.Mutex

	// refreshMu makes reading the live limits and applying them one step, so a
	// refresh that read the limits before a config edit cannot land after a
	// refresh that read the edited ones and restore the old policy.
	refreshMu sync.Mutex

	mu       *sync.Mutex
	max      int
	hold     bool
	active   int
	retrying bool
	// held keeps queued new runs from being claimed and launched. Daemon startup
	// holds the queue until IPC is serving: a claim moves a row from queued to
	// pending, and a startup that fails after that would leave a crashed-looking
	// pending run in place of its durable queued state.
	held bool
	// yielded counts live runs that gave their slot back for a wait (a parked
	// gate, the CI poll) and have not taken one again.
	yielded int
	// parkedHeld is the set of slots whose run is parked (or waiting on CI) but
	// kept its slot because hold_slot_while_parked was on when it tried to yield.
	// Turning the policy off releases them, so queued runs are not blocked by
	// gates that would now have yielded.
	parkedHeld map[*runSlot]struct{}
	prio       []*slotWaiter // resumed runs waiting for a slot, oldest first
	queued     []string      // new runs not yet started, oldest first
}

type slotWaiter struct {
	runID   string
	ready   chan struct{}
	granted bool
}

func newSlotScheduler(limits func() (int, bool), mu *sync.Mutex) *slotScheduler {
	return &slotScheduler{limits: limits, mu: mu}
}

func (s *slotScheduler) refresh() {
	if s.limits == nil {
		return
	}
	s.refreshMu.Lock()
	max, hold := s.limits()
	if max < 0 {
		max = 0
	}
	s.mu.Lock()
	releasing := s.hold && !hold
	s.max, s.hold = max, hold
	var parked []*runSlot
	if releasing {
		for slot := range s.parkedHeld {
			parked = append(parked, slot)
		}
	}
	s.mu.Unlock()
	s.refreshMu.Unlock()
	// hold_slot_while_parked was just switched off: gates already parked under
	// it never yield again on their own, so give their slots back now. Turning
	// it on needs no retroactive step - a run simply stops yielding.
	released := false
	for _, slot := range parked {
		released = slot.releaseParked() || released
	}
	if released {
		s.dispatch()
	}
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

// removeQueuedLocked drops a not-yet-started run from the start order and
// reports whether it was queued. The caller holds the manager mutex.
func (s *slotScheduler) removeQueuedLocked(runID string) bool {
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
			if s.retrying || s.held {
				s.mu.Unlock()
				return
			}
			id := s.queued[0]
			s.active++
			launch, claimed := s.start(id, &runSlot{s: s, runID: id, held: true})
			if claimed {
				s.queued = s.queued[1:]
			} else {
				s.active--
			}
			s.mu.Unlock()
			launch()
			if !claimed {
				return
			}
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
	// parkedHeld: Yield was asked for but skipped by the hold policy, so the run
	// is waiting while still holding its slot (see slotScheduler.parkedHeld).
	parkedHeld bool
	pending    *slotWaiter
	// flight is the one Acquire currently taking a slot for this run. Another
	// concurrent Acquire waits on it instead of taking a second slot.
	flight *acquireFlight
}

type acquireFlight struct {
	done chan struct{}
}

var _ interface {
	Reserve()
	Yield()
	Acquire(context.Context) error
} = (*runSlot)(nil)

// release frees a held slot. yield marks the run as alive-but-waiting (counted
// in slotStats.Yielded) rather than finished.
func (t *runSlot) release(yield bool) {
	t.mu.Lock()
	wasHeld, wasYielded := t.held, t.yielded
	var pending *slotWaiter
	if !yield {
		pending, t.pending = t.pending, nil
	}
	if pending == nil && !wasHeld && !(wasYielded && !yield) {
		t.mu.Unlock()
		return
	}
	t.parkedHeld = false
	t.held = false
	t.yielded = yield
	t.mu.Unlock()
	t.s.mu.Lock()
	delete(t.s.parkedHeld, t)
	if pending != nil {
		t.s.cancelWaiterLocked(pending)
	}
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
	if !t.markParkedHeld() {
		t.release(true)
	}
}

func (t *runSlot) markParkedHeld() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	if !t.s.hold {
		return false
	}
	if t.held {
		t.parkedHeld = true
		if t.s.parkedHeld == nil {
			t.s.parkedHeld = map[*runSlot]struct{}{}
		}
		t.s.parkedHeld[t] = struct{}{}
	}
	return true
}

// releaseParked gives back the slot of a run that is parked while holding it,
// as if its yield had not been skipped. It reports whether it released one; a
// run that resumed in the meantime is left alone.
func (t *runSlot) releaseParked() bool {
	t.mu.Lock()
	if !t.held || !t.parkedHeld {
		t.mu.Unlock()
		return false
	}
	t.parkedHeld, t.held, t.yielded = false, false, true
	t.mu.Unlock()
	t.s.mu.Lock()
	delete(t.s.parkedHeld, t)
	t.s.active--
	t.s.yielded++
	t.s.mu.Unlock()
	return true
}

func (t *runSlot) Reserve() {
	t.s.refresh()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	if t.held {
		t.parkedHeld = false
		delete(t.s.parkedHeld, t)
		return
	}
	if t.pending == nil && t.flight == nil {
		t.pending = t.s.requestSlotLocked(t.runID)
	}
}

// Acquire takes a slot again for a run that resumed work. It jumps ahead of
// every queued new run and waits behind earlier resumed runs. It is
// single-flight per run: a concurrent Acquire for the same run waits for the
// first one instead of taking a second slot, and takes over if that one is
// cancelled.
func (t *runSlot) Acquire(ctx context.Context) error {
	for {
		t.Reserve()
		t.mu.Lock()
		if t.held {
			t.mu.Unlock()
			return nil
		}
		if f := t.flight; f != nil {
			t.mu.Unlock()
			select {
			case <-f.done:
				continue
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		if t.pending == nil {
			t.mu.Unlock()
			continue
		}
		w := t.pending
		t.pending = nil
		f := &acquireFlight{done: make(chan struct{})}
		t.flight = f
		t.mu.Unlock()

		err := t.waitForSlot(ctx, w)

		t.mu.Lock()
		t.flight = nil
		t.mu.Unlock()
		close(f.done)
		return err
	}
}

func (s *slotScheduler) requestSlotLocked(runID string) *slotWaiter {
	w := &slotWaiter{runID: runID, ready: make(chan struct{})}
	if s.capacityLocked() && len(s.prio) == 0 {
		s.active++
		w.granted = true
		close(w.ready)
	} else {
		s.prio = append(s.prio, w)
	}
	return w
}

func (t *runSlot) waitForSlot(ctx context.Context, w *slotWaiter) error {
	s := t.s

	select {
	case <-w.ready:
		t.setHeld()
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		s.cancelWaiterLocked(w)
		s.mu.Unlock()
		s.dispatch()
		return context.Cause(ctx)
	}
}

func (s *slotScheduler) cancelWaiterLocked(w *slotWaiter) {
	if w.granted {
		s.active--
		return
	}
	for i, queued := range s.prio {
		if queued == w {
			s.prio = append(s.prio[:i], s.prio[i+1:]...)
			return
		}
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
