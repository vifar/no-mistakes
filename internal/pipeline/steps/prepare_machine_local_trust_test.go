package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// Exercise the selection-to-execution boundary. The oracle is the operator's
// gap-fill contract: only a matching global override can fill an empty effective
// command; commands authorized by trusted opt-in retain their existing semantics.
// A wrong trust decision changes the dependency visible at the first agent turn.
// This is a component integration test, not a live CLI journey.
func TestMachineLocalPrepareTrustSelectionReachesAgentEntry(t *testing.T) {
	const remote = "https://github.com/acme/widget.git"
	const machine = "mkdir -p .deps && echo machine > .deps/source"
	const trusted = "mkdir -p .deps && echo trusted > .deps/source"
	const pushed = "mkdir -p .deps && echo pushed > .deps/source"
	for _, tc := range []struct {
		name       string
		remote     string
		trusted    string
		pushed     string
		wantSource string
		wantCmd    string
	}{
		{"matching_gap", remote, "", "", "machine", machine},
		{"unmatched", "https://github.com/acme/other.git", "", "", "", ""},
		{"trusted_lazy", remote, "commands:\n  prepare: " + trusted + "\n", "", "", trusted},
		{"trusted_eager", remote, "test:\n  prepare: true\ncommands:\n  prepare: " + trusted + "\n", "", "trusted", trusted},
		{"default_blocks_pushed_command", remote, "", "test:\n  prepare: true\ncommands:\n  prepare: " + pushed + "\n", "machine", machine},
		{"pushed_cannot_self_enable", remote, "", "allow_repo_commands: true\ntest:\n  prepare: true\ncommands:\n  prepare: " + pushed + "\n", "machine", machine},
		{"opt_in_authorizes_pushed_command", remote, "allow_repo_commands: true\ntest:\n  prepare: true\n", "commands:\n  prepare: " + pushed + "\n", "pushed", pushed},
		{"default_ignores_pushed_override", remote, "", "repository_overrides:\n  " + remote + ":\n    prepare: " + pushed + "\n", "machine", machine},
		{"opt_in_ignores_pushed_override", remote, "allow_repo_commands: true\n", "repository_overrides:\n  " + remote + ":\n    prepare: " + pushed + "\n", "machine", machine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global, err := config.LoadGlobalFromBytes([]byte("repository_overrides:\n  " + remote + ":\n    prepare: " + machine + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			trustedCfg, err := config.LoadRepoFromBytes([]byte(tc.trusted))
			if err != nil {
				t.Fatal(err)
			}
			pushedCfg, err := config.LoadRepoFromBytes([]byte(tc.pushed))
			if err != nil {
				t.Fatal(err)
			}
			effective := config.EffectiveRepoConfig(pushedCfg, trustedCfg, trustedCfg.AllowRepoCommands)
			cfg := config.MergeForRemote(global, effective, tc.remote)
			if cfg.Commands.Prepare != tc.wantCmd {
				t.Fatalf("prepare = %q, want %q", cfg.Commands.Prepare, tc.wantCmd)
			}
			dir, base, head := setupGitRepo(t)
			ignoreTestDependencies(t, dir)
			calls := 0
			ag := &mockAgent{name: "prepare-entry", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				calls++
				data, err := os.ReadFile(filepath.Join(opts.CWD, ".deps", "source"))
				if tc.wantSource == "" {
					if !os.IsNotExist(err) {
						t.Fatalf("unexpected eager preparation: source=%q err=%v", data, err)
					}
				} else if err != nil || strings.TrimSpace(string(data)) != tc.wantSource {
					t.Fatalf("first agent turn source=%q err=%v, want %q", data, err, tc.wantSource)
				}
				t.Logf("first agent turn: dependency source=%q", strings.TrimSpace(string(data)))
				return &agent.Result{Output: json.RawMessage(preparationNoSurface)}, nil
			}}
			sctx := newPreparationTestContext(t, ag, dir, base, head, cfg.Commands)
			sctx.Config = cfg
			sctx.Shared = &pipeline.RunShared{}
			if _, err := (&TestStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("agent turns=%d, want 1", calls)
			}
		})
	}
}
