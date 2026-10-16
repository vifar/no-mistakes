package daemon

import (
	"context"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The barrier models descheduling the IPC answer handler after it decided the
// answer closed the last question, but before it calls Executor.Respond.
// Execute supplies ordinary supported review outcomes. The real ReviewStep
// owns the resumer, and the real daemon handler owns the answer append/release.
func TestAnswerReviewQuestionDelayedReleaseMustNotConsumeNextGate(t *testing.T) {
	for _, fixing := range []bool{false, true} {
		name := "initial-review"
		if fixing {
			name = "fix-review"
		}
		t.Run(name, func(t *testing.T) {
			m, p, _ := answerFixture(t)
			repo, err := m.db.InsertRepo(t.TempDir(), "https://example.test/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := m.db.InsertRun(repo.ID, "feature", "head-1", "base-1")
			if err != nil {
				t.Fatal(err)
			}
			step := &diagnosticAnswerStep{fixing: fixing, t: t}
			parks := make(chan ipc.Event, 8)
			cfg := &config.Config{Review: config.Review{Conversation: true}, AutoFix: config.AutoFix{Review: 1}}
			executor := pipeline.NewExecutor(m.db, p, cfg, nil, []pipeline.Step{step}, func(e ipc.Event) {
				if e.Status != nil && (*e.Status == string(types.StepStatusAwaitingApproval) || *e.Status == string(types.StepStatusFixReview)) {
					parks <- e
				}
			})
			executor.SetRunSlot(diagnosticImmediateSlot{})
			executor.SetGateReconcileTimings(time.Millisecond, time.Second)
			m.mu.Lock()
			m.runs[run.ID] = &managedRun{run: run, executor: executor}
			m.mu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			workDir := t.TempDir()
			go func() { done <- executor.Execute(ctx, run, repo, workDir) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("executor did not stop")
				}
				m.beforeAnswerRespond = nil
			})
			first := diagnosticWaitPark(t, parks)
			if first.Findings == nil || !strings.Contains(*first.Findings, "question-q1") {
				t.Fatalf("first park: %+v", first)
			}
			paused, release := make(chan struct{}), make(chan struct{})
			m.beforeAnswerRespond = func() { close(paused); <-release }
			answerDone := make(chan struct{})
			var result *ipc.AnswerReviewQuestionResult
			var answerErr error
			go func() {
				defer close(answerDone)
				result, answerErr = m.HandleAnswerReviewQuestion(run.ID, "q1", "keep", "operator")
			}()
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
				select {
				case <-answerDone:
				case <-time.After(5 * time.Second):
					t.Error("answer handler did not stop")
				}
			})
			select {
			case <-paused:
			case <-time.After(5 * time.Second):
				t.Fatal("answer handler did not reach release barrier")
			}
			second := diagnosticWaitPark(t, parks)
			if second.Findings == nil || !strings.Contains(*second.Findings, "operator decision") || strings.Contains(*second.Findings, "question-q1") {
				t.Fatalf("next gate is not the code-only gate: %+v", second)
			}
			conv, err := reviewqa.Load(executor.ReviewConversationDir(run.ID))
			if err != nil || len(conv.Answered()) != 1 || len(conv.Open()) != 0 {
				t.Fatalf("answer durability: %+v, %v", conv, err)
			}
			close(release)
			select {
			case <-answerDone:
			case <-time.After(5 * time.Second):
				t.Fatal("answer handler did not finish")
			}
			if answerErr != nil {
				t.Fatal(answerErr)
			}
			if result.Resumed {
				// Observe the actual unintended turn before failing, so the
				// assertion proves an effect rather than only an OK echo.
				third := diagnosticWaitPark(t, parks)
				rows, err := m.db.GetStepsByRun(run.ID)
				if err != nil || len(rows) != 1 {
					t.Fatalf("steps: %+v, %v", rows, err)
				}
				rounds, err := m.db.GetRoundsByStep(rows[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				t.Fatalf("stale answer release consumed a NEW code-only gate: result=%+v; extra park=%s; durable rounds=%d; last trigger=%s", result, *third.Status, len(rounds), rounds[len(rounds)-1].Trigger)
			}
			wantCalls := int32(2)
			if fixing {
				wantCalls = 3
			}
			if got := step.calls.Load(); got != wantCalls {
				t.Fatalf("dispatches = %d, want %d (exactly one answer finalization)", got, wantCalls)
			}
			rows, err := m.db.GetStepsByRun(run.ID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("steps: %+v, %v", rows, err)
			}
			rounds, err := m.db.GetRoundsByStep(rows[0].ID)
			if err != nil || len(rounds) != int(wantCalls) {
				t.Fatalf("rounds = %d, want %d: %v", len(rounds), wantCalls, err)
			}
			if rows[0].Status != types.StepStatus(*second.Status) {
				t.Fatalf("new gate did not stay parked: %s", rows[0].Status)
			}
			if !strings.Contains(result.Note, "already resolved or superseded") {
				t.Fatalf("inaccurate no-resume reason: %s", result.Note)
			}

		})
	}
}

type diagnosticImmediateSlot struct{}

func (diagnosticImmediateSlot) Reserve()                      {}
func (diagnosticImmediateSlot) Yield()                        {}
func (diagnosticImmediateSlot) Acquire(context.Context) error { return nil }

func diagnosticWaitPark(t *testing.T, ch <-chan ipc.Event) ipc.Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("expected park not published")
	}
	return ipc.Event{}
}

type diagnosticAnswerStep struct {
	fixing bool
	calls  atomic.Int32
	t      *testing.T
}

func (s *diagnosticAnswerStep) Name() types.StepName { return types.StepReview }
func (s *diagnosticAnswerStep) ResumeApprovalGate(c *pipeline.StepContext, f string) (types.ApprovalAction, bool, error) {
	return (&steps.ReviewStep{}).ResumeApprovalGate(c, f)
}
func (s *diagnosticAnswerStep) Execute(c *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	call := s.calls.Add(1)
	questionTurn := 1
	if s.fixing {
		questionTurn = 2
		if call == 1 {
			return &pipeline.StepOutcome{AutoFixable: true, Findings: `{"findings":[{"id":"bug","severity":"error","description":"bug","action":"auto-fix"}],"summary":"fix"}`}, nil
		}
	}
	if int(call) == questionTurn {
		dir := reviewqa.Dir(c.EvidenceDir)
		appendAgentQuestionLine(s.t, dir, `{"id":"q1","question":"keep this behavior?","options":["keep","remove"]}`)
		return &pipeline.StepOutcome{Findings: `{"findings":[{"id":"question-q1","severity":"warning","description":"question awaiting answer","action":"ask-user","category":"review-question"}],"summary":"question"}`}, nil
	}
	if !c.FinalizingAnswers {
		s.t.Error("expected answer finalization")
	}
	if s.fixing && (!c.Fixing || !c.SkipFixExecution) {
		s.t.Error("fix-review finalization lost skip-fixer state")
	}
	return &pipeline.StepOutcome{Findings: `{"findings":[{"id":"code","severity":"warning","description":"operator decision","action":"ask-user"}],"summary":"operator decision"}`}, nil
}
