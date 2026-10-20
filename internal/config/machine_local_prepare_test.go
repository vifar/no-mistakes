package config

import (
	"strings"
	"testing"
)

// TestMachineLocalPrepareFillsOnlyAnEmptyRepositoryCommand proves the
// operator's machine-local preparation command fills a gap the repository
// left and never replaces what the repository configured.
//
// The gap case is the one that matters: a repository with NO .no-mistakes.yaml
// could not get commands.prepare from any source, so the operator
// provisioned dependencies by hand (vifar/no-mistakes#77, six measured
// instances).
func TestMachineLocalPrepareFillsOnlyAnEmptyRepositoryCommand(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(`repository_overrides:
  https://github.com/acme/widget.git:
    prepare: "pnpm install --frozen-lockfile"
`))
	if err != nil {
		t.Fatal(err)
	}

	// The gap: the repository configured nothing.
	gap := MergeForRemote(global, &RepoConfig{}, "git@github.com:acme/widget.git")
	if gap.Commands.Prepare != "pnpm install --frozen-lockfile" {
		t.Fatalf("the machine-local prepare did not fill the gap: %q", gap.Commands.Prepare)
	}
	if gap.MachineLocalPrepare != "pnpm install --frozen-lockfile" {
		t.Fatalf("the machine-local prepare was not recorded: %q", gap.MachineLocalPrepare)
	}
	// The eager trigger follows the machine-local request, so an agent-only
	// Test actually prepares.
	if !gap.Test.Prepare {
		t.Fatal("the machine-local prepare did not trigger eager preparation")
	}

	// The repository's own value wins.
	owned := MergeForRemote(global, &RepoConfig{Commands: Commands{Prepare: "repo-prepare"}}, "git@github.com:acme/widget.git")
	if owned.Commands.Prepare != "repo-prepare" {
		t.Fatalf("the machine-local prepare replaced the repository's: %q", owned.Commands.Prepare)
	}
	if owned.MachineLocalPrepare != "" {
		t.Fatalf("a replaced command was still declared machine-local: %q", owned.MachineLocalPrepare)
	}
	if owned.Test.Prepare {
		t.Fatal("a repository without test.prepare inherited the eager trigger")
	}

	// A repository that supplied no command and no machine-local override is
	// unchanged.
	plain := MergeForRemote(global, &RepoConfig{}, "git@github.com:acme/other.git")
	if plain.Commands.Prepare != "" {
		t.Fatalf("an unmatched override changed the merge: %q", plain.Commands.Prepare)
	}
	if plain.Test.Prepare {
		t.Fatal("an unmatched override set the eager trigger")
	}
}

// TestMachineLocalPrepareIsRefusedWhenMalformed proves the machine-local
// preparation command is validated like any other configured command.
func TestMachineLocalPrepareIsRefusedWhenMalformed(t *testing.T) {
	for _, value := range []string{
		"prepare: '   '",
		"prepare: \"echo\\0bad\"",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte("repository_overrides:\n  https://github.com/acme/widget:\n    " + value + "\n"))
			if err == nil {
				t.Fatal("accepted a malformed machine-local prepare command")
			}
			if !strings.Contains(err.Error(), "prepare") {
				t.Fatalf("the refusal does not name the setting: %v", err)
			}
		})
	}
}

// TestMachineLocalCommandsSurfaceStillRefusesACommandString proves the
// existing safety boundary is intact: the general machine-local commands
// surface still refuses a command string, so a machine's configuration can
// never replace what a repository's maintainer asked to run.
func TestMachineLocalCommandsSurfaceStillRefusesACommandString(t *testing.T) {
	for _, value := range []string{
		"test: {command: 'exit 0'}",
		"test: {replace: 'exit 0'}",
		"test: {skip: true}",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte("repository_overrides:\n  https://github.com/acme/widget:\n    commands:\n      " + value + "\n"))
			if err == nil {
				t.Fatal("accepted a replacement, skip, or invalid execution context")
			}
		})
	}
}
