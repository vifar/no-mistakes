package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
	toon "github.com/toon-format/toon-go"
)

func TestAxiOwnerInput(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		args      []string
		want      string
		invalid   bool
	}{
		{name: "absent"}, {name: "environment", env: "thread-42", want: "thread-42"},
		{name: "flag wins", env: "old-thread", args: []string{"--owner", "  Firstmate  "}, want: "Firstmate"},
		{name: "explicit empty", env: "thread-42", args: []string{"--owner="}},
		{name: "invalid control", args: []string{"--owner", "thread\n42"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NO_MISTAKES_OWNER", tc.env)
			cmd := newAxiRunCmd()
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatal(err)
			}
			owner, err := launchOwner(cmd)
			if (err != nil) != tc.invalid || owner != tc.want {
				t.Fatalf("owner %q, error %v; want %q, invalid=%v", owner, err, tc.want, tc.invalid)
			}
			if err == nil {
				got, err := parseOwnerPushOptions([]string{formatOwnerPushOption(owner)})
				if err != nil || got != owner {
					t.Fatalf("hook transport: %q, %v", got, err)
				}
			}
		})
	}
	if _, err := parseOwnerPushOptions([]string{formatOwnerPushOption("a"), formatOwnerPushOption("b")}); err == nil {
		t.Fatal("duplicate owners accepted")
	}
	if err := types.ValidateRunOwner(strings.Repeat("x", 257)); err == nil {
		t.Fatal("oversized owner accepted")
	}
}

func TestAxiAwaitingRequiresLiveGate(t *testing.T) {
	since := int64(100)
	gate := types.GateIdentity{RunID: "run", StepResultID: "step", Step: types.StepReview, RoundID: "round", Nonce: "nonce", Status: types.StepStatusAwaitingApproval}
	for _, tc := range []struct {
		name, status             string
		gate                     *types.GateIdentity
		terminal, answered, want bool
	}{
		{name: "parked", status: "awaiting_approval", gate: &gate, want: true},
		{name: "no gate", status: "awaiting_approval"},
		{name: "completed stale gate", status: "completed", gate: &gate},
		{name: "terminal run", status: "awaiting_approval", gate: &gate, terminal: true},
		{name: "already answered", status: "awaiting_approval", gate: &gate, answered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rv := runView{ID: "run", Status: "running", AwaitingAgentSince: &since, Steps: []stepView{{ID: "step", Name: "review", Status: tc.status, Gate: tc.gate, FindingsJSON: `{"summary":"stale finding","findings":[]}`}}}
			if tc.terminal {
				rv.Status = "completed"
			}
			if tc.answered {
				rv.AnsweredStep = "review"
			}
			var doc struct {
				Run struct {
					Awaiting bool `toon:"awaiting_person"`
				} `toon:"run"`
			}
			if err := toon.UnmarshalString(axiDoc(runObjectField(rv)), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Run.Awaiting != tc.want {
				t.Fatalf("awaiting_person=%v; want %v", doc.Run.Awaiting, tc.want)
			}
		})
	}
}

func TestOwnerReceiptClaimsAndOutput(t *testing.T) {
	for _, owner := range []string{"Firstmate", ""} {
		t.Run("owner="+owner, func(t *testing.T) {
			olderDaemonFixture(t, nil, func(srv *ipc.Server) {
				srv.Handle(ipc.MethodProbeRunOwner, func(context.Context, json.RawMessage) (interface{}, error) {
					return &ipc.ProbeRunOwnerResult{OK: true}, nil
				})
				srv.HandleStream(ipc.MethodSubscribe, hangSubscribe)
				srv.Handle(ipc.MethodClaimLaunchReceipt, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
					var params map[string]any
					if err := json.Unmarshal(raw, &params); err != nil {
						return nil, err
					}
					if got, _ := params["owner"].(string); got != owner {
						return nil, fmt.Errorf("conflicting owner: got %q, want %q", got, owner)
					}
					return &ipc.ClaimLaunchReceiptResult{Receipt: &ipc.LaunchReceipt{RunID: "owned-run", Owner: owner, Disposition: "reused"}}, nil
				})
				srv.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
					return &ipc.GetRunResult{Run: &ipc.RunInfo{ID: "owned-run", Status: types.RunCompleted}}, nil
				})
			})
			writeGlobalConfig(t, "")
			t.Setenv("NO_MISTAKES_OWNER", "")
			cmd := newAxiRunCmd()
			cmd.SetArgs([]string{"--intent", "intent", "--owner=" + owner, "--launch-nonce", "nonce", "--validation-generation", "generation"})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Receipt map[string]any `toon:"launch_receipt"`
			}
			if err := toon.UnmarshalString(out.String(), &doc); err != nil {
				t.Fatal(err)
			}
			if got, ok := doc.Receipt["owner"]; !ok || got != owner {
				t.Errorf("stdout owner = %v, present=%v; want %q", got, ok, owner)
			}
			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			ctx := types.WithRunOwner(context.Background(), owner)
			receipt, err := waitForLaunchReceipt(ctx, client, "repo", "branch", "nonce", "head", "generation", "intent", "", false, "", time.Second)
			if err != nil || receipt == nil || receipt.Owner != owner {
				t.Fatalf("poll receipt = %+v, %v", receipt, err)
			}
			for _, requested := range []string{"Other", ""} {
				if requested == owner {
					continue
				}
				cmd := newAxiRunCmd()
				cmd.SetArgs([]string{"--intent", "intent", "--owner=" + requested, "--launch-nonce", "nonce", "--validation-generation", "generation"})
				cmd.SetOut(io.Discard)
				cmd.SetErr(io.Discard)
				if err := cmd.Execute(); err == nil {
					t.Errorf("initial replay accepted owner %q for %q", requested, owner)
				}
				ctx := types.WithRunOwner(context.Background(), requested)
				if receipt, err := waitForLaunchReceipt(ctx, client, "repo", "branch", "nonce", "head", "generation", "intent", "", false, "", time.Second); err == nil {
					t.Errorf("poll accepted owner %q for %q: %+v", requested, owner, receipt)
				}
			}

		})
	}
}
