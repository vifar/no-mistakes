package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestEnsurePrepared_DeclaresAMachineLocalPrepareCommand proves a
// dependency-preparation command the OPERATOR supplied is declared in the step
// log, so a reader knows the prepared worktree came from the machine's policy
// and not from the repository's trusted configuration.
//
// This is the case a repository with no .no-mistakes.yaml needed: it had no
// commands.prepare from any source, and the operator provisioned dependencies
// by hand (vifar/no-mistakes#77).
func TestEnsurePrepared_DeclaresAMachineLocalPrepareCommand(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ignoreTestDependencies(t, dir)
	sctx := newPreparationTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{Prepare: preparationCommand()})
	sctx.Shared = &pipeline.RunShared{}
	// The command came from the machine, not from the repository.
	sctx.Config.MachineLocalPrepare = preparationCommand()

	var logged []string
	original := sctx.Log
	sctx.Log = func(message string) {
		logged = append(logged, message)
		original(message)
	}

	if err := ensurePrepared(sctx, types.StepTest); err != nil {
		t.Fatalf("preparation: %v", err)
	}
	declared := false
	for _, line := range logged {
		if strings.Contains(line, "machine-local prepare command applied") {
			declared = true
		}
	}
	if !declared {
		t.Fatalf("the machine-local prepare command was not declared: %v", logged)
	}
	// The preparation really ran.
	count, err := os.ReadFile(filepath.Join(dir, ".deps", "count"))
	if err != nil {
		t.Fatalf("read materialized dependency marker: %v", err)
	}
	if !strings.Contains(string(count), "prepared") {
		t.Fatalf("the machine-local prepare did not run: %q", count)
	}
}

// TestEnsurePrepared_AnEmptyPrepareCommandDeclaresNothing proves the
// declaration never names a command that did not run.
func TestEnsurePrepared_AnEmptyPrepareCommandDeclaresNothing(t *testing.T) {
	if declaration := machineLocalPrepareDeclaration(""); declaration != "" {
		t.Fatalf("an empty command was declared: %q", declaration)
	}
	if declaration := machineLocalPrepareDeclaration("   "); declaration != "" {
		t.Fatalf("a blank command was declared: %q", declaration)
	}
	if declaration := machineLocalPrepareDeclaration("pnpm install"); !strings.Contains(declaration, "pnpm install") {
		t.Fatalf("the declaration does not name the command: %q", declaration)
	}
}
