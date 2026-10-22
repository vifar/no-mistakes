//go:build !windows

package steps

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise the dogfood configuration through the actual Lint and Push steps.
// make and gofmt are real; unrelated Go checks are executable boundary stubs,
// so a successful vet cannot conceal the formatting failure reported in #72.
func dogfoodLintFixture(t *testing.T, source string) *pipeline.StepContext {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
	repo, err := config.LoadRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	c, selected := publicationFixture(t)
	c.Config.Commands = repo.Commands
	for _, name := range []string{"Makefile", ".release-please-manifest.json"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(c.WorkDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(c.WorkDir, "formatting.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, c.WorkDir, "add", ".")
	gitCmd(t, c.WorkDir, "commit", "-m", "add dogfood lint fixture")
	c.Run.HeadSHA = gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	if err := c.DB.UpdateRunHeadSHA(c.Run.ID, c.Run.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := guardPRContextWithSelection(c, types.StepRebase, selected, false); err != nil {
		t.Fatal(err)
	}
	recordReviewApproval(t, c, c.Run.HeadSHA)

	bin := t.TempDir()
	// The stub observes the real Makefile's invocations and lets each existing
	// check independently fail. It never substitutes for the formatter.
	stub := `#!/bin/sh
case "$*" in
 'env GOPATH') printf '/unused\n' ;;
 'run ./cmd/genskill --check') echo generated-skill-check; exit "${NM72_SKILL_EXIT:-0}" ;;
 'vet ./...') echo go-vet-check; exit "${NM72_VET_EXIT:-0}" ;;
 *) echo "unexpected go invocation: $*" >&2; exit 99 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NM72_SKILL_EXIT", "0")
	t.Setenv("NM72_VET_EXIT", "0")
	return c
}

func TestDogfoodLintRejectsUnformattedGo(t *testing.T) {
	const source = "package example\n\ntype sample struct {\n A int\n LongerName string\n}\n"
	c := dogfoodLintFixture(t, source)
	outcome, err := (&LintStep{}).Execute(c)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ExitCode == 0 || !outcome.NeedsApproval || !outcome.AutoFixable {
		t.Fatalf("unformatted file passed the lint gate: %+v", outcome)
	}
	if !strings.Contains(outcome.Findings, "formatting.go") {
		t.Fatalf("lint failure did not name the unformatted file: %s", outcome.Findings)
	}
	data, err := os.ReadFile(filepath.Join(c.WorkDir, "formatting.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != source {
		t.Fatal("lint modified the file instead of checking it")
	}
}

func TestDogfoodLintAcceptsFormattedGo(t *testing.T) {
	c := dogfoodLintFixture(t, "package example\n\nvar Answer = 42\n")
	outcome, err := (&LintStep{}).Execute(c)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ExitCode != 0 || outcome.NeedsApproval {
		t.Fatalf("clean lint gate failed: %+v", outcome)
	}
	if status := gitStatusPorcelain(t, c.WorkDir); status != "" {
		t.Fatalf("lint dirtied the tree: %s", status)
	}
}

func TestDogfoodLintRetainsExistingChecks(t *testing.T) {
	for _, check := range []struct{ name, key, output string }{
		{"generated skill", "NM72_SKILL_EXIT", "generated-skill-check"},
		{"go vet", "NM72_VET_EXIT", "go-vet-check"},
	} {
		t.Run(check.name, func(t *testing.T) {
			c := dogfoodLintFixture(t, "package example\n")
			t.Setenv(check.key, "1")
			outcome, err := (&LintStep{}).Execute(c)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.ExitCode == 0 || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, check.output) {
				t.Fatalf("existing check no longer gates lint: %+v", outcome)
			}
		})
	}
}

func TestDogfoodLintCleanPushPreservesReviewedHead(t *testing.T) {
	c := dogfoodLintFixture(t, "package example\n\nvar Answer = 42\n")
	reviewedHead := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
	lint, err := (&LintStep{}).Execute(c)
	if err != nil || lint == nil || lint.ExitCode != 0 || lint.NeedsApproval {
		t.Fatalf("lint: %+v, %v", lint, err)
	}
	outcome, err := (&PushStep{}).Execute(c)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.RestartFrom != "" {
		t.Fatalf("clean formatter requested revalidation: %+v", outcome)
	}
	if got := gitCmd(t, c.WorkDir, "rev-parse", "HEAD"); got != reviewedHead {
		t.Fatalf("Push moved reviewed HEAD: %s -> %s", reviewedHead, got)
	}
	if got := gitCmd(t, c.WorkDir, "log", reviewedHead+"..HEAD", "--format=%s"); got != "" {
		t.Fatalf("Push created commits: %s", got)
	}
	run, err := c.DB.GetRun(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != reviewedHead || run.LastPushedSHA == nil || *run.LastPushedSHA != reviewedHead || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != reviewedHead {
		t.Fatalf("publication differs from reviewed HEAD: %+v", run)
	}
	if status := gitStatusPorcelain(t, c.WorkDir); status != "" {
		t.Fatalf("formatter left dirty files: %s", status)
	}
}
