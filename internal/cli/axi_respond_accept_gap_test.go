package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	acceptGapReason    = "owner approved via first mate, 2026-10-14"
	acceptGapCoveredBy = "TestBrokerTimeoutMocked; internal/broker/timeout_test.go"
)

func TestAxiRespond_AcceptGapFlagValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"covered-by without accept-gap", []string{"--action", "approve", "--step", "test", "--reason", acceptGapReason, "--covered-by", "TestX"}, "--covered-by applies only with --accept-gap"},
		{"fix action", []string{"--action", "fix", "--findings", "test-verdict", "--step", "test", "--accept-gap", "test-verdict", "--reason", acceptGapReason, "--covered-by", "TestX"}, "--accept-gap applies only to --action approve"},
		{"skip action", []string{"--action", "skip", "--step", "test", "--accept-gap", "test-verdict", "--reason", acceptGapReason, "--covered-by", "TestX"}, "--accept-gap applies only to --action approve"},
		{"missing reason", []string{"--action", "approve", "--step", "test", "--accept-gap", "test-verdict", "--covered-by", "TestX"}, "--reason of at least 10 characters"},
		{"short reason", []string{"--action", "approve", "--step", "test", "--accept-gap", "test-verdict", "--reason", "ok", "--covered-by", "TestX"}, "--reason of at least 10 characters"},
		{"missing covered-by", []string{"--action", "approve", "--step", "test", "--accept-gap", "test-verdict", "--reason", acceptGapReason}, "non-empty --covered-by"},
		{"blank covered-by", []string{"--action", "approve", "--step", "test", "--accept-gap", "test-verdict", "--reason", acceptGapReason, "--covered-by", "  "}, "non-empty --covered-by"},
		{"NEL in reason", []string{"--action", "approve", "--step", "test", "--accept-gap", "test-verdict", "--reason", "owner approved\u0085via first mate", "--covered-by", "TestX"}, "single line"},
		{"line separator in reason", []string{"--action", "approve", "--step", "test", "--accept-gap", "test-verdict", "--reason", "owner approved\u2028via first mate", "--covered-by", "TestX"}, "single line"},
		{"paragraph separator in covered-by", []string{"--action", "approve", "--step", "test", "--accept-gap", "test-verdict", "--reason", "owner approved via first mate", "--covered-by", "TestX\u2029TestY"}, "single line"},
		{"multiline reason", []string{"--action", "approve", "--step", "test", "--accept-gap", "test-verdict", "--reason", "owner approved\nvia first mate", "--covered-by", "TestX"}, "single line"},
		{"no ids", []string{"--action", "approve", "--step", "test", "--accept-gap", " , ", "--reason", acceptGapReason, "--covered-by", "TestX"}, "at least one finding id"},
		{"not the Test step", []string{"--action", "approve", "--step", "review", "--accept-gap", "test-verdict", "--reason", acceptGapReason, "--covered-by", "TestX"}, "applies only to the Test step"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var responded atomic.Bool
			fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
				respond: func(context.Context, json.RawMessage) (interface{}, error) {
					responded.Store(true)
					return &ipc.RespondResult{OK: true}, nil
				},
			})
			fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) { return fx.awaiting(), nil })
			out, err := executeCmd(append([]string{"axi", "respond", "--wait", "3s"}, tt.args...)...)
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 2 {
				t.Fatalf("error = %v, want usage exit 2\n%s", err, out)
			}
			if !strings.Contains(out, tt.want) {
				t.Fatalf("output missing %q:\n%s", tt.want, out)
			}
			if responded.Load() {
				t.Fatal("an invalid accepted-gap response reached the daemon")
			}
		})
	}
}

func TestAxiRespond_ForwardsAnAcceptedGapVerbatim(t *testing.T) {
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
	parked := func() *ipc.RunInfo {
		run := fx.awaiting()
		run.Steps[0].StepName = types.StepTest
		return run
	}
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if responded.Load() {
			return fx.completed(), nil
		}
		return parked(), nil
	})
	// This checks the forwarded audit payload, not the wait deadline. Allow a
	// busy test host enough time for the local Git and IPC round trips.
	out, err := executeCmd("axi", "respond", "--action", "approve", "--accept-gap", "test-verdict, test-2", "--reason", acceptGapReason, "--covered-by", acceptGapCoveredBy, "--wait", "1m")
	if err != nil {
		t.Fatalf("accepted gap refused: %v\n%s", err, out)
	}
	if got.Action != types.ActionApprove || got.Step != types.StepTest || strings.Join(got.AcceptGaps, "|") != "test-verdict|test-2" || got.ApprovalReason != acceptGapReason || got.CoveredBy != acceptGapCoveredBy {
		t.Fatalf("forwarded params = %+v", got)
	}
}

func acceptGapGate(t *testing.T, items ...types.Finding) stepView {
	t.Helper()
	verdict := types.Finding{ID: types.FindingIDTestVerdict, Category: types.FindingCategoryTestVerdict, Severity: types.FindingSeverityWarning, Action: types.ActionAskUser, Description: "live validation verdict: inconclusive"}
	raw, err := types.MarshalFindingsJSON(types.Findings{Items: append([]types.Finding{verdict}, items...), Verdict: types.TestVerdictInconclusive})
	if err != nil {
		t.Fatal(err)
	}
	return stepView{Name: "test", Status: "awaiting_approval", FindingsJSON: raw}
}

func TestGateHelpOffersTheAcceptedGapCommandForEligibleStrictTest(t *testing.T) {
	out := axiDoc(gateFieldsFor(acceptGapGate(t), "run-7", true)...)
	for _, want := range []string{"--run run-7 --action approve --accept-gap test-verdict", `--reason`, `<who approved, where, when>`, `--covered-by`, `<tests>`, "owner has explicitly approved"} {
		if !strings.Contains(out, want) {
			t.Fatalf("strict gate help missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "to accept this step and continue") {
		t.Fatalf("strict gate still suggests a plain approve:\n%s", out)
	}
}

func TestGateHelpKeepsPlainApproveWhereAGapIsNotEligible(t *testing.T) {
	for name, out := range map[string]string{
		"non-strict run":    axiDoc(gateFieldsFor(acceptGapGate(t), "", false)...),
		"other unresolved":  axiDoc(gateFieldsFor(acceptGapGate(t, types.Finding{ID: "test-2", Severity: "error", Action: types.ActionAskUser, Description: "defect"}), "", true)...),
		"command exception": axiDoc(gateFieldsFor(acceptGapGate(t, types.Finding{ID: "cmd", Category: types.FindingCategoryTestCommand, Severity: "error", Action: types.ActionAutoFix, Description: "failed"}), "", true)...),
	} {
		if strings.Contains(out, "--accept-gap") {
			t.Fatalf("%s: gate help offers an accepted gap that would be refused:\n%s", name, out)
		}
	}
}

func TestGateHelpAfterACrashOffersTheRecoveryApproval(t *testing.T) {
	gate := acceptGapGate(t)
	info := &ipc.RunInfo{ID: "run-7", Status: types.RunRunning, Shipping: &types.ShippingOptions{Mode: types.ShippingFull}, Steps: []ipc.StepResultInfo{{
		StepName: types.StepTest, Status: types.StepStatusAwaitingApproval, FindingsJSON: &gate.FindingsJSON, AcceptedGapRecorded: true,
	}}}
	rv := runViewFromIPC(info)
	parked, ok := rv.awaitingStep()
	if !ok || !parked.AcceptedGapRecorded {
		t.Fatalf("recorded gap did not reach the step view: %+v", parked)
	}
	out := axiDoc(gateFieldsFor(parked, "run-7", true)...)
	if !strings.Contains(out, "already recorded") || !strings.Contains(out, "--run run-7 --action approve` with no --reason") || !strings.Contains(out, "reused unchanged") {
		t.Fatalf("recovery help missing:\n%s", out)
	}
	if strings.Contains(out, "--accept-gap test-verdict") || strings.Contains(out, "<who approved") {
		t.Fatalf("recovery help still asks for a new reason and coverage:\n%s", out)
	}
	// Without the record the gate keeps asking for the full command.
	parked.AcceptedGapRecorded = false
	if out := axiDoc(gateFieldsFor(parked, "run-7", true)...); !strings.Contains(out, "--accept-gap test-verdict") {
		t.Fatalf("unrecorded gate lost the accept-gap command:\n%s", out)
	}
}

func TestAxiRespond_RefusesAnAcceptedGapOnADaemonThatPredatesThem(t *testing.T) {
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{
		withoutAcceptedGaps: true,
		respond: func(context.Context, json.RawMessage) (interface{}, error) {
			responded.Store(true)
			return &ipc.RespondResult{OK: true}, nil
		},
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		run := fx.awaiting()
		run.Steps[0].StepName = types.StepTest
		return run, nil
	})
	out, err := executeCmd("axi", "respond", "--action", "approve", "--accept-gap", "test-verdict", "--reason", acceptGapReason, "--covered-by", acceptGapCoveredBy, "--wait", "3s")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("error = %v, want usage exit 2\n%s", err, out)
	}
	if !strings.Contains(out, "the daemon predates accepted gaps; restart it with `no-mistakes daemon restart`") {
		t.Fatalf("output missing the capability refusal:\n%s", out)
	}
	if responded.Load() {
		t.Fatal("the response reached a daemon that would have dropped the accepted gap")
	}
}
