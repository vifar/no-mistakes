package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #158: the CLI's own pre-check is one of the callers that must not
// drift from the shared eligibility rule. A non-strict run has no shipping
// options, so the CLI must forward an owner-approved gap to the daemon
// unchanged - the daemon, not the CLI, decides whether the gap is eligible.

func TestAxiRespond_ForwardsAnAcceptedGapOnANonStrictRun(t *testing.T) {
	var got ipc.RespondParams
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			if err := json.Unmarshal(raw, &got); err != nil {
				return nil, err
			}
			responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		run := fx.awaiting()
		run.Steps[0].StepName = types.StepTest
		// A plain run: no shipping options, so the run is not strict.
		run.Shipping = nil
		if responded.Load() {
			run.Status = types.RunCompleted
		}
		return run, nil
	})
	out, err := executeCmd("axi", "respond", "--action", "approve", "--accept-gap", "test-verdict", "--reason", acceptGapReason, "--covered-by", acceptGapCoveredBy, "--wait", "1m")
	if err != nil {
		t.Fatalf("a non-strict run refused the accepted gap at the CLI: %v\n%s", err, out)
	}
	if !responded.Load() {
		t.Fatal("the accepted gap never reached the daemon")
	}
	if got.Action != types.ActionApprove || got.Step != types.StepTest || strings.Join(got.AcceptGaps, "|") != "test-verdict" || got.ApprovalReason != acceptGapReason || got.CoveredBy != acceptGapCoveredBy {
		t.Fatalf("forwarded params = %+v", got)
	}
}

// A non-strict Test gate parked on an eligible inconclusive verdict names the
// accepted-gap command: without it the operator has no way to discover the
// command that finishes the run.
func TestGateHelpOffersTheAcceptedGapCommandForANonStrictTest(t *testing.T) {
	out := axiDoc(gateFieldsFor(acceptGapGate(t), "run-7", false)...)
	for _, want := range []string{"--run run-7 --action approve --accept-gap test-verdict", `--reason`, `<who approved, where, when>`, `--covered-by`, `<tests>`, "owner has explicitly approved"} {
		if !strings.Contains(out, want) {
			t.Fatalf("non-strict gate help missing %q:\n%s", want, out)
		}
	}
	// A plain approve is still a plain approve: it stays on offer, and the
	// help says what it costs.
	if !strings.Contains(out, "to accept this step and continue") {
		t.Fatalf("non-strict gate help dropped the plain approve:\n%s", out)
	}
}
