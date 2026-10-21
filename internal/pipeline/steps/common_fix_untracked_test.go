package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #68: display preferences cannot omit intended proposals from a
// correction commit. Read both the commit and a local publication as the oracle,
// rather than trusting the helper's boolean or another config-sensitive status.
func TestCommitAgentFixes_UntrackedVisibility(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		for _, tracked := range []bool{false, true} {
			name := "default"
			if hidden {
				name = "hidden"
			}
			if tracked {
				name += "/tracked-and-untracked"
			}
			t.Run(name, func(t *testing.T) {
				dir, base, head := setupGitRepo(t)
				if hidden {
					inheritHiddenUntracked(t, dir)
				}
				sc := newTestContextWithDBRecords(t, &mockAgent{}, dir, base, head, config.Commands{})
				writeTestFile(t, dir, "docs/proposal.md", "# Intended documentation\n")
				if tracked {
					writeTestFile(t, dir, "feature.txt", "tracked correction\n")
				}
				committed, err := commitAgentFixesWithResult(sc, types.StepDocument, "document proposal", "", nil)
				if err != nil {
					t.Fatal(err)
				}
				if !committed {
					t.Error("intended untracked proposal was reported as no changes")
				}
				assertUntrackedProposalCommitted(t, sc, head)
				if tracked && gitCmd(t, dir, "show", "HEAD:feature.txt") != "tracked correction" {
					t.Error("tracked correction missing from commit")
				}
				published := t.TempDir()
				gitCmd(t, published, "init", "--bare")
				gitCmd(t, dir, "push", published, "HEAD:refs/heads/proposal")
				if got, err := git.RunRaw(sc.Ctx, published, "show", "refs/heads/proposal:docs/proposal.md"); err != nil || string(got) != "# Intended documentation\n" {
					t.Errorf("published content = %q, err=%v", got, err)
				}
				committed, err = commitAgentFixesWithResult(sc, types.StepDocument, "document proposal", "", nil)
				if err != nil || committed {
					t.Errorf("clean worktree must remain a no-op: committed=%v err=%v", committed, err)
				}
			})
		}
	}
}

func TestCommitAgentFixes_UntrackedVisibilityGuards(t *testing.T) {
	for _, guard := range []string{"protected", "oversized", "scratch-only", "proposal-with-scratch"} {
		t.Run(guard, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			inheritHiddenUntracked(t, dir)
			sc := newTestContextWithDBRecords(t, &mockAgent{}, dir, base, head, config.Commands{})
			var wantRule string
			switch guard {
			case "protected":
				sc.Config.ProtectedPaths = []string{"docs/proposal.md"}
				wantRule = "docs/proposal.md"
			case "oversized":
				sc.Config.MaxFileSizeBytes = 8
				wantRule = "max_file_size_bytes"
			}
			if guard != "scratch-only" {
				writeTestFile(t, dir, "docs/proposal.md", "# Intended documentation\n")
			}
			if strings.Contains(guard, "scratch") {
				writeTestFile(t, dir, ".cache/tool/output.txt", "scratch must stay local\n")
				writeTestFile(t, dir, "temporary.txt", "excluded scratch\n")
				sc.Config.StagingExclude = []string{"temporary.txt"}
			}
			index := gitCmd(t, dir, "write-tree")
			committed, err := commitAgentFixesWithResult(sc, types.StepDocument, "document proposal", "", nil)
			if wantRule != "" {
				var refusal *pipeline.ProtectedPathError
				if !errors.As(err, &refusal) || refusal.Rule != wantRule || refusal.Path != "docs/proposal.md" {
					t.Errorf("expected explicit %s refusal, got %v", wantRule, err)
				}
				if committed || gitCmd(t, dir, "rev-parse", "HEAD") != head || gitCmd(t, dir, "write-tree") != index {
					t.Error("refusal changed HEAD or index, or reported a commit")
				}
				if got, err := os.ReadFile(filepath.Join(dir, "docs/proposal.md")); err != nil || string(got) != "# Intended documentation\n" {
					t.Errorf("refusal lost proposal: %q %v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if guard == "scratch-only" {
				if committed || gitCmd(t, dir, "rev-parse", "HEAD") != head {
					t.Error("scratch-only changes advanced HEAD")
				}
			} else {
				if !committed || gitCmd(t, dir, "show", "--format=", "--name-only", "HEAD") != "docs/proposal.md" {
					t.Error("commit must contain only the intended documentation")
				}
			}
			for _, path := range []string{".cache/tool/output.txt", "temporary.txt"} {
				if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
					t.Errorf("scratch not preserved: %v", err)
				}
				if got := gitCmd(t, dir, "ls-files", "--", path); got != "" {
					t.Errorf("scratch was staged: %s", got)
				}
			}
		})
	}
}

func TestAgentFixCallers_UntrackedVisibility(t *testing.T) {
	for _, caller := range []string{"document", "lint-agent", "lint-fix", "review-fix"} {
		t.Run(caller, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			inheritHiddenUntracked(t, dir)
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				calls++
				if calls > 1 {
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"ok","risk_scope":"source-or-external","reviewed_paths":["feature.txt","docs/proposal.md"]}`)}, nil
				}
				writeTestFile(t, dir, "docs/proposal.md", "# Intended documentation\n")
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"document proposal","verification":"passed"}`)}, nil
			}}
			commands := config.Commands{}
			if caller == "document" || caller == "lint-fix" {
				commands.Lint = "true"
			}
			sc := newTestContextWithDBRecords(t, ag, dir, base, head, commands)
			sc.Config.Document.MaxCommitsPerRun = 1
			var step pipeline.Step
			switch caller {
			case "document":
				step = &DocumentStep{}
			case "lint-agent", "lint-fix":
				step = &LintStep{}
				sc.Fixing = caller == "lint-fix"
			case "review-fix":
				step = &ReviewStep{}
				sc.Fixing = true
				sc.PreviousFindings = unverifiedFixFindings
			}
			out, err := step.Execute(sc)
			if err != nil {
				t.Fatal(err)
			}
			if out.NeedsApproval || out.FixSummary == NoChangesAppliedSummary || out.FixSummary == "" {
				t.Errorf("caller must report applied changes without approval: %+v", out)
			}
			assertUntrackedProposalCommitted(t, sc, head)
			if caller == "document" {
				passes, err := sc.DB.GetDocumentPasses(sc.Run.ID)
				if err != nil || len(passes) != 1 || passes[0].CommitCount != 1 {
					t.Errorf("within-budget proposal must consume one Document commit: %+v %v", passes, err)
				}
			}
		})
	}
}

// Use a global config file to reproduce an inherited display preference; no
// repository-local status setting is needed to trigger the original bug.
func inheritHiddenUntracked(t *testing.T, dir string) {
	t.Helper()
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[status]\n\tshowUntrackedFiles = no\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	if got := gitCmd(t, dir, "config", "--get", "status.showUntrackedFiles"); got != "no" {
		t.Fatalf("inherited setting = %q", got)
	}
}

func assertUntrackedProposalCommitted(t *testing.T, sc *pipeline.StepContext, before string) {
	t.Helper()
	head := gitCmd(t, sc.WorkDir, "rev-parse", "HEAD")
	if head == before || sc.Run.HeadSHA != head {
		t.Errorf("helper did not record a new head: before=%s HEAD=%s run=%s", before, head, sc.Run.HeadSHA)
	}
	if got, err := git.RunRaw(sc.Ctx, sc.WorkDir, "show", "HEAD:docs/proposal.md"); err != nil || string(got) != "# Intended documentation\n" {
		t.Errorf("committed content = %q, err=%v", got, err)
	}
	if got := gitCmd(t, sc.WorkDir, "status", "--porcelain", "--untracked-files=all"); got != "" {
		t.Errorf("worktree is not clean: %s", got)
	}
}
