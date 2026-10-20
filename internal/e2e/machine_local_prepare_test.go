//go:build e2e && !windows

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The oracle is the operator's gap-fill contract: repository preparation wins,
// pushed configuration cannot self-authorize, and only a matching global entry
// supplies eager preparation. Observe the real daemon's shell and agent entry,
// rather than trusting the fake agent's canned verdict as evidence of setup.
func TestMachineLocalPrepareJourney(t *testing.T) {
	for _, tc := range []struct {
		name, trusted, pushed, first, final string
		allow, unmatched, noMachine, fail  bool
	}{
		{name: "matching_agent_only", first: "machine", final: "machine"},
		{name: "unmatched", unmatched: true, first: "absent", final: "absent"},
		{name: "trusted_lazy", trusted: "commands:\n  prepare: nm-prepare-trusted\n", first: "absent", final: "trusted"},
		{name: "trusted_eager", trusted: "commands:\n  prepare: nm-prepare-trusted\ntest:\n  prepare: true\n", first: "trusted", final: "trusted"},
		{name: "pushed_cannot_self_enable", pushed: "allow_repo_commands: true\ncommands:\n  prepare: nm-prepare-pushed\ntest:\n  prepare: true\n", first: "machine", final: "machine"},
		{name: "authorized_pushed_command", allow: true, trusted: "test:\n  prepare: true\n", pushed: "commands:\n  prepare: nm-prepare-pushed\n", first: "pushed", final: "pushed"},
		{name: "pushed_override_default", pushed: "repository_overrides:\n  " + machineLocalRemote + ":\n    prepare: nm-prepare-pushed\n", first: "machine", final: "machine"},
		{name: "pushed_override_opt_in", allow: true, pushed: "repository_overrides:\n  " + machineLocalRemote + ":\n    prepare: nm-prepare-pushed\n", first: "machine", final: "machine"},
		{name: "pushed_override_cannot_create_default", noMachine: true, pushed: "repository_overrides:\n  " + machineLocalRemote + ":\n    prepare: nm-prepare-pushed\n", first: "absent", final: "absent"},
		{name: "pushed_override_cannot_create_opt_in", noMachine: true, allow: true, pushed: "repository_overrides:\n  " + machineLocalRemote + ":\n    prepare: nm-prepare-pushed\n", first: "absent", final: "absent"},
		{name: "preparation_failure", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", AllowRepoCommands: &tc.allow})
			observed := filepath.Join(filepath.Dir(h.AgentLog), "agent-entry")
			calls := filepath.Join(filepath.Dir(h.AgentLog), "prepare-calls")
			// Keep the native adapter and wire protocol, instrumenting only its
			// external process boundary to inspect the worktree before any reply.
			realDir := filepath.Join(h.BinDir, "delegate")
			if err := os.Mkdir(realDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(h.FakeAgent, filepath.Join(realDir, "claude")); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(h.BinDir, "claude")); err != nil {
				t.Fatal(err)
			}
			writeMachineLocalScript(t, filepath.Join(h.BinDir, "claude"), `set -eu
prompt=$(cat)
case "$prompt" in
  *'`+testStepPromptMarker+`'*)
    source=absent
    if [ -f .deps/source ]; then source=$(cat .deps/source); fi
    printf '%s\n' "$source" >> `+shellQuote(observed)+`
    test -z "$(git status --porcelain)"
    test ! -e prepare-stray
    test ! -e prepare-nested
    ;;
esac
printf '%s' "$prompt" | `+shellQuote(filepath.Join(realDir, "claude"))+` "$@"
`)
			for _, source := range []string{"machine", "trusted", "pushed"} {
				body := "set -eu\nmkdir -p .deps\necho " + source + " > .deps/source\necho " + source + " >> " + shellQuote(calls) + "\n" +
					"echo mutation > README.md\ngit add README.md\necho stray > prepare-stray\ngit init -q prepare-nested\n"
				if tc.fail {
					body += "exit 23\n"
				}
				writeMachineLocalScript(t, filepath.Join(h.BinDir, "nm-prepare-"+source), body)
			}
			writeMachineLocalScript(t, filepath.Join(h.BinDir, "nm-assert-deps"), "set -eu\n"+
				"source=absent\nif [ -f .deps/source ]; then source=$(cat .deps/source); fi\n"+
				"test \"$source\" = "+shellQuote(tc.final)+"\ntest -z \"$(git status --porcelain)\"\ntest ! -e prepare-stray\ntest ! -e prepare-nested\n")
			h.CommitChange("main", ".gitignore", ".deps/\n", "ignore dependencies")
			trusted := fmt.Sprintf("allow_repo_commands: %t\n", tc.allow) + tc.trusted
			if !strings.Contains(tc.trusted, "commands:") {
				trusted += "commands:\n  lint: nm-assert-deps\n"
			} else {
				trusted = strings.Replace(trusted, "commands:\n", "commands:\n  lint: nm-assert-deps\n", 1)
			}
			pushMainRepoConfig(t, h, trusted)
			remote := machineLocalRemote
			if tc.unmatched {
				remote = "https://github.com/acme/other.git"
			}
			global := filepath.Join(h.NMHome, "config.yaml")
			data, err := os.ReadFile(global)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.noMachine {
				if err := os.WriteFile(global, append(data, []byte("repository_overrides:\n  "+remote+":\n    prepare: nm-prepare-machine\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			setupMachineLocalCommands(t, h, "")
			const branch = "feature/prepare-journey"
			if tc.pushed != "" {
				pushed := tc.pushed
				if tc.allow {
					if strings.Contains(pushed, "commands:\n") {
						pushed = strings.Replace(pushed, "commands:\n", "commands:\n  lint: nm-assert-deps\n", 1)
					} else {
						pushed += "commands:\n  lint: nm-assert-deps\n"
					}
				}
				h.CommitChange(branch, ".no-mistakes.yaml", pushed, "supply pushed configuration")
			}
			h.CommitChange(branch, "feature.txt", "feature\n", "add feature")
			h.PushToGate(branch)
			run := h.WaitForRun(branch, 120*time.Second)
			log := readStepLog(t, h, run.ID, string(types.StepTest))
			t.Logf("run=%s status=%s error=%s\ntest log:\n%s", run.ID, run.Status, deref(run.Error), log)
			if tc.fail {
				if run.Status != types.RunFailed || !strings.Contains(deref(run.Error), "prepare command exited with code 23") {
					t.Fatalf("preparation failure did not stop the run: %+v", run)
				}
				if _, err := os.Stat(observed); !os.IsNotExist(err) {
					t.Fatalf("agent entered after failed preparation: %v", err)
				}
				return
			}
			if run.Status != types.RunCompleted {
				t.Fatalf("run did not complete: %s %s", run.Status, deref(run.Error))
			}
			entries, err := os.ReadFile(observed)
			if err != nil || strings.TrimSpace(string(entries)) != tc.first {
				t.Fatalf("first Test agent saw %q, error=%v; want %s", entries, err, tc.first)
			}
			prepared, err := os.ReadFile(calls)
			if tc.final == "absent" {
				if !os.IsNotExist(err) {
					t.Fatalf("unmatched preparation executed: %q %v", prepared, err)
				}
			} else if err != nil || string(prepared) != tc.final+"\n" {
				t.Fatalf("prepare executions=%q, error=%v; want one %s", prepared, err, tc.final)
			}
			declared := strings.Contains(log, "machine-local prepare command applied")
			if declared != (tc.first == "machine") {
				t.Fatalf("machine-local declaration=%t for first source %s", declared, tc.first)
			}
			t.Logf("first agent source=%s; preparation executions=%q; lint verified retained dependencies and clean worktree", tc.first, prepared)
		})
	}
}

func TestMachineLocalPrepareExistingPRRoutingJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	state := setupStatefulGitHub(t, h)
	h.CommitChange("main", ".gitignore", ".deps/\n", "ignore dependencies")
	pushMainRepoConfig(t, h, "allow_repo_commands: false\nno_ci: true\npr:\n  base_branch: staging\n")
	h.CommitChange("staging", "staging-only.txt", "staging\n", "advance prospective target")
	gitIn(t, h, h.WorkDir, "push", "origin", "staging")
	h.Checkout("main")
	global := filepath.Join(h.NMHome, "config.yaml")
	data, err := os.ReadFile(global)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, append(data, []byte("repository_overrides:\n  https://github.com/example/closes.git:\n    prepare: mkdir -p .deps && echo ready > .deps/source\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/prepare-routing"
	head := h.CommitChange(branch, "feature.txt", "feature\n", "add feature")
	gitIn(t, h, h.WorkDir, "push", "origin", branch)
	writeStatefulGH(t, state, statefulGHState{PRs: map[string]*statefulGHPR{branch: {
		Number: 42, URL: "https://github.com/example/closes/pull/42", Title: "Existing PR", Body: "existing", Base: "main", Head: branch, State: "OPEN",
	}}})
	h.PushToGate(branch)
	// An open PR intentionally keeps CI monitoring. Observe publication and
	// let the existing helper stop this synthetic run instead of waiting for
	// a merge that this routing fixture never requests.
	run := waitPublished(t, h, branch, "", "machine-local preparation routing")
	if run.PRURL == nil || *run.PRURL != "https://github.com/example/closes/pull/42" {
		t.Fatalf("existing PR changed: status=%s PR=%v error=%s", run.Status, run.PRURL, deref(run.Error))
	}
	prs := readStatefulGH(t, state).PRs
	pr := prs[branch]
	if len(prs) != 1 || pr == nil || pr.Number != 42 || pr.Base != "main" || run.HeadSHA != head {
		t.Fatalf("PR/base routing changed: PR=%+v head=%s, want %s", pr, run.HeadSHA, head)
	}
	// A first gate push stores the zero old-ref SHA in RunInfo.BaseSHA;
	// the emitted review request carries the actual comparison base.
	if !strings.Contains(reviewPrompt(t, h), "- base commit: "+h.UpstreamBranchSHA("main")+"\n") {
		t.Fatal("review did not receive the existing PR's comparison base")
	}
	for _, call := range readGHStubInvocations(t, os.Getenv("FAKEAGENT_GH_LOG")) {
		if len(call.Args) >= 2 && call.Args[0] == "pr" && call.Args[1] == "create" {
			t.Fatal("created duplicate PR")
		}
	}
	t.Logf("retained PR #42 base main over configured staging at head %s\n%s", head, readStepLog(t, h, run.ID, string(types.StepTest)))
}
