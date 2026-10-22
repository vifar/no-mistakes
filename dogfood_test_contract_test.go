package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// Local Test is targeted validation of the requested intent. This repository
// must not dogfood a complete-suite commands.test override (historically
// go test -race ./...), which burned multi-hour local walks while remote CI
// already owns the race-enabled full suite.
func TestDogfoodConfig_NoBroadLocalTestCommand(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadRepo(root)
	if err != nil {
		t.Fatalf("LoadRepo: %v", err)
	}
	if got := strings.TrimSpace(cfg.Commands.Test); got != "" {
		t.Fatalf("dogfood commands.test = %q, want empty so local Test stays agent-targeted; put broad regression in remote CI", got)
	}

	raw, err := os.ReadFile(filepath.Join(root, ".no-mistakes.yaml"))
	if err != nil {
		t.Fatalf("read .no-mistakes.yaml: %v", err)
	}
	content := string(raw)
	for _, forbid := range []string{
		`test: "go test -race ./..."`,
		`test: 'go test -race ./...'`,
		`test: go test -race ./...`,
	} {
		if strings.Contains(content, forbid) {
			t.Fatalf(".no-mistakes.yaml still configures broad local Test %q", forbid)
		}
	}
}

// commands.test contract ownership lives in repo-config.md: targeted local
// validation, not CI-parity complete-suite configuration, and no brittle
// shell-heuristics claiming to detect "broad" commands.
func TestRepoConfigDocs_CommandsTestIsTargetedContract(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("docs", "src", "content", "docs", "reference", "repo-config.md"))
	if err != nil {
		t.Fatalf("read repo-config.md: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"### commands.test",
		"targeted validation",
		"not a CI-parity repository-wide regression command",
		"Broad regression belongs in remote CI",
		"does not guess whether an arbitrary shell string is \"too broad\"",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("repo-config.md commands.test contract missing %q", want)
		}
	}
}

func TestCIWorkflow_RetainsFullRaceSuiteAsBroadRegressionOwner(t *testing.T) {
	job := loadCIWorkflowDoc(t).Jobs["test"]
	if job == nil {
		t.Fatal("missing test job")
	}
	var suite []workflowCommand
	for _, command := range workflowCommandsMatching(job.Steps, func(step wfStep) bool { return step.If == "" }) {
		if command.name == "bash" && slices.Equal(command.args, []string{"scripts/race.sh"}) {
			suite = append(suite, command)
		}
	}
	if len(suite) != 1 {
		t.Fatalf("unconditional race wrapper invocations = %d, want 1", len(suite))
	}
	dir := t.TempDir()
	inventory := []string{
		"github.com/kunchenguid/no-mistakes/internal/daemon",
		"github.com/kunchenguid/no-mistakes/internal/pipeline/steps",
		"github.com/kunchenguid/no-mistakes/internal/pipeline",
		"example.org/suite/alpha",
		"example.org/suite/beta",
		"example.org/suite/gamma",
		"example.org/suite/" + filepath.Base(dir),
	}
	if err := os.WriteFile(filepath.Join(dir, "inventory"), []byte(strings.Join(inventory, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
case "$1" in
 list)
  [ "$#" -eq 2 ] && [ "$2" = './...' ] || exit 81
  cat "$RACE_CONTRACT_DIR/inventory"
  ;;
 test)
  shift
  printf '%s\n' "$@" > "$RACE_CONTRACT_DIR/args-$$"
  ;;
 *) exit 82 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	command := suite[0]
	cmd := exec.Command(command.name, command.args...)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "RACE_CONTRACT_DIR="+dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("race wrapper: %v\n%s", err, output)
	}
	logs, err := filepath.Glob(filepath.Join(dir, "args-*"))
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for _, log := range logs {
		raw, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Fields(string(raw))
		race := false
		for i := 0; i < len(args); i++ {
			arg := args[i]
			switch {
			case arg == "-race":
				race = true
			case arg == "-p" || arg == "-timeout" || arg == "-count":
				i++
			case strings.HasPrefix(arg, "-"):
			default:
				counts[arg]++
			}
		}
		if !race {
			t.Fatalf("test invocation lacks race detector: %v", args)
		}
	}
	for _, pkg := range inventory {
		if counts[pkg] != 1 {
			t.Errorf("%s executed %d times, want exactly once", pkg, counts[pkg])
		}
		delete(counts, pkg)
	}
	if len(counts) != 0 {
		t.Fatalf("unexpected packages executed: %v", counts)
	}
}
