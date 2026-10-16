//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/types"
	toon "github.com/toon-format/toon-go"
)

// These journeys use the user intent as the oracle: only the exact observed
// park authorizes a response. An implementation that matches just the step,
// accepts a missing identity, or replays an answer at a later park must fail.
func gateJourneyEvidence(t *testing.T, label string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %s", label, raw)
}

func gateJourneyStart(t *testing.T, h *Harness, branch string) types.GateIdentity {
	t.Helper()
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "feature.txt", "flag = true\n", "add feature flag")
	if out, err := h.Run("axi", "run", "--intent", axiIntent); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	run := h.ActiveRun(branch)
	if run == nil {
		t.Fatal("no active run")
	}
	return gateJourneyWait(t, h, run.ID, "")
}

func gateJourneyWait(t *testing.T, h *Harness, runID, previousNonce string) types.GateIdentity {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		run := h.RunInfo(runID)
		for _, step := range run.Steps {
			if step.StepName == types.StepReview && step.Gate != nil && step.Gate.Nonce != previousNonce &&
				(step.Status == types.StepStatusAwaitingApproval || step.Status == types.StepStatusFixReview) && (step.LastActivity == nil || *step.LastActivity != "already answered; waiting for a run slot") {
				g := *step.Gate
				if g.RunID != runID || g.StepResultID == "" || g.RoundID == "" || g.Nonce == "" {
					t.Fatalf("incomplete IPC gate identity: %+v", g)
				}
				gateJourneyEvidence(t, "IPC parked gate", g)
				return g
			}
		}
		if run.Status.Terminal() {
			t.Fatalf("run ended before review parked: %+v", run)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("review did not publish the expected gate")
	return types.GateIdentity{}
}

func gateJourneyRespond(t *testing.T, socket string, runID string, action types.ApprovalAction, expected *types.GateIdentity) ipc.RespondResult {
	t.Helper()
	client, err := ipc.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.RespondResult
	if err := client.Call(ipc.MethodRespond, &ipc.RespondParams{
		RunID: runID, Step: types.StepReview, Action: action, ExpectedGate: expected,
	}, &result); err != nil {
		t.Fatal(err)
	}
	gateJourneyEvidence(t, "IPC respond request", ipc.RespondParams{RunID: runID, Step: types.StepReview, Action: action, ExpectedGate: expected})
	gateJourneyEvidence(t, "IPC respond result", result)
	return result
}

func gateJourneyRefusal(t *testing.T, result ipc.RespondResult, current types.GateIdentity) {
	t.Helper()
	if result.OK || result.Code != "gate_superseded" || result.CurrentGate == nil || *result.CurrentGate != current {
		t.Fatalf("want typed refusal identifying current gate %+v; got %+v", current, result)
	}
}

func gateJourneyRounds(t *testing.T, h *Harness, gate types.GateIdentity) []*db.StepRound {
	t.Helper()
	database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rounds, err := database.GetRoundsByStep(gate.StepResultID)
	if err != nil {
		t.Fatal(err)
	}
	gateJourneyEvidence(t, "persisted review rounds", rounds)
	return rounds
}

func TestGateIdentityJourney_StaleAndUnboundResponsesAndStatus(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})
	a := gateJourneyStart(t, h, "feature/gate-identity")
	socket := paths.WithRoot(h.NMHome).Socket()
	if result := gateJourneyRespond(t, socket, a.RunID, types.ActionAnswer, &a); !result.OK {
		t.Fatalf("current gate refused: %+v", result)
	}
	b := gateJourneyWait(t, h, a.RunID, a.Nonce)
	if a.StepResultID != b.StepResultID || a.RoundID == b.RoundID {
		t.Fatalf("expected a new round on the same step: A=%+v B=%+v", a, b)
	}
	for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix, types.ActionSkip, types.ActionAnswer} {
		gateJourneyRefusal(t, gateJourneyRespond(t, socket, a.RunID, action, &a), b)
		gateJourneyRefusal(t, gateJourneyRespond(t, socket, a.RunID, action, nil), b)
	}
	// Omit the wire key entirely too; null and absent are both unauthorized.
	client, err := ipc.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	var missing ipc.RespondResult
	err = client.Call(ipc.MethodRespond, map[string]string{"run_id": a.RunID, "step": "review", "action": "approve"}, &missing)
	client.Close()
	if err != nil {
		t.Fatal(err)
	}
	gateJourneyRefusal(t, missing, b)
	gateJourneyEvidence(t, "IPC respond result with omitted expected_gate", missing)
	if got := gateJourneyWait(t, h, a.RunID, ""); got != b {
		t.Fatalf("refused responses changed B: %+v", got)
	}
	if rounds := gateJourneyRounds(t, h, b); len(rounds) != 2 {
		t.Fatalf("refused responses dispatched extra turns: %d rounds", len(rounds))
	}
	out, err := h.Run("axi", "status")
	if err != nil {
		t.Fatalf("axi status: %v\n%s", err, out)
	}
	t.Logf("axi status output:\n%s", out)
	var status struct {
		Gate struct {
			RunID        string `toon:"run_id"`
			StepResultID string `toon:"step_result_id"`
			Step         string `toon:"step"`
			RoundID      string `toon:"round_id"`
			Nonce        string `toon:"nonce"`
			Status       string `toon:"status"`
		} `toon:"gate"`
	}
	if err := toon.UnmarshalString(out, &status); err != nil {
		t.Fatal(err)
	}
	g := status.Gate
	if g.RunID != b.RunID || g.StepResultID != b.StepResultID || g.Step != "review" || g.RoundID != b.RoundID || g.Nonce != b.Nonce || g.Status != string(b.Status) {
		t.Fatalf("CLI status disagrees with the parked IPC identity: %+v, want %+v\n%s", g, b, out)
	}
	t.Log("scenarios 1 and 6: stale and identity-less responses refused; B retained; CLI and IPC expose the complete identity")
}

func TestGateIdentityJourney_DelayedClosingAnswer(t *testing.T) {
	barrier := t.TempDir()
	t.Setenv("NM_E2E_ANSWER_BARRIER", barrier)
	scenario := writeScenario(t, "delayed-answer.yaml", `actions:
  - match: "Answers to the questions you asked in this pass"
    structured:
      findings:
        - id: operator
          severity: warning
          file: feature.txt
          line: 1
          description: operator must decide the code change
          action: ask-user
      summary: operator decision
      risk_level: medium
      risk_rationale: operator decision remains
      risk_scope: source-or-external
  - match: "Review the code changes and return structured findings"
    ask_questions:
`+question("q1", "Keep the flag enabled?")+`
    structured:
      findings: []
      summary: question open
      risk_level: medium
      risk_rationale: question open
      risk_scope: source-or-external
`)
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: "gate_reconcile_interval: 100ms\ngate_reconcile_timeout: 2s"})
	pushMainRepoConfig(t, h, trustedRepoConfigWithReviewConversation)
	a := gateJourneyStart(t, h, "feature/delayed-answer")
	client, err := ipc.Dial(paths.WithRoot(h.NMHome).Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	type answerResult struct {
		result ipc.AnswerReviewQuestionResult
		err    error
	}
	done := make(chan answerResult, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var result ipc.AnswerReviewQuestionResult
		err := client.Call(ipc.MethodAnswerReview, &ipc.AnswerReviewQuestionParams{RunID: a.RunID, QuestionID: "q1", Answer: "keep", AnsweredBy: "operator"}, &result)
		done <- answerResult{result, err}
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(barrier, "release"), nil, 0o600)
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("answer request did not finish")
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(barrier, "arrived")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("answer handler did not reach its release boundary")
		}
		time.Sleep(10 * time.Millisecond)
	}
	b := gateJourneyWait(t, h, a.RunID, a.Nonce)
	step, _ := findStep(h.RunInfo(a.RunID).Steps, types.StepReview)
	if step.FindingsJSON == nil || !strings.Contains(*step.FindingsJSON, "operator must decide") || strings.Contains(*step.FindingsJSON, "question-q1") {
		t.Fatalf("B is not the operator gate: %+v", step)
	}
	if err := os.WriteFile(filepath.Join(barrier, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || !got.result.OK || !got.result.ClosedLast || got.result.Resumed || !strings.Contains(got.result.Note, "superseded") {
			t.Fatalf("late answer result: %+v, %v", got.result, got.err)
		}
		gateJourneyEvidence(t, "delayed answer result", got.result)
	case <-time.After(10 * time.Second):
		t.Fatal("late answer handler did not return")
	}
	conv, err := reviewqa.Load(reviewqa.Dir(filepath.Join(h.NMHome, "evidence", a.RunID)))
	if err != nil || len(conv.Open()) != 0 || len(conv.Answered()) != 1 || conv.Answered()[0].Answer.Answer != "keep" {
		t.Fatalf("answer was lost: %+v, %v", conv, err)
	}
	gateJourneyEvidence(t, "persisted answered conversation", conv.Answered())
	if got := gateJourneyWait(t, h, a.RunID, ""); got != b {
		t.Fatalf("late answer released B: %+v", got)
	}
	rounds := gateJourneyRounds(t, h, b)
	if len(rounds) != 2 || rounds[1].Trigger != "answer" {
		t.Fatalf("want exactly one finalize turn: %+v", rounds)
	}
	t.Log("scenario 2: automatic resumer finalized once; delayed handler retained its answer and left the operator gate parked")
}

// Delay one real CLI request on its way to the real daemon. Neither the status
// reply nor the response/refusal is synthesized. Moving A to B while the wire
// request waits detects callers that bind to admission time instead of the
// snapshot they read. All connections and the socket rename are cleaned up.
func gateJourneyProxy(t *testing.T, socket string) (string, <-chan ipc.RespondParams, func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("this wire-delay helper uses Unix sockets")
	}
	backend := socket + ".real"
	if err := os.Rename(socket, backend); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		_ = os.Rename(backend, socket)
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	arrived := make(chan ipc.RespondParams, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var intercepted atomic.Bool
	var workers sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			front, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer front.Close()
				back, err := net.Dial("unix", backend)
				if err != nil {
					return
				}
				defer back.Close()
				stop := context.AfterFunc(ctx, func() { front.Close(); back.Close() })
				defer stop()
				copied := make(chan struct{})
				go func() { _, _ = io.Copy(front, back); front.Close(); close(copied) }()
				scanner := bufio.NewScanner(front)
				scanner.Buffer(make([]byte, 4096), ipc.MaxFrameBytes)
				for scanner.Scan() {
					line := append([]byte(nil), scanner.Bytes()...)
					var req ipc.Request
					if json.Unmarshal(line, &req) == nil && req.Method == ipc.MethodRespond && intercepted.CompareAndSwap(false, true) {
						var params ipc.RespondParams
						if json.Unmarshal(req.Params, &params) == nil {
							arrived <- params
						}
						select {
						case <-release:
						case <-ctx.Done():
						}
					}
					if _, err := back.Write(append(line, '\n')); err != nil {
						break
					}
				}
				back.Close()
				<-copied
			}()
		}
	}()
	t.Cleanup(func() {
		unblock()
		cancel()
		listener.Close()
		<-acceptDone
		workers.Wait()
		_ = os.Remove(socket)
		if err := os.Rename(backend, socket); err != nil {
			t.Errorf("restore daemon socket: %v", err)
		}
	})
	return backend, arrived, unblock
}

func TestGateIdentityJourney_CLIRespondUsesObservedSnapshot(t *testing.T) {
	gateJourneyObservedResponse(t, false)
}

func TestGateIdentityJourney_TUIRespondUsesDisplayedSnapshot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the PTY driver requires Unix")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("python3 is required to drive the TUI through a PTY")
	}
	gateJourneyObservedResponse(t, true)
}

func gateJourneyObservedResponse(t *testing.T, tui bool) {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})
	a := gateJourneyStart(t, h, "feature/cli-snapshot")
	backend, arrived, release := gateJourneyProxy(t, paths.WithRoot(h.NMHome).Socket())
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.NMBin, "axi", "respond", "--run", a.RunID, "--action", "approve")
	if tui {
		driver, err := filepath.Abs(filepath.Join("testdata", "tui_yolo_driver.py"))
		if err != nil {
			t.Fatal(err)
		}
		cmd = exec.CommandContext(ctx, "python3", driver, h.NMBin, h.WorkDir, "a", "6", "6")
		// Let the driver's finally block terminate and wait for its PTY child
		// when a failing assertion cancels this command.
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 5 * time.Second
	}
	cmd.Dir = h.WorkDir
	cmd.Env = os.Environ()
	type cliResult struct {
		out []byte
		err error
	}
	done := make(chan struct{})
	var got cliResult
	go func() {
		got.out, got.err = cmd.CombinedOutput()
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("caller process did not exit")
		}
	})
	select {
	case params := <-arrived:
		if params.ExpectedGate == nil || *params.ExpectedGate != a {
			t.Fatalf("caller did not send the snapshot it observed: %+v; want %+v", params, a)
		}
		gateJourneyEvidence(t, "intercepted caller request", params)
	case <-ctx.Done():
		t.Fatal("CLI did not submit a response")
	}
	if result := gateJourneyRespond(t, backend, a.RunID, types.ActionAnswer, &a); !result.OK {
		t.Fatalf("could not replace A: %+v", result)
	}
	b := gateJourneyWait(t, h, a.RunID, a.Nonce)
	release()
	select {
	case <-done:
		if tui {
			if got.err != nil || !strings.Contains(string(got.out), "gate already resolved or superseded") {
				t.Fatalf("TUI did not display the refusal: %v\n%s", got.err, got.out)
			}
			t.Logf("TUI terminal transcript:\n%s", got.out)
		} else if got.err == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 2 {
			t.Fatalf("stale CLI response did not exit with refusal: %v\n%s", got.err, got.out)
		}
		out := string(got.out)
		if !tui {
			t.Logf("axi respond refusal (exit %d):\n%s", cmd.ProcessState.ExitCode(), out)
			for _, want := range []string{"gate_superseded", "current_gate:", b.RunID, b.StepResultID, b.RoundID, b.Nonce} {
				if !strings.Contains(out, want) {
					t.Fatalf("CLI refusal omits %q:\n%s", want, out)
				}
			}
		}
	case <-ctx.Done():
		t.Fatal("CLI did not return its refusal")
	}
	if got := gateJourneyWait(t, h, a.RunID, ""); got != b || len(gateJourneyRounds(t, h, b)) != 2 {
		t.Fatalf("stale CLI response released B: %+v", got)
	}
	t.Logf("scenario 5 (TUI=%t): caller sent its observed A identity; real daemon refused it after B parked; B remains parked", tui)
}

func gateJourneyDaemon(t *testing.T, h *Harness, action string) {
	t.Helper()
	args := []string{"daemon", action}
	if action == "stop" {
		args = append(args, "--force")
	}
	if out, err := h.Run(args...); err != nil {
		t.Fatalf("daemon %s: %v\n%s", action, err, out)
	}
}

func TestGateIdentityJourney_ReparkSameRoundAndUnboundAbort(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})
	const branch = "feature/repark"
	a := gateJourneyStart(t, h, branch)
	gateJourneyDaemon(t, h, "stop")
	// Restore the same durable round as a fresh park, rather than inventing a
	// second review turn. This persisted-state fixture exercises the real park
	// writer and then the real daemon's recovery and response admission.
	database, err := db.Open(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := database.GetStepResult(a.StepResultID)
	if err != nil {
		t.Fatal(err)
	}
	err = database.ParkStepForApproval(a.RunID, a.StepResultID, a.Status, 0, 1, row.FindingsJSON, a.RoundID)
	database.Close()
	if err != nil {
		t.Fatal(err)
	}
	gateJourneyDaemon(t, h, "start")
	b := gateJourneyWait(t, h, a.RunID, a.Nonce)
	if b.RoundID != a.RoundID || b.StepResultID != a.StepResultID || b.Nonce == a.Nonce {
		t.Fatalf("repark did not change only the park identity: A=%+v B=%+v", a, b)
	}
	socket := paths.WithRoot(h.NMHome).Socket()
	gateJourneyRefusal(t, gateJourneyRespond(t, socket, a.RunID, types.ActionApprove, &a), b)
	// Wait for recovery registration, then exercise abort without any identity.
	gateJourneyAwaitSlots(t, h, func(s ipc.SlotsResult) bool { return s.ParkedReleased == 1 })
	if result := gateJourneyRespond(t, socket, a.RunID, types.ActionAbort, nil); !result.OK {
		t.Fatalf("unbound abort refused: %+v", result)
	}
	// Recovery has dispatched and yielded its slot at the waiting gate, so
	// this is a gate response (failed), not a pre-dispatch cancellation.
	if run := h.WaitForRun(branch, 30*time.Second); run.Status != types.RunFailed || run.Error == nil || *run.Error != "step review: aborted by user" {
		t.Fatalf("abort did not terminate the original run: status=%s error=%v", run.Status, run.Error)
	} else {
		gateJourneyEvidence(t, "aborted run", run)
	}
	if rounds := gateJourneyRounds(t, h, b); len(rounds) != 1 {
		t.Fatalf("stale nonce or abort dispatched work: %d rounds", len(rounds))
	}
	t.Log("scenario 3: real recovery retained the round and renewed the nonce; old nonce refused; identity-less abort terminated the run")
}

func gateJourneyAwaitSlots(t *testing.T, h *Harness, ready func(ipc.SlotsResult) bool) {
	t.Helper()
	client, err := ipc.Dial(paths.WithRoot(h.NMHome).Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var slots ipc.SlotsResult
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := client.Call(ipc.MethodGetSlots, nil, &slots); err != nil {
			t.Fatal(err)
		}
		if ready(slots) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("scheduler did not reach required state: %+v", slots)
}

func gateJourneyPending(t *testing.T, h *Harness, gate types.GateIdentity) string {
	t.Helper()
	database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	row, err := database.GetStepResult(gate.StepResultID)
	if err != nil || row.PendingResponse == nil || row.Status != types.StepStatusAwaitingApproval {
		t.Fatalf("response not durably parked while waiting for capacity: %+v, %v", row, err)
	}
	t.Logf("persisted pending response: %s", *row.PendingResponse)
	return *row.PendingResponse
}

// The pending-response JSON is an owned persisted contract. Malformed/stale
// fixtures model a pre-upgrade response and a response stranded beside a newer
// park. They are inserted only into this harness's disposable database, after
// obtaining the payload by actually accepting a response over daemon IPC.
func gateJourneyReplacePending(t *testing.T, h *Harness, gate types.GateIdentity, raw string) {
	t.Helper()
	control, err := sql.Open("sqlite", filepath.Join(h.NMHome, "state.sqlite")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec("UPDATE step_results SET pending_response = ? WHERE id = ?", raw, gate.StepResultID); err != nil {
		t.Fatal(err)
	}
}

func TestGateIdentityJourney_PendingResponseRecovery(t *testing.T) {
	for _, mode := range []string{"valid", "missing-identity", "superseded-identity", "superseded-during-slot-wait"} {
		t.Run(mode, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t), GlobalConfigExtra: "daemon:\n  max_concurrent_runs: 1\n  hold_slot_while_parked: false"})
			a := gateJourneyStart(t, h, "feature/pending-response")
			gateJourneyAwaitSlots(t, h, func(s ipc.SlotsResult) bool { return s.InUse == 0 && s.ParkedReleased == 1 })
			// A newer, ordinary review gate occupies the only slot, before and
			// after restart (recovery visits the newest run first).
			if err := config.SetGlobalDaemonSetting(filepath.Join(h.NMHome, "config.yaml"), "hold_slot_while_parked", "true"); err != nil {
				t.Fatal(err)
			}
			client, err := ipc.Dial(paths.WithRoot(h.NMHome).Socket())
			if err != nil {
				t.Fatal(err)
			}
			var slots ipc.SlotsResult
			err = client.Call(ipc.MethodReloadSlots, nil, &slots)
			client.Close()
			if err != nil || !slots.HoldSlotWhileParked {
				t.Fatalf("enable slot holder: %+v, %v", slots, err)
			}
			h.CommitChange("feature/slot-holder", "feature.txt", "holder = true\n", "occupy a slot")
			if out, err := h.Run("axi", "run", "--intent", axiIntent); err != nil {
				t.Fatalf("start slot holder: %v\n%s", err, out)
			}
			holder := h.ActiveRun("feature/slot-holder")
			if holder == nil {
				t.Fatal("slot holder did not start")
			}
			gateJourneyWait(t, h, holder.ID, "")
			gateJourneyAwaitSlots(t, h, func(s ipc.SlotsResult) bool { return s.InUse == 1 })
			socket := paths.WithRoot(h.NMHome).Socket()
			if result := gateJourneyRespond(t, socket, a.RunID, types.ActionAnswer, &a); !result.OK {
				t.Fatalf("accept while slot occupied: %+v", result)
			}
			raw := gateJourneyPending(t, h, a)
			var payload map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				t.Fatal(err)
			}
			var savedGate types.GateIdentity
			if err := json.Unmarshal(payload["gate"], &savedGate); err != nil || savedGate != a {
				t.Fatalf("pending response lost identity: %+v, %v", savedGate, err)
			}
			gateJourneyDaemon(t, h, "stop")
			switch mode {
			case "missing-identity":
				delete(payload, "gate")
			case "superseded-identity":
				savedGate.Nonce = "a-previous-park"
				payload["gate"], err = json.Marshal(savedGate)
			}
			if mode == "missing-identity" || mode == "superseded-identity" {
				encoded, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				gateJourneyReplacePending(t, h, a, string(encoded))
			}
			gateJourneyDaemon(t, h, "start")
			if mode == "missing-identity" || mode == "superseded-identity" {
				run := h.WaitForRun("feature/pending-response", 30*time.Second)
				if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "superseded") {
					t.Fatalf("invalid pending response did not fail closed: %+v", run)
				}
				gateJourneyEvidence(t, "recovered invalid pending run", run)
				if len(gateJourneyRounds(t, h, a)) != 1 {
					t.Fatal("invalid pending response dispatched another turn")
				}
				t.Logf("scenario 4 (%s): restart refused the persisted response without dispatch", mode)
				return
			}
			gateJourneyAwaitSlots(t, h, func(s ipc.SlotsResult) bool { return s.InUse == 1 && s.Queued >= 1 })
			recovered := h.RunInfo(a.RunID)
			step, _ := findStep(recovered.Steps, types.StepReview)
			if !recovered.WaitingForSlot || !step.ResponsePending || len(gateJourneyRounds(t, h, a)) != 1 {
				t.Fatalf("recovered response not waiting inertly for capacity: %+v", recovered)
			}
			gateJourneyEvidence(t, "recovered run waiting for capacity", recovered)
			if got := gateJourneyPending(t, h, a); got != raw {
				t.Fatalf("restart changed the pending response: %s, want %s", got, raw)
			}
			if mode == "superseded-during-slot-wait" {
				// The response was registered successfully, but authority moved
				// while it waited for capacity. Slot dispatch must re-check it.
				database, err := db.Open(filepath.Join(h.NMHome, "state.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				row, err := database.GetStepResult(a.StepResultID)
				if err != nil {
					t.Fatal(err)
				}
				err = database.ParkStepForApproval(a.RunID, a.StepResultID, a.Status, 0, 1, row.FindingsJSON, a.RoundID)
				database.Close()
				if err != nil {
					t.Fatal(err)
				}
				gateJourneyReplacePending(t, h, a, raw)
			}
			if result := gateJourneyRespond(t, socket, holder.ID, types.ActionAbort, nil); !result.OK {
				t.Fatalf("release slot holder: %+v", result)
			}
			if mode == "superseded-during-slot-wait" {
				run := h.WaitForRun("feature/pending-response", 30*time.Second)
				if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "superseded") || len(gateJourneyRounds(t, h, a)) != 1 {
					t.Fatalf("slot dispatch reused stale response: %+v", run)
				}
				gateJourneyEvidence(t, "superseded slot dispatch run", run)
				t.Log("scenario 4: gate changed after recovery registration; slot dispatch refused stale authority")
				return
			}
			b := gateJourneyWait(t, h, a.RunID, a.Nonce)
			rounds := gateJourneyRounds(t, h, b)
			if len(rounds) != 2 || rounds[1].Trigger != "answer" {
				t.Fatalf("recovery did not dispatch the original response exactly once: %+v", rounds)
			}
			t.Log("scenario 4: accepted response survived restart and waiting for a slot, then dispatched once when capacity freed")
		})
	}
}
