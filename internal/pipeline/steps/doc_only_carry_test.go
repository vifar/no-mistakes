package steps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestIsDocOnlyPath(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]bool{
		"README.md":                   true,
		"CHANGELOG.md":                true,
		"docs/guide.md":               true,
		"docs/src/content/docs/a.mdx": true,
		"docs/reference/api.rst":      true,
		"AGENTS.md":                   false,
		"CLAUDE.md":                   false,
		"docs/AGENTS.md":              false,
		"docs/skills/ship/SKILL.md":   false,
		"docs/prompts/review.md":      false,
		".github/README.md":           false,
		".codex/skills/x/SKILL.md":    false,
		"docs/.hidden/notes.md":       false,
		"internal/agent/prompt.md":    false,
		"requirements.txt":            false,
		"docs/build.go":               false,
		"main.go":                     false,
		".no-mistakes.yaml":           false,
		"docs/config.yaml":            false,
	} {
		if got := isDocOnlyPath(path); got != want {
			t.Errorf("isDocOnlyPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// A Document commit between the approved head and the head being pushed used
// to restart Review (push.go), and each revalidation's Document pass committed
// again, so runs churned through generation after generation. A doc-only gap
// now carries Review and Test credit onto the pushed head instead.
func TestReviewRestartNeededForPush_DocOnlyGapCarriesValidation(t *testing.T) {
	cases := []struct {
		name        string
		change      func(t *testing.T, dir string)
		strict      bool
		testedOther bool
		pushCommit  bool
		wantRestart bool
	}{
		{name: "doc-only commit carries", change: writeAndCommit("docs/guide.md", "guide\n"), wantRestart: false},
		{name: "root readme carries", change: writeAndCommit("README.md", "readme v2\n"), wantRestart: false},
		{name: "code commit restarts", change: writeAndCommit("src/widget.go", "package widget\n"), wantRestart: true},
		{name: "agent instruction file restarts", change: writeAndCommit("AGENTS.md", "rules\n"), wantRestart: true},
		{name: "code deletion restarts", change: deleteAndCommit("tracked.go"), wantRestart: true},
		{name: "strict shipping restarts", change: writeAndCommit("docs/guide.md", "guide\n"), strict: true, wantRestart: true},
		{name: "tested head elsewhere restarts", change: writeAndCommit("docs/guide.md", "guide\n"), testedOther: true, wantRestart: true},
		{name: "push-made commit restarts", change: writeAndCommit("docs/guide.md", "guide\n"), pushCommit: true, wantRestart: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, baseSHA, _ := setupGitRepo(t)
			if err := os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("package tracked\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "-q", "-m", "feat: tracked")
			approved := gitCmd(t, dir, "rev-parse", "HEAD")
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, approved, config.Commands{})
			recordReviewApproval(t, sctx, approved)
			tested := approved
			if tc.testedOther {
				tested = baseSHA
			}
			recordTestedHead(t, sctx, tested)

			tc.change(t, dir)
			pushed := gitCmd(t, dir, "rev-parse", "HEAD")
			if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, pushed); err != nil {
				t.Fatal(err)
			}
			sctx.Run.HeadSHA = pushed
			if tc.strict {
				sctx.Run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull}
			}
			newHead := ""
			if tc.pushCommit {
				newHead = pushed
			}

			restart, err := reviewRestartNeededForPush(sctx, newHead, pushed)
			if err != nil {
				t.Fatal(err)
			}
			if restart != tc.wantRestart {
				t.Fatalf("restart = %v, want %v", restart, tc.wantRestart)
			}
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantApproved := approved
			if !tc.wantRestart {
				wantApproved = pushed
			}
			if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != wantApproved {
				t.Fatalf("review approved head = %v, want %s", run.ReviewApprovedHeadSHA, wantApproved)
			}
			if got := testedHead(t, sctx); !tc.wantRestart && got != pushed {
				t.Fatalf("tested head = %s, want carried %s", got, pushed)
			} else if tc.wantRestart && got != tested {
				t.Fatalf("tested head changed to %s on a restart", got)
			}
		})
	}
}

// Without a doc-only carry, an unchanged approval still needs no restart.
func TestReviewRestartNeededForPush_ApprovedHeadNeedsNothing(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	recordReviewApproval(t, sctx, headSHA)
	restart, err := reviewRestartNeededForPush(sctx, "", headSHA)
	if err != nil || restart {
		t.Fatalf("restart = %v, err = %v; want no restart", restart, err)
	}
}

func writeAndCommit(path, content string) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		t.Helper()
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, dir, "add", "-A")
		gitCmd(t, dir, "commit", "-q", "-m", "change "+path)
	}
}

func deleteAndCommit(path string) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		t.Helper()
		gitCmd(t, dir, "rm", "-q", path)
		gitCmd(t, dir, "commit", "-q", "-m", "remove "+path)
	}
}

func recordTestedHead(t *testing.T, sctx *pipeline.StepContext, head string) {
	t.Helper()
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := types.MarshalFindingsJSON(types.Findings{Summary: "tests passed", TestedHeadSHA: head, Verdict: "passed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(step.ID, raw); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
}

func testedHead(t *testing.T, sctx *pipeline.StepContext) string {
	t.Helper()
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepTest && step.FindingsJSON != nil {
			findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
			if err != nil {
				t.Fatal(err)
			}
			return findings.TestedHeadSHA
		}
	}
	t.Fatal("no test step findings")
	return ""
}
