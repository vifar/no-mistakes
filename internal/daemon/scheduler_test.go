package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
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
	})
	h.start = func(id string, slot *runSlot) {
		h.mu.Lock()
		h.started = append(h.started, id)
		h.slots[id] = slot
		h.mu.Unlock()
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
	a := h.forceAcquire("a")
	a.Yield()
	b := h.forceAcquire("b")
	b.Yield()
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

func TestSchedulerForceAcquireCountsBeyondTheCap(t *testing.T) {
	h := newSchedHarness(1, false)
	a := h.forceAcquire("recovered-a")
	b := h.forceAcquire("recovered-b")
	if _, ok := h.tryAdmit("new"); ok {
		t.Fatal("new launch admitted while recovered runs exceed the cap")
	}
	h.enqueue("new")
	a.Release()
	if got := h.startedIDs(); len(got) != 0 {
		t.Fatalf("queued run started while one recovered run still holds the cap: %v", got)
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
	waiter := h.forceAcquire("waiter")
	waiter.Yield()
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
