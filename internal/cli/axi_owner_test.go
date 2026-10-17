package cli

import (
	"strings"
	"testing"

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
