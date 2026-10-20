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
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCheckUnverifiedFixSummary(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		summary     string
		wantBlocked bool
	}{
		{"Fix Alpaca admission, accounting, recovery; tests blocked without tsx", true},
		{"Blocked by missing tsx; regression tests unverified", true},
		{"verification blocked by missing tsx", true},
		{"tests blocked", true},
		{"regression tests unverified", true},
		{"tests unverified", true},
		{"blocked without tsx", true},
		{"address review findings", false},
		{"fix edge case in parser", false},
		{"add nil check for protectionTakeProfits", false},
	}

	for _, tc := range testCases {
		got := CheckUnverifiedFixSummary(tc.summary)
		if (got != "") != tc.wantBlocked {
			t.Errorf("CheckUnverifiedFixSummary(%q) = %q, wantBlocked=%v", tc.summary, got, tc.wantBlocked)
		}
	}
}

func TestCheckUnverifiedFixCommitMessage(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		subject     string
		wantBlocked bool
	}{
		{"no-mistakes(review): Fix Alpaca admission; tests blocked without tsx", true},
		{"no-mistakes(review): Blocked by missing tsx; regression tests unverified", true},
		{"no-mistakes(review): verification blocked by missing tsx", true},
		{"no-mistakes(test): tests blocked", true},
		{"fix: support unverified emails", false}, // user commit about domain entity
		{"feat: add blocked users list", false},   // user commit about feature
		{"no-mistakes(review): address review findings", false},
	}

	for _, tc := range testCases {
		got := CheckUnverifiedFixCommitMessage(tc.subject)
		if (got != "") != tc.wantBlocked {
			t.Errorf("CheckUnverifiedFixCommitMessage(%q) = %q, wantBlocked=%v", tc.subject, got, tc.wantBlocked)
		}
	}
}

// When a commit in the branch history under review is an unverified review fix commit
// (such as one whose tests were blocked without tsx), ReviewStep must block the run
// with an UnverifiedFixError and MUST NOT review it as a normal head.
func TestReviewStep_BlocksUnverifiedReviewFixCommitAsNormalHead(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)

	// Author an unverified review-fix commit on the branch
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("unverified fix code\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "feature.txt")
	gitCmd(t, dir, "commit", "-m", "no-mistakes(review): Fix Alpaca admission; tests blocked without tsx")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			t.Fatal("review agent must NOT be invoked when head contains an unverified fix commit")
			return nil, nil
		},
	}

	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = false

	step := &ReviewStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected ReviewStep to return error when head contains an unverified fix commit")
	}

	var unverifiedErr *UnverifiedFixError
	if !errors.As(err, &unverifiedErr) {
		t.Fatalf("expected UnverifiedFixError, got %T: %v", err, err)
	}
	if unverifiedErr.Step != types.StepReview {
		t.Errorf("expected step Review, got %s", unverifiedErr.Step)
	}
	if !strings.Contains(unverifiedErr.Error(), "unverified fix commit") {
		t.Errorf("error message %q should mention unverified fix commit", unverifiedErr.Error())
	}
}

// When the fixer agent returns a summary indicating verification was blocked or unverified,
// the fix round must stop with UnverifiedFixError, commit nothing, and leave the worktree clean.
func TestReviewStep_FixerBlockedVerificationCommitsNothingAndBlocks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// Fixer agent touches a file then returns an unverified summary
			if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("broken edit\n"), 0o644); err != nil {
				return nil, err
			}
			output, _ := json.Marshal(map[string]string{
				"summary": "Blocked by missing tsx; regression tests unverified",
			})
			return &agent.Result{Output: output}, nil
		},
	}

	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"f-1","severity":"error","file":"feature.txt","description":"bug"}],"summary":"1 bug"}`

	step := &ReviewStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected error when fixer reports blocked verification")
	}

	var unverifiedErr *UnverifiedFixError
	if !errors.As(err, &unverifiedErr) {
		t.Fatalf("expected UnverifiedFixError, got %T: %v", err, err)
	}

	// Verify nothing was committed: HEAD must remain unchanged
	currentHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if currentHead != headSHA {
		t.Fatalf("HEAD advanced to %s, want %s (commit must not be created)", currentHead, headSHA)
	}

	// Verify worktree is clean (dirty unverified edits were discarded)
	status := gitStatusPorcelain(t, dir)
	if status != "" {
		t.Fatalf("worktree should be clean after blocked verification, got %q", status)
	}
}

func TestReviewStep_FixPromptIncludesRepositoryCommandsGuidance(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			output, _ := json.Marshal(map[string]string{
				"summary": "clean fix",
			})
			return &agent.Result{Output: output}, nil
		},
	}

	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"f-1","severity":"warning","file":"feature.txt","description":"bug"}],"summary":"1 bug"}`

	step := &ReviewStep{}
	// Will fail on commit because nothing was modified in worktree, but fixer was invoked
	_, _ = step.Execute(sctx)

	if len(ag.calls) == 0 {
		t.Fatal("expected fixer to be called")
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		"run the repository's own commands",
		"a PATH probe is not a dependency check",
		"unverified fixes are rejected and block the run",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("fixer prompt missing %q:\n%s", want, prompt)
		}
	}
}
