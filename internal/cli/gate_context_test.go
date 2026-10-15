package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestGateControlPolicyCoversEveryMutationEntrypoint(t *testing.T) {
	root := newRootCmd()
	cases := []struct {
		args    []string
		mutates bool
	}{
		{args: nil, mutates: true},
		{args: []string{"init"}, mutates: true},
		{args: []string{"eject"}, mutates: true},
		{args: []string{"rerun"}, mutates: true},
		{args: []string{"sync"}, mutates: true},
		{args: []string{"sync", "--recover"}, mutates: true},
		{args: []string{"sync", "--check"}, mutates: false},
		{args: []string{"axi", "run"}, mutates: true},
		{args: []string{"axi", "respond"}, mutates: true},
		{args: []string{"axi", "sync"}, mutates: true},
		{args: []string{"axi", "sync", "--recover"}, mutates: true},
		{args: []string{"axi", "sync", "--check"}, mutates: false},
		{args: []string{"axi", "abort"}, mutates: true},
		{args: []string{"axi", "status"}, mutates: false},
		{args: []string{"axi", "logs"}, mutates: false},
		{args: []string{"status"}, mutates: false},
		{args: []string{"doctor"}, mutates: false},
		{args: []string{"daemon", "stop", "--force"}, mutates: true},
		{args: []string{"daemon", "set", "max-concurrent-runs", "0"}, mutates: true},
		{args: []string{"daemon", "uninstall", "--force"}, mutates: true},
	}
	for _, tc := range cases {
		cmd, _, err := root.Find(tc.args)
		if err != nil {
			t.Fatalf("find %v: %v", tc.args, err)
		}
		if contains(tc.args, "--check") {
			_ = cmd.Flags().Set("check", "true")
		} else if cmd.Flags().Lookup("check") != nil {
			_ = cmd.Flags().Set("check", "false")
		}
		if got := mutatesPipelineControl(cmd); got != tc.mutates {
			t.Errorf("%v mutates = %v, want %v (path=%s)", tc.args, got, tc.mutates, cmd.CommandPath())
		}
	}
}

func TestNestedDaemonSetRefusesBeforeWritingConfig(t *testing.T) {
	root := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", root)
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	before := []byte("daemon:\n  max_concurrent_runs: 3\n")
	if err := os.WriteFile(p.ConfigFile(), before, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.HealthResult{Status: "ok"}, nil
	})
	srv.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GateContextResult{Nested: true, AgentDescendant: true, RunID: "outer", Phase: types.StepReview}, nil
	})
	startIPCServer(t, srv, p.Socket())
	client := dialReady(t, p.Socket())
	client.Close()
	out, err := executeCmd("daemon", "set", "max-concurrent-runs", "0")
	if err == nil || !strings.Contains(out, "nested_gate_context") {
		t.Fatalf("nested command: %v\n%s", err, out)
	}
	after, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("nested command changed global config: %q", after)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestGateContextRefusalIsStructuredActionableAndPrivacySafe(t *testing.T) {
	cmd := newAxiRunCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := emitGateContextRefusal(cmd, gatecontext.Result{Nested: true, ManagedGit: true, AgentDescendant: true, RunID: "run-safe", Phase: types.StepDocument})
	if err == nil {
		t.Fatal("expected refusal exit error")
	}
	text := out.String()
	for _, want := range []string{
		"code: nested_gate_context",
		"run: run-safe",
		"phase: document",
		"enclosing executor owns validation, push, PR, and CI",
		"no-mistakes axi status",
		"Return control to the outer executor",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("refusal missing %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"agent_pid", "peer_pid", "/worktrees/", "/repos/"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("refusal leaked %q:\n%s", forbidden, text)
		}
	}
}
