package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func replayAgentGit(t *testing.T, ctx context.Context, opts agent.RunOpts, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, testGitExecutable, args...)
	cmd.Dir = opts.CWD
	cmd.Env = append(append(os.Environ(), opts.Env...), "GIT_EDITOR=true")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent Git %v: %v\n%s", args, err, out)
	}
}

func traceReplayProtocol(t *testing.T, dir string) {
	t.Helper()
	hooks := gitCmd(t, dir, "config", "core.hooksPath")
	log := filepath.Join(hooks, "pick-protocol")
	script := "#!/bin/sh\nprintf 'source=%s original=%s action=%s\\n' \"$2\" \"$3\" \"$GIT_REFLOG_ACTION\" >> '" + log + "'\nstate=$(git rev-parse --git-path rebase-merge/done)\nif test -f \"$state\"; then tail -n 1 \"$state\" >> '" + log + "'; fi\n"
	if err := os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if data, err := os.ReadFile(log); err == nil {
			t.Logf("observed Git replay protocol:\n%s", data)
		}
	})
}

func TestReplayProvenanceAutomaticPicks(t *testing.T) {
	for _, entry := range []string{"automatic", "with-agent"} {
		t.Run(entry, func(t *testing.T) {
			f := newMergeFixture(t, false)
			const message = "submitted feature\n\nKeep this body.\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nCodex-Session: submitted-clean"
			gitCmd(t, f.dir, "commit", "--amend", "-m", message)
			f.headSHA = gitCmd(t, f.dir, "rev-parse", "HEAD")
			attributionAppendingHook(t, f.dir)
			traceReplayProtocol(t, f.dir)
			c := f.context(t, &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				t.Fatal("clean rebase launched an agent")
				return nil, nil
			}}, "rebase")
			if entry == "automatic" {
				if conflicts, err := tryRebase(c.Ctx, c, "origin/main"); err != nil || len(conflicts) != 0 {
					t.Fatalf("automatic rebase = %v, %v", conflicts, err)
				}
			} else {
				if err := rebaseWithAgent(c.Ctx, c, "origin/main"); err != nil {
					t.Fatal(err)
				}
			}
			if got := storedReplayMessage(t, f.dir, "HEAD"); !bytes.Equal(got, []byte(message+"\n")) {
				t.Errorf("raw submitted replay changed: %q", got)
			}
			if got := gitCmd(t, f.dir, "log", "-1", "--format=%B"); got != message {
				t.Errorf("automatic submitted replay = %q, want %q", got, message)
			}
			if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
				t.Error(err)
			}
			if got := gitCmd(t, f.dir, "show", "-s", "--format=%B", f.headSHA); got != message {
				t.Fatal("submitted object changed")
			}
		})
	}
}

func TestReplayProvenanceCleanPickAfterConflict(t *testing.T) {
	f := newMergeFixture(t, true)
	const first = "submitted conflict\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nCodex-Session: first"
	const second = "submitted clean followup\n\nCo-Authored-By: gpt-5.5 <model@example.com>\nCodex-Session: second"
	gitCmd(t, f.dir, "commit", "--amend", "-m", first)
	writeFixtureFile(t, f.dir, "followup.txt", "submitted followup\n")
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", second)
	f.headSHA = gitCmd(t, f.dir, "rev-parse", "HEAD")
	attributionAppendingHook(t, f.dir)
	traceReplayProtocol(t, f.dir)
	ag := &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		writeFixtureFile(t, f.dir, "shared.txt", "main line\nfeature line\n")
		replayAgentGit(t, ctx, opts, "add", "-A")
		replayAgentGit(t, ctx, opts, "rebase", "--continue")
		if got := storedReplayMessage(t, f.dir, "HEAD"); !bytes.Equal(got, []byte(second+"\n")) {
			t.Errorf("raw clean pick after conflict changed: %q", got)
		}
		if got := gitCmd(t, f.dir, "log", "-1", "--format=%B"); got != second {
			t.Errorf("clean pick after conflict = %q, want %q", got, second)
		}
		return &agent.Result{}, nil
	}}
	c := f.context(t, ag, "rebase")
	if err := rebaseWithAgent(c.Ctx, c, "origin/main"); err != nil {
		t.Error(err)
	}
}

func TestReplayProvenanceSuccessiveRebases(t *testing.T) {
	f := newMergeFixture(t, true)
	const message = "submitted conflict\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nCodex-Session: submitted-original"
	gitCmd(t, f.dir, "commit", "--amend", "-m", message)
	f.headSHA = gitCmd(t, f.dir, "rev-parse", "HEAD")
	attributionAppendingHook(t, f.dir)
	traceReplayProtocol(t, f.dir)
	ag := &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		writeFixtureFile(t, f.dir, "shared.txt", "main line\nfeature line\n")
		replayAgentGit(t, ctx, opts, "add", "-A")
		replayAgentGit(t, ctx, opts, "rebase", "--continue")
		return &agent.Result{}, nil
	}}
	c := f.context(t, ag, "rebase")
	if err := rebaseWithAgent(c.Ctx, c, "origin/main"); err != nil {
		t.Fatal(err)
	}
	if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
		t.Fatal(err)
	}
	firstReplay := gitCmd(t, f.dir, "rev-parse", "HEAD")
	if got := gitCmd(t, f.dir, "log", "-1", "--format=%B"); got != message {
		t.Fatalf("first replay = %q", got)
	}
	gitCmd(t, f.dir, "checkout", "main")
	writeFixtureFile(t, f.dir, "second-base.txt", "new imported base\n")
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "second upstream advance")
	gitCmd(t, f.dir, "push", "origin", "main")
	gitCmd(t, f.dir, "checkout", "feature")
	if conflicts, err := tryRebase(c.Ctx, c, "origin/main"); err != nil || len(conflicts) != 0 {
		t.Fatalf("second rebase = %v, %v", conflicts, err)
	}
	if got := gitCmd(t, f.dir, "log", "-1", "--format=%B"); got != message {
		t.Errorf("second replay = %q, want %q", got, message)
	}
	if got := storedReplayMessage(t, f.dir, "HEAD"); !bytes.Equal(got, []byte(message+"\n")) {
		t.Errorf("raw successive replay changed: %q", got)
	}
	if next := gitCmd(t, f.dir, "rev-parse", "HEAD"); next == firstReplay {
		t.Fatal("second rebase did not replay history")
	}
	if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
		t.Error(err)
	}
}

func TestReplayProvenanceCopiedCorrectionDuringStop(t *testing.T) {
	f := newMergeFixture(t, true)
	const message = "submitted conflict\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nCodex-Session: submitted"
	gitCmd(t, f.dir, "commit", "--amend", "-m", message)
	f.headSHA = gitCmd(t, f.dir, "rev-parse", "HEAD")
	attributionAppendingHook(t, f.dir)
	ag := &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		writeFixtureFile(t, f.dir, "shared.txt", "main line\nfeature line\n")
		replayAgentGit(t, ctx, opts, "add", "-A")
		replayAgentGit(t, ctx, opts, "commit", "--allow-empty", "--only", "-C", f.headSHA)
		if got := gitTrailers(t, f.dir); got != "Signed-off-by: Keeper <keeper@example.com>" {
			t.Errorf("new source-copy correction retained attribution: %q", got)
		}
		replayAgentGit(t, ctx, opts, "rebase", "--continue")
		if got := gitCmd(t, f.dir, "log", "-1", "--format=%B"); got != message {
			t.Errorf("continued submitted message = %q", got)
		}
		return &agent.Result{}, nil
	}}
	c := f.context(t, ag, "rebase")
	if err := rebaseWithAgent(c.Ctx, c, "origin/main"); err != nil {
		t.Error(err)
	}
}

func TestImportedProvenanceFastForward(t *testing.T) {
	for _, entry := range []string{"automatic-rebase", "agent-rebase", "automatic-merge", "agent-merge"} {
		t.Run(entry, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			c := newTestContextWithDBRecords(t, &mockAgent{}, dir, base, head, config.Commands{})
			if entry == "automatic-merge" {
				if _, err := c.CommitAttributionEnv(nil, func(args ...string) (string, error) {
					cmd := exec.CommandContext(c.Ctx, testGitExecutable, args...)
					cmd.Dir = dir
					out, err := cmd.Output()
					return strings.TrimSpace(string(out)), err
				}); err != nil {
					t.Fatal(err)
				}
			}
			gitCmd(t, dir, "checkout", "-b", "imported-target")
			writeFixtureFile(t, dir, "upstream.txt", "existing upstream\n")
			gitCmd(t, dir, "add", "-A")
			const message = "existing upstream\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nCodex-Session: imported"
			gitCmd(t, dir, "commit", "-m", message)
			target := gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "update-ref", "refs/remotes/origin/main", target)
			gitCmd(t, dir, "checkout", "feature")
			switch entry {
			case "automatic-rebase":
				if _, err := tryRebase(c.Ctx, c, "origin/main"); err != nil {
					t.Fatal(err)
				}
			case "agent-rebase":
				if err := rebaseWithAgent(c.Ctx, c, "origin/main"); err != nil {
					t.Fatal(err)
				}
			case "automatic-merge":
				if _, err := tryMerge(c.Ctx, c, "origin/main"); err != nil {
					t.Fatal(err)
				}
			case "agent-merge":
				if err := mergeWithAgent(c.Ctx, c, "origin/main"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := updateHeadSHA(c.Ctx, c, "main"); err != nil {
				t.Fatalf("rejecting existing imported history: %v", err)
			}
			if c.Run.HeadSHA != target || gitCmd(t, dir, "log", "-1", "--format=%B") != message {
				t.Fatal("imported identity or message changed")
			}
			c.Agent = &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				replayAgentGit(t, ctx, opts, "commit", "--allow-empty", "-m", "new correction\n\nCodex-Session: fresh")
				return &agent.Result{}, nil
			}}
			if _, err := c.RunAgent(agent.RunOpts{CWD: dir}); err != nil {
				t.Fatal(err)
			}
			if got := gitTrailers(t, dir); got != "" {
				t.Fatalf("new correction = %q", got)
			}
			if err := commitAgentFixes(c, types.StepReview, "", "fallback"); err != nil {
				t.Fatal(err)
			}
			clean := c.Run.HeadSHA
			gitCmd(t, dir, "commit", "--allow-empty", "-m", "bypassed correction\n\nCodex-Session: bypass")
			if _, err := updateHeadSHA(c.Ctx, c, "main"); err == nil {
				t.Fatal("accepted a newly attributed bypass")
			}
			if c.Run.HeadSHA != clean {
				t.Fatal("adopted bypassed head")
			}
		})
	}
}

func TestRepairOutcomeExecutorWithoutEvalProvenance(t *testing.T) {
	testRepairOutcomeExecutor(t, false, "guard unsafe value", true)
}
func TestRepairOutcomeRecordedMatrix(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, description := range []string{"guard unsafe value", "no changes applied"} {
			for _, change := range []bool{false, true} {
				t.Run(fmt.Sprintf("capture=%v/summary=%s/change=%v", capture, description, change), func(t *testing.T) { testRepairOutcomeExecutor(t, capture, description, change) })
			}
		}
	}
}
func testRepairOutcomeExecutor(t *testing.T, capture bool, description string, change bool) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	c := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
	c.Repo.UpstreamURL = dir
	c.Config.CaptureEvalProvenance = capture
	c.Config.AutoFix = config.AutoFix{Review: 1}
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	reviews := 0
	c.Agent = &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if strings.Contains(opts.Prompt, "Investigate previous review findings") {
			if change {
				writeFixtureFile(t, dir, "feature.txt", "guarded feature\n")
				replayAgentGit(t, ctx, opts, "add", "-A")
				replayAgentGit(t, ctx, opts, "commit", "-m", "guard unsafe value\n\nCodex-Session: agent")
			}
			output, _ := json.Marshal(map[string]string{"summary": description, "verification": "passed"})
			return &agent.Result{Output: output}, nil
		}
		reviews++
		findings := cleanReviewFindings()
		if reviews == 1 {
			findings.Items = []types.Finding{{ID: "review-1", Severity: types.FindingSeverityError, Action: types.ActionAutoFix, File: "feature.txt", Line: 1, Description: "unsafe value", Support: &types.FindingSupport{ClaimType: types.FindingClaimSource, Source: &types.FindingSourceSupport{Path: "feature.txt", Line: 1, Quote: "feature code"}}}}
		}
		data, _ := json.Marshal(findings)
		return &agent.Result{Output: data}, nil
	}}
	executor := pipeline.NewExecutor(c.DB, p, c.Config, c.Agent, []pipeline.Step{&ReviewStep{}}, nil)
	executor.SetPRContextGuard(GuardPRContext)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := executor.Execute(ctx, c.Run, c.Repo, dir); err != nil {
		t.Fatal(err)
	}
	steps, err := c.DB.GetStepsByRun(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds, err := c.DB.GetRoundsByRun(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	grouped := map[string][]*db.StepRound{}
	found := false
	for _, round := range rounds {
		grouped[round.StepResultID] = append(grouped[round.StepResultID], round)
		if round.IsFixRound() {
			found = true
			encoded, _ := json.Marshal(round)
			var recorded map[string]any
			_ = json.Unmarshal(encoded, &recorded)
			if applied, ok := recorded["FixApplied"].(bool); !ok || applied != change {
				t.Errorf("recorded repair outcome = %v, want %v", recorded["FixApplied"], change)
			}
			if !capture && round.StartingHeadSHA != nil {
				t.Fatal("test enabled optional evaluation provenance")
			}
			want := description
			if !change {
				want = NoChangesAppliedSummary
			}
			if round.FixSummary == nil || *round.FixSummary != want {
				t.Errorf("persisted direct repair summary = %v", round.FixSummary)
			}
		}
	}
	if !found {
		t.Fatal("no persisted fixer round")
	}
	md, _ := BuildPipelineSummary(steps, grouped, c.Run.HeadSHA)
	if change && (!strings.Contains(md, "auto-fixed") || !strings.Contains(md, "🔧 Fix applied.") || strings.Contains(md, "result not reported") || strings.Contains(md, "🔧 No changes applied.")) {
		t.Fatalf("ordinary Review rendering:\n%s", md)
	}
	if !change && (!strings.Contains(md, "🔧 No changes applied.") || strings.Contains(md, "auto-fixed")) {
		t.Fatalf("no-op rendering:\n%s", md)
	}
}

func TestRepairOutcomeDirectAgentAndNoop(t *testing.T) {
	for _, step := range []types.StepName{types.StepReview, types.StepDocument, types.StepLint} {
		for _, change := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", step, change), func(t *testing.T) {
				dir, base, head := setupGitRepo(t)
				ag := &mockAgent{runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					if change {
						replayAgentGit(t, ctx, opts, "commit", "--allow-empty", "-m", "guard unsafe value")
					}
					return &agent.Result{Output: []byte(`{"summary":"guard unsafe value","verification":"passed"}`)}, nil
				}}
				c := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
				c.Fixing = true
				summary, err := executeFixMode(c, step, fixExecutionOptions{RequireVerification: true})
				if err != nil {
					t.Fatal(err)
				}
				want := NoChangesAppliedSummary
				if change {
					want = changesAppliedSummary
					if step == types.StepReview {
						want = "guard unsafe value"
					}
				}
				if summary != want {
					t.Fatalf("fix result = %q, want %q", summary, want)
				}
			})
		}
	}
}

func TestRepairOutcomeDescriptionIsNotEvidence(t *testing.T) {
	applied, noChange := true, false
	for _, tc := range []struct {
		name, description, want string
		evidence                *bool
	}{
		{"applied contradicts description", "no changes applied", "🔧 Fix applied.", &applied},
		{"noop contradicts description", "changes applied", "🔧 No changes applied.", &noChange},
		{"no report", "guard unsafe value", "🔧 Fix attempted; result not reported.", nil},
		{"committed without description", "", "🔧 Fix applied.", &applied},
		{"nothing reported", "", "🔧 Fix attempted; result not reported.", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings := `{"findings":[{"id":"f1","severity":"error","description":"unsafe value"}]}`
			md, _ := BuildPipelineSummary([]*db.StepResult{{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted}}, map[string][]*db.StepRound{"review": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}, {Round: 2, Trigger: "auto_fix", FixSummary: &tc.description, FixApplied: tc.evidence}}}, testPipelineHeadSHA)
			if !strings.Contains(md, tc.want) {
				t.Fatalf("round outcome:\n%s", md)
			}
			if tc.evidence == nil && (strings.Contains(md, "auto-fixed") || strings.Contains(md, "🔧 Fix applied.")) {
				t.Fatalf("description claimed activity:\n%s", md)
			}
		})
	}
}
