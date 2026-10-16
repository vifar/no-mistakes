package tui

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestModel_ResponseBindsTheDisplayedParkEvent(t *testing.T) {
	sock := testSocketPath(t)
	srv := startTestIPCServer(t, sock)
	got := make(chan ipc.RespondParams, 1)
	srv.Handle(ipc.MethodRespond, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var p ipc.RespondParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		got <- p
		return &ipc.RespondResult{OK: true}, nil
	})
	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	run := testRun()
	m := NewModel(sock, client, run)
	g := types.GateIdentity{RunID: run.ID, Step: types.StepReview, StepResultID: "step", RoundID: "round", Nonce: "park-1", Status: types.StepStatusAwaitingApproval}
	step, status := g.Step, string(g.Status)
	m.applyEvent(ipc.Event{Type: ipc.EventStepCompleted, RunID: run.ID, StepName: &step, Status: &status, Gate: &g})
	cmd := m.respondCmd(types.ActionApprove)
	if cmd == nil {
		t.Fatal("no response command")
	}
	// A later event must not retarget the already queued keypress.
	laterEvent := g
	laterEvent.Nonce = "new-park"
	m.applyEvent(ipc.Event{Type: ipc.EventStepCompleted, RunID: run.ID, StepName: &step, Status: &status, Gate: &laterEvent})
	if msg := cmd(); msg != nil {
		t.Fatalf("respond: %+v", msg)
	}
	p := <-got
	if p.ExpectedGate == nil || *p.ExpectedGate != g {
		t.Fatalf("response did not bind displayed event: %+v", p)
	}
	old, new := ipc.StepResultInfo{Gate: &g}, ipc.StepResultInfo{Gate: &g}
	later := g
	later.Nonce = "park-2"
	new.Gate = &later
	if sameGateState(&old, &new, nil, nil) {
		t.Fatal("same-round repark preserved stale TUI selection")
	}
}
