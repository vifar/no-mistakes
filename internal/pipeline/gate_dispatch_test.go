package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_DelayedResponseDoesNotConsumeReplacementGate(t *testing.T) {
	testDelayedGateDispatch(t, types.ActionApprove)
}

func TestExecutor_DelayedAnswerDoesNotConsumeReplacementGate(t *testing.T) {
	testDelayedGateDispatch(t, types.ActionAnswer)
}

// Admission tests cannot cover a response that has already left approvalCh.
// Pause exactly there, replace the park through durable recovery, and observe
// the real execution loop. The barrier is instance-local and nil in production.
func testDelayedGateDispatch(t *testing.T, action types.ApprovalAction) {
	t.Helper()
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)
	parks := make(chan ipc.Event, 4)
	discarded := make(chan struct{}, 1)
	extraTurn := make(chan struct{}, 1)
	var calls atomic.Int32
	step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		if calls.Add(1) > 1 {
			extraTurn <- struct{}{}
		}
		return &StepOutcome{NeedsApproval: true, Findings: gateFindingsThree}, nil
	}}
	e := NewExecutor(database, p, nil, nil, []Step{step}, func(event ipc.Event) {
		if event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			parks <- event
		}
		if event.Content != nil && strings.Contains(*event.Content, "discarding superseded gate response") {
			discarded <- struct{}{}
		}
	})
	e.SetRunSlot(&recordingSlot{})
	dequeued, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.afterApprovalDequeue = func() { once.Do(func() { close(dequeued); <-release }) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = e.Execute(ctx, run, repo, workDir) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-release:
		default:
			close(release)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("executor did not stop")
		}
	})
	nextIdentityPark(t, parks)
	a := e.CurrentGate()
	answerDir := reviewqa.Dir(p.RunEvidenceDir("", run.ID))
	if action == types.ActionAnswer {
		if err := os.MkdirAll(answerDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(answerDir, reviewqa.QuestionsFile), []byte("{\"id\":\"q1\",\"kind\":\"question\",\"question\":\"Keep it?\",\"weight\":\"major\"}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := reviewqa.AppendAnswer(answerDir, reviewqa.Answer{ID: "q1", Answer: "keep", AskOrdinal: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Respond(types.StepReview, action, nil, &a); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dequeued:
	case <-time.After(5 * time.Second):
		t.Fatal("response did not reach dequeue barrier")
	}

	// Recovery replaces A with another park of the same round. The old
	// response is now in flight, so draining the channel alone cannot fix it.
	e.clearApprovalGate()
	// Match the comparison-restart path's cleared admission bookkeeping.
	e.mu.Lock()
	e.slotWaitStep = ""
	e.mu.Unlock()
	accepted, err := database.GetStepResult(a.StepResultID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(run.ID, a.StepResultID, types.StepStatusAwaitingApproval, 0, 1, accepted.FindingsJSON, a.RoundID); err != nil {
		t.Fatal(err)
	}
	recovered, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RegisterRecoveredGate(recovered); err != nil {
		t.Fatal(err)
	}
	b := e.CurrentGate()
	if b.Nonce == a.Nonce {
		t.Fatal("replacement did not change park identity")
	}
	close(release)
	select {
	case <-discarded:
	case <-extraTurn:
		t.Fatal("delayed response dispatched an extra finalization round at replacement gate B")
	case <-done:
		t.Fatal("delayed response consumed replacement gate B")
	case <-time.After(5 * time.Second):
		t.Fatal("superseded response was not discarded")
	}
	e.mu.Lock()
	waiting, current := e.waiting, e.gate
	e.mu.Unlock()
	row, err := database.GetStepResult(b.StepResultID)
	if err != nil || !waiting || current != b || row.Status != types.StepStatusAwaitingApproval || row.PendingResponse != nil {
		t.Fatalf("B did not stay parked: waiting=%v current=%+v row=%+v err=%v", waiting, current, row, err)
	}
	rounds, err := database.GetRoundsByStep(b.StepResultID)
	if err != nil || len(rounds) != 1 || calls.Load() != 1 {
		t.Fatalf("extra finalization: calls=%d rounds=%d err=%v", calls.Load(), len(rounds), err)
	}
	if action == types.ActionAnswer {
		conv, err := reviewqa.Load(answerDir)
		if err != nil || len(conv.Answered()) != 1 || len(conv.Open()) != 0 {
			t.Fatalf("answer did not stay recorded: %+v, %v", conv, err)
		}
	}
}
