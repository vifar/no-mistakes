package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// newTestScheduler returns a scheduler with fixed limits whose start callback
// records dispatched run ids. Dispatched slots are kept so tests can release
// them like a finishing run would.
type schedHarness struct {
	*slotScheduler
	mu      sync.Mutex
	started []string
	slots   map[string]*runSlot
	max     int
	hold    bool
}

func newSchedHarness(max int, hold bool) *schedHarness {
	h := &schedHarness{slots: map[string]*runSlot{}, max: max, hold: hold}
	h.slotScheduler = newSlotScheduler(func() (int, bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.max, h.hold
	}, &sync.Mutex{})
	h.start = func(id string, slot *runSlot) (func(), bool) {
		h.mu.Lock()
		h.started = append(h.started, id)
		h.slots[id] = slot
		h.mu.Unlock()
		return func() {}, true
	}
	return h
}

func (h *schedHarness) setLimits(max int, hold bool) {
	h.mu.Lock()
	h.max, h.hold = max, hold
	h.mu.Unlock()
}

func (h *schedHarness) startedIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.started...)
}

// removeQueued mirrors what the manager does under its mutex.
func (h *schedHarness) removeQueued(id string) bool {
	h.slotScheduler.mu.Lock()
	defer h.slotScheduler.mu.Unlock()
	return h.removeQueuedLocked(id)
}

func (h *schedHarness) slot(id string) *runSlot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.slots[id]
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSchedulerDispatchCancellationHandoff(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, head := setupTestGitRepo(t, p, database, "handoff")
	for i := 0; i < 40; i++ {
		run, err := database.InsertRun(repo.ID, "feature", head, head)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.MarkRunQueued(run.ID, ""); err != nil {
			t.Fatal(err)
		}
		m := NewRunManager(database, p, nil)
		m.sched = newSlotScheduler(func() (int, bool) { return 1, false }, &m.mu)
		preparing := make(chan struct{})
		proceed := make(chan struct{})
		registered := make(chan struct{})
		m.sched.start = func(id string, slot *runSlot) (func(), bool) {
			close(preparing)
			<-proceed
			m.startQueuedRun(id, slot)
			close(registered)
			return func() {}, true
		}
		m.runs[run.ID] = &managedRun{run: run, state: ownershipQueued}
		m.sched.restoreQueue([]string{run.ID})
		dispatched := make(chan struct{})
		go func() { m.sched.dispatch(); close(dispatched) }()
		<-preparing
		cancelled := make(chan error, 1)
		go func() { cancelled <- m.HandleCancel(run.ID) }()
		select {
		case err := <-cancelled:
			t.Errorf("cancellation crossed the unregistered handoff: %v", err)
		case <-time.After(time.Millisecond):
		}
		close(proceed)
		<-registered
		<-dispatched
		select {
		case err := <-cancelled:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancellation did not find the dispatched run")
		}
		m.wg.Done()
	}
}

func position(h *schedHarness, id string) (int, int, bool) {
	info := &ipc.RunInfo{ID: id}
	h.annotate(info)
	return info.QueuePosition, info.QueueTotal, info.WaitingForSlot
}

func TestSchedulerCapAdmitsThenQueuesFIFO(t *testing.T) {
	h := newSchedHarness(2, false)
	var admitted []*runSlot
	for _, id := range []string{"r1", "r2"} {
		slot, ok := h.tryAdmit(id)
		if !ok {
			t.Fatalf("%s should be admitted under the cap", id)
		}
		admitted = append(admitted, slot)
	}
	for _, id := range []string{"r3", "r4"} {
		if _, ok := h.tryAdmit(id); ok {
			t.Fatalf("%s must not be admitted at the cap", id)
		}
		h.enqueue(id)
	}
	if pos, total, _ := position(h, "r3"); pos != 1 || total != 2 {
		t.Fatalf("r3 position = %d of %d, want 1 of 2", pos, total)
	}
	if pos, total, _ := position(h, "r4"); pos != 2 || total != 2 {
		t.Fatalf("r4 position = %d of %d, want 2 of 2", pos, total)
	}
	if got := h.startedIDs(); len(got) != 0 {
		t.Fatalf("queued runs started early: %v", got)
	}

	admitted[1].Release()
	if got := h.startedIDs(); !equalIDs(got, []string{"r3"}) {
		t.Fatalf("started after first release = %v, want [r3]", got)
	}
	if pos, total, _ := position(h, "r4"); pos != 1 || total != 1 {
		t.Fatalf("r4 position after r3 started = %d of %d, want 1 of 1", pos, total)
	}
	admitted[0].Release()
	if got := h.startedIDs(); !equalIDs(got, []string{"r3", "r4"}) {
		t.Fatalf("started after second release = %v, want [r3 r4]", got)
	}
}

func TestSchedulerLaunchCannotJumpTheQueue(t *testing.T) {
	h := newSchedHarness(1, false)
	first, _ := h.tryAdmit("r1")
	h.enqueue("r2")
	first.Release() // r2 takes the freed slot
	if _, ok := h.tryAdmit("r3"); ok {
		t.Fatal("a new launch took a slot while the queue's head held it")
	}
}

func TestSchedulerUnlimitedWhenCapIsZero(t *testing.T) {
	h := newSchedHarness(0, false)
	for i := 0; i < 50; i++ {
		if _, ok := h.tryAdmit("r"); !ok {
			t.Fatalf("launch %d queued with cap 0", i)
		}
	}
}

func TestSchedulerParkedRunReleasesSlotAndResumedRunHasPriority(t *testing.T) {
	h := newSchedHarness(1, false)
	parked, _ := h.tryAdmit("parked")
	h.enqueue("new1")
	if got := h.startedIDs(); len(got) != 0 {
		t.Fatalf("new1 started while the cap was full: %v", got)
	}

	parked.Yield() // run parks at a gate
	if got := h.startedIDs(); !equalIDs(got, []string{"new1"}) {
		t.Fatalf("a parked run must free its slot for the queue; started = %v", got)
	}
	h.enqueue("new2")

	acquired := make(chan error, 1)
	go func() { acquired <- parked.Acquire(context.Background()) }()
	waitForSchedCondition(t, func() bool { _, _, waiting := position(h, "parked"); return waiting })
	if pos, total, waiting := position(h, "parked"); !waiting || pos != 1 || total != 2 {
		t.Fatalf("resumed run = position %d of %d waiting=%v, want 1 of 2 waiting", pos, total, waiting)
	}
	if pos, _, _ := position(h, "new2"); pos != 2 {
		t.Fatalf("new2 position = %d, want 2 (behind the resumed run)", pos)
	}

	h.slot("new1").Release()
	if err := <-acquired; err != nil {
		t.Fatalf("resumed run Acquire: %v", err)
	}
	if got := h.startedIDs(); !equalIDs(got, []string{"new1"}) {
		t.Fatalf("new2 started before the resumed run: %v", got)
	}
	parked.Release()
	if got := h.startedIDs(); !equalIDs(got, []string{"new1", "new2"}) {
		t.Fatalf("started = %v, want new2 after the resumed run finished", got)
	}
}

func TestSchedulerResumedRunsAreServedInArrivalOrder(t *testing.T) {
	h := newSchedHarness(1, false)
	blocker, _ := h.tryAdmit("blocker")
	a := &runSlot{s: h.slotScheduler, runID: "a"}
	b := &runSlot{s: h.slotScheduler, runID: "b"}
	order := make(chan string, 2)
	go func() { _ = a.Acquire(context.Background()); order <- "a" }()
	waitForSchedCondition(t, func() bool { _, _, w := position(h, "a"); return w })
	go func() { _ = b.Acquire(context.Background()); order <- "b" }()
	waitForSchedCondition(t, func() bool { _, _, w := position(h, "b"); return w })
	blocker.Release()
	if got := <-order; got != "a" {
		t.Fatalf("first resumed run to get a slot = %s, want a", got)
	}
	a.Release()
	if got := <-order; got != "b" {
		t.Fatalf("second resumed run to get a slot = %s, want b", got)
	}
}

func TestSchedulerHoldSlotWhileParkedKeepsTheSlot(t *testing.T) {
	h := newSchedHarness(1, true)
	parked, _ := h.tryAdmit("parked")
	h.enqueue("new1")
	parked.Yield()
	if got := h.startedIDs(); len(got) != 0 {
		t.Fatalf("hold_slot_while_parked must keep the slot; started = %v", got)
	}
	if err := parked.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire on a held slot: %v", err)
	}
	parked.Release()
	if got := h.startedIDs(); !equalIDs(got, []string{"new1"}) {
		t.Fatalf("started after release = %v, want [new1]", got)
	}
}

func TestSchedulerLimitsApplyLive(t *testing.T) {
	h := newSchedHarness(1, false)
	h.tryAdmit("r1")
	h.enqueue("r2")
	h.enqueue("r3")
	h.setLimits(3, false) // operator raises the cap
	h.dispatch()
	if got := h.startedIDs(); !equalIDs(got, []string{"r2", "r3"}) {
		t.Fatalf("started after cap raise = %v, want [r2 r3]", got)
	}
}

func TestSchedulerRecoveredParkedRunsRespectCapAndPriority(t *testing.T) {
	h := newSchedHarness(1, true)
	a := &runSlot{s: h.slotScheduler, runID: "recovered-a"}
	b := &runSlot{s: h.slotScheduler, runID: "recovered-b"}
	a.Reserve()
	b.Reserve()
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.Yield()
	h.enqueue("new")
	if st := h.stats(); st.Active != 1 || st.Queued != 2 {
		t.Fatalf("recovery stats = %+v", st)
	}
	if pos, _, waiting := position(h, b.runID); pos != 1 || !waiting {
		t.Fatalf("second recovered run position = %d, waiting = %v", pos, waiting)
	}
	a.Release()
	if err := b.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.Yield()
	if got := h.startedIDs(); len(got) != 0 {
		t.Fatalf("new run started ahead of recovered parked run: %v", got)
	}
	if st := h.stats(); st.Active != 1 {
		t.Fatalf("recovery stats = %+v", st)
	}
	b.Release()
	if got := h.startedIDs(); !equalIDs(got, []string{"new"}) {
		t.Fatalf("started = %v, want [new]", got)
	}
}

func TestSchedulerRemoveQueuedShiftsPositions(t *testing.T) {
	h := newSchedHarness(1, false)
	first, _ := h.tryAdmit("r1")
	for _, id := range []string{"q1", "q2", "q3"} {
		h.enqueue(id)
	}
	if !h.removeQueued("q2") {
		t.Fatal("q2 should have been queued")
	}
	if h.removeQueued("q2") {
		t.Fatal("q2 removed twice")
	}
	if pos, total, _ := position(h, "q3"); pos != 2 || total != 2 {
		t.Fatalf("q3 position = %d of %d, want 2 of 2", pos, total)
	}
	first.Release()
	if got := h.startedIDs(); !equalIDs(got, []string{"q1"}) {
		t.Fatalf("started = %v, want [q1]", got)
	}
}

func TestSchedulerAcquireCancelledWhileWaitingLeavesNoWaiter(t *testing.T) {
	h := newSchedHarness(1, false)
	holder, _ := h.tryAdmit("holder")
	waiter := &runSlot{s: h.slotScheduler, runID: "waiter"}
	ctx, cancel := context.WithCancelCause(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- waiter.Acquire(ctx) }()
	waitForSchedCondition(t, func() bool { _, _, w := position(h, "waiter"); return w })
	cause := errors.New("aborted")
	cancel(cause)
	if err := <-errc; !errors.Is(err, cause) {
		t.Fatalf("Acquire error = %v, want the cancel cause", err)
	}
	if _, _, waiting := position(h, "waiter"); waiting {
		t.Fatal("cancelled waiter is still queued")
	}
	h.enqueue("next")
	holder.Release()
	if got := h.startedIDs(); !equalIDs(got, []string{"next"}) {
		t.Fatalf("started = %v, want [next]: a cancelled waiter must not swallow the slot", got)
	}
}

func TestSchedulerReleaseIsIdempotent(t *testing.T) {
	h := newSchedHarness(2, false)
	a, _ := h.tryAdmit("a")
	h.tryAdmit("b")
	a.Release()
	a.Release()
	a.Yield()
	// One slot is still held by b; the cap of 2 must still bind.
	if _, ok := h.tryAdmit("c"); !ok {
		t.Fatal("a slot should be free after a released")
	}
	if _, ok := h.tryAdmit("d"); ok {
		t.Fatal("double release freed a slot that b still holds")
	}
}

func waitForSchedCondition(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestSchedulerStatsTrackYieldedRuns(t *testing.T) {
	h := newSchedHarness(2, false)
	a, _ := h.tryAdmit("a")
	b, _ := h.tryAdmit("b")
	h.enqueue("q")
	if st := h.stats(); st.Active != 2 || st.Queued != 1 || st.Yielded != 0 || st.Max != 2 {
		t.Fatalf("stats = %+v, want 2 active, 1 queued", st)
	}
	a.Yield() // a parks: the queued run takes its slot
	if st := h.stats(); st.Active != 2 || st.Queued != 0 || st.Yielded != 1 {
		t.Fatalf("stats after a yielded = %+v, want 2 active, 0 queued, 1 yielded", st)
	}
	a.Yield() // idempotent
	if st := h.stats(); st.Yielded != 1 {
		t.Fatalf("double yield counted twice: %+v", st)
	}
	b.Release()
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := h.stats(); st.Active != 2 || st.Yielded != 0 {
		t.Fatalf("stats after a resumed = %+v, want 2 active, 0 yielded", st)
	}
	a.Yield()
	a.Release() // a finishes while parked
	if st := h.stats(); st.Yielded != 0 || st.Active != 1 {
		t.Fatalf("stats after a finished = %+v, want 1 active (the started queue head), 0 yielded", st)
	}
}

func TestSchedulerLoweredCapAdmitsNothingUntilUsageDropsBelowIt(t *testing.T) {
	h := newSchedHarness(3, false)
	a, _ := h.tryAdmit("a")
	b, _ := h.tryAdmit("b")
	h.setLimits(1, false)
	if _, ok := h.tryAdmit("c"); ok {
		t.Fatal("admitted a run while usage (2) exceeds the lowered cap (1)")
	}
	h.enqueue("c")
	a.Release()
	if got := h.startedIDs(); len(got) != 0 {
		t.Fatalf("started %v with usage at the lowered cap", got)
	}
	b.Release()
	if got := h.startedIDs(); !equalIDs(got, []string{"c"}) {
		t.Fatalf("started = %v, want [c] once usage dropped below the cap", got)
	}
}

// Concurrent Acquire calls for one run are single-flight: the run takes one
// slot, however many callers race.
func TestSchedulerConcurrentAcquireTakesOneSlot(t *testing.T) {
	for iter := 0; iter < 10; iter++ {
		h := newSchedHarness(4, false)
		run, _ := h.tryAdmit("run")
		run.Yield()
		if st := h.stats(); st.Active != 0 || st.Yielded != 1 {
			t.Fatalf("setup stats = %+v", st)
		}
		// Widen the window between "not held" and "slot taken" so every
		// caller is inside Acquire before any of them finishes.
		inner := h.limits
		h.limits = func() (int, bool) {
			time.Sleep(5 * time.Millisecond)
			return inner()
		}
		const callers = 8
		var ready, done sync.WaitGroup
		start := make(chan struct{})
		ready.Add(callers)
		done.Add(callers)
		for i := 0; i < callers; i++ {
			go func() {
				defer done.Done()
				ready.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if err := run.Acquire(ctx); err != nil {
					t.Error(err)
				}
			}()
		}
		ready.Wait()
		close(start)
		done.Wait()
		if st := h.stats(); st.Active != 1 || st.Yielded != 0 {
			t.Fatalf("iteration %d: stats after %d concurrent Acquires = %+v, want exactly 1 slot", iter, callers, st)
		}
		run.Release()
		if st := h.stats(); st.Active != 0 {
			t.Fatalf("iteration %d: release left %+v", iter, st)
		}
	}
}

// A second Acquire waits on the first rather than queueing its own waiter, and
// takes over if the first is cancelled.
func TestSchedulerSecondAcquireWaitsForFirstAndSurvivesItsCancel(t *testing.T) {
	h := newSchedHarness(1, false)
	holder, _ := h.tryAdmit("holder")
	run := &runSlot{s: h.slotScheduler, runID: "run"}

	ctx1, cancel1 := context.WithCancelCause(context.Background())
	first := make(chan error, 1)
	go func() { first <- run.Acquire(ctx1) }()
	waitForSchedCondition(t, func() bool { _, _, w := position(h, "run"); return w })

	second := make(chan error, 1)
	go func() { second <- run.Acquire(context.Background()) }()
	time.Sleep(30 * time.Millisecond)
	if st := h.stats(); st.Queued != 1 {
		t.Fatalf("two concurrent Acquires queued %d waiters, want 1", st.Queued)
	}

	cancel1(errors.New("first caller gave up"))
	if err := <-first; err == nil {
		t.Fatal("cancelled Acquire returned nil")
	}
	waitForSchedCondition(t, func() bool { _, _, w := position(h, "run"); return w })
	holder.Release()
	if err := <-second; err != nil {
		t.Fatalf("second Acquire = %v, want it to take over and succeed", err)
	}
	if st := h.stats(); st.Active != 1 || st.Yielded != 0 {
		t.Fatalf("stats = %+v, want the run holding exactly one slot", st)
	}
}

func TestSchedulerDisablingHoldReleasesOnlyParkedHeldSlots(t *testing.T) {
	h := newSchedHarness(2, true)
	parked, _ := h.tryAdmit("parked")
	working, _ := h.tryAdmit("working")
	h.enqueue("queued")
	parked.Yield() // skipped by the policy: parked while holding
	if st := h.stats(); st.Active != 2 || st.Yielded != 0 {
		t.Fatalf("under hold: %+v", st)
	}
	h.setLimits(2, false)
	h.dispatch()
	if got := h.startedIDs(); !equalIDs(got, []string{"queued"}) {
		t.Fatalf("started = %v, want the queued run to take the parked gate's slot", got)
	}
	if st := h.stats(); st.Active != 2 || st.Yielded != 1 {
		t.Fatalf("after disabling hold: %+v, want the parked gate yielded and the working run untouched", st)
	}
	// The working run never yielded, so its slot was not taken.
	if !working.held {
		t.Fatal("a run that was not parked lost its slot")
	}
	// The parked gate resumes with priority, like any yielded run.
	got := make(chan error, 1)
	go func() { got <- parked.Acquire(context.Background()) }()
	waitForSchedCondition(t, func() bool { _, _, w := position(h, "parked"); return w })
	working.Release()
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	// Re-enabling hold is not retroactive and a resumed run is never released by it.
	h.setLimits(2, true)
	h.dispatch()
	if !parked.held {
		t.Fatal("re-enabling hold took the slot of a running gate")
	}
}

// A run that resumes concurrently with the policy change keeps its slot: the
// release only takes slots still marked parked-while-holding.
func TestSchedulerResumedHeldSlotIsNotReleasedByPolicyChange(t *testing.T) {
	h := newSchedHarness(1, true)
	slot, _ := h.tryAdmit("run")
	slot.Yield()
	if err := slot.Acquire(context.Background()); err != nil { // resume while holding
		t.Fatal(err)
	}
	h.setLimits(1, false)
	h.dispatch()
	if !slot.held || h.stats().Active != 1 || h.stats().Yielded != 0 {
		t.Fatalf("a resumed run lost its slot on the policy change: %+v held=%v", h.stats(), slot.held)
	}
}

// Disabling hold releases the slots of runs parked under it, starts the queue,
// and leaves a run that resumed in the meantime holding its slot.
func TestSchedulerDisablingHoldReleasesParkedSlots(t *testing.T) {
	h := newSchedHarness(1, true)
	parked, _ := h.tryAdmit("parked")
	h.enqueue("queued")
	parked.Yield() // skipped by the hold policy
	if got := h.startedIDs(); len(got) != 0 {
		t.Fatalf("started %v under hold", got)
	}
	h.setLimits(1, false)
	h.dispatch()
	if got := h.startedIDs(); !equalIDs(got, []string{"queued"}) {
		t.Fatalf("started = %v after disabling hold, want [queued]", got)
	}
	if st := h.stats(); st.Yielded != 1 || st.Active != 1 {
		t.Fatalf("stats = %+v, want the parked run yielded and the queued one active", st)
	}
	// It resumes through the normal priority path.
	done := make(chan error, 1)
	go func() { done <- parked.Acquire(context.Background()) }()
	waitForSchedCondition(t, func() bool { _, _, w := position(h, "parked"); return w })
	h.slot("queued").Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerResumedRunKeepsItsSlotWhenHoldIsDisabled(t *testing.T) {
	h := newSchedHarness(1, true)
	run, _ := h.tryAdmit("run")
	h.enqueue("queued")
	run.Yield()
	if err := run.Acquire(context.Background()); err != nil { // resumed: working again
		t.Fatal(err)
	}
	h.setLimits(1, false)
	h.dispatch()
	if got := h.startedIDs(); len(got) != 0 {
		t.Fatalf("a working run's slot was released: started %v", got)
	}
	if st := h.stats(); st.Active != 1 || st.Yielded != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestSchedulerDisablingHoldDuringYieldRegistration(t *testing.T) {
	h := newSchedHarness(1, true)
	parked, _ := h.tryAdmit("parked")
	h.enqueue("queued")
	checked := make(chan struct{})
	register := make(chan struct{})
	done := make(chan struct{})
	go func() {
		h.slotScheduler.mu.Lock()
		hold := h.slotScheduler.hold
		h.slotScheduler.mu.Unlock()
		close(checked)
		<-register
		if !hold || !parked.markParkedHeld() {
			parked.release(true)
		}
		close(done)
	}()
	<-checked
	h.setLimits(1, false)
	h.refresh()
	close(register)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("yield did not finish")
	}
	h.dispatch()
	if got := h.startedIDs(); !equalIDs(got, []string{"queued"}) {
		t.Fatalf("started = %v, want queued after disabling hold during yield", got)
	}
	if st := h.stats(); st.Active != 1 || st.Yielded != 1 {
		t.Fatalf("stats = %+v, want one active queued run and one yielded run", st)
	}
}

func TestSchedulerReservationPrecedesAcquire(t *testing.T) {
	h := newSchedHarness(1, false)
	parked, _ := h.tryAdmit("parked")
	parked.Yield()
	blocker, _ := h.tryAdmit("blocker")
	h.enqueue("new")
	parked.Reserve()
	parked.Reserve()
	if pos, total, waiting := position(h, "parked"); !waiting || pos != 1 || total != 2 {
		t.Fatalf("reservation = %d of %d, waiting=%v", pos, total, waiting)
	}
	blocker.Release()
	if got := h.startedIDs(); len(got) != 0 {
		t.Fatalf("new work overtook a reserved resume before Acquire: %v", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := parked.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if st := h.stats(); st.Active != 1 || st.Queued != 1 || st.Yielded != 0 {
		t.Fatalf("acquired reservation: %+v", st)
	}
	parked.Release()
	if got := h.startedIDs(); !equalIDs(got, []string{"new"}) {
		t.Fatalf("start order = %v", got)
	}
}

func TestSchedulerReleaseUnusedReservation(t *testing.T) {
	for _, granted := range []bool{false, true} {
		t.Run(fmt.Sprint(granted), func(t *testing.T) {
			h := newSchedHarness(1, false)
			parked, _ := h.tryAdmit("parked")
			parked.Yield()
			blocker, _ := h.tryAdmit("blocker")
			h.enqueue("new")
			parked.Reserve()
			if granted {
				blocker.Release()
			}
			parked.Release()
			parked.Release()
			blocker.Release()
			if got := h.startedIDs(); !equalIDs(got, []string{"new"}) {
				t.Fatalf("unused reservation blocked new work: %v", got)
			}
			if st := h.stats(); st.Active != 1 || st.Queued != 0 || st.Yielded != 0 {
				t.Fatalf("release left a reservation: %+v", st)
			}
		})
	}
}
