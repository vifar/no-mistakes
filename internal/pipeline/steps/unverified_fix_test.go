package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const unverifiedFixFindings = `{"findings":[{"id":"f-1","severity":"error","file":"feature.txt","description":"bug"}],"summary":"1 bug"}`

// reviewFixContext returns a Review fix-round context whose agent runs fixFn
// for the fixer turn and returns a clean review for every later turn.
func reviewFixContext(t *testing.T, fixFn func(dir string) (json.RawMessage, error)) (*pipeline.StepContext, *mockAgent, string, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{name: "test"}
	ag.runFn = func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if len(ag.calls) == 1 {
			out, err := fixFn(dir)
			if err != nil {
				return nil, err
			}
			return &agent.Result{Output: out}, nil
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"ok","risk_scope":"source-or-external","reviewed_paths":["feature.txt"]}`)}, nil
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = unverifiedFixFindings
	return sctx, ag, dir, headSHA
}

func editFeature(dir string) error {
	return os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("fixer edit\n"), 0o644)
}

// Issue #81: a Review fix the fixer could not verify must not be committed,
// so it can never be reviewed as a normal head.
func TestReviewStep_UnverifiedFixIsDiscardedAndStopsTheRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		output string
		want   string
	}{
		{"blocked", `{"summary":"fix admission","verification":"blocked"}`, "blocked"},
		{"failed", `{"summary":"fix admission","verification":"failed"}`, "failed"},
		{"not_needed with changes", `{"summary":"fix admission","verification":"not_needed"}`, "not_needed"},
		{"missing outcome", `{"summary":"fix admission"}`, ""},
		{"unknown outcome", `{"summary":"fix admission","verification":"probably"}`, "probably"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx, ag, dir, headSHA := reviewFixContext(t, func(dir string) (json.RawMessage, error) {
				if err := editFeature(dir); err != nil {
					return nil, err
				}
				if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
					return nil, err
				}
				return json.RawMessage(tc.output), nil
			})

			_, err := (&ReviewStep{}).Execute(sctx)
			var unverified *UnverifiedFixError
			if !errors.As(err, &unverified) {
				t.Fatalf("error = %T %v, want *UnverifiedFixError", err, err)
			}
			if unverified.Step != types.StepReview || unverified.Verification != tc.want {
				t.Fatalf("error = %+v, want review step with verification %q", unverified, tc.want)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
				t.Fatalf("HEAD = %s, want %s: an unverified fix was committed", got, headSHA)
			}
			if sctx.Run.HeadSHA != headSHA {
				t.Fatalf("run head = %s, want %s", sctx.Run.HeadSHA, headSHA)
			}
			if status := gitStatusPorcelain(t, dir); status != "" {
				t.Fatalf("worktree kept unverified changes: %q", status)
			}
			if len(ag.calls) != 1 {
				t.Fatalf("agent calls = %d, want only the fixer: the unverified change was reviewed", len(ag.calls))
			}
		})
	}
}

// A fixer that commits its own unverified work is restored to the starting
// head too; otherwise its commit would be the next head under review.
func TestReviewStep_UnverifiedFixCommittedByTheFixerIsDiscarded(t *testing.T) {
	t.Parallel()
	sctx, _, dir, headSHA := reviewFixContext(t, func(dir string) (json.RawMessage, error) {
		if err := editFeature(dir); err != nil {
			return nil, err
		}
		gitCmd(t, dir, "add", "feature.txt")
		gitCmd(t, dir, "commit", "-m", "fixer's own commit")
		return json.RawMessage(`{"summary":"fix admission","verification":"blocked"}`), nil
	})

	_, err := (&ReviewStep{}).Execute(sctx)
	var unverified *UnverifiedFixError
	if !errors.As(err, &unverified) {
		t.Fatalf("error = %T %v, want *UnverifiedFixError", err, err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want %s", got, headSHA)
	}
}

// The outcome is structured, so a passing fix whose subject names blocked or
// unverified things in the product domain still commits and is reviewed.
func TestReviewStep_VerifiedFixCommitsWhateverItsSummarySays(t *testing.T) {
	t.Parallel()
	sctx, ag, dir, headSHA := reviewFixContext(t, func(dir string) (json.RawMessage, error) {
		if err := editFeature(dir); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"summary":"reject unverified emails blocked by policy","verification":"passed"}`), nil
	})

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatalf("verified fix failed: %v", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got == headSHA {
		t.Fatal("verified fix was not committed")
	}
	if got := lastCommitMessage(t, dir); !strings.Contains(got, "reject unverified emails blocked by policy") {
		t.Fatalf("commit subject = %q", got)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("agent calls = %d, want the fixer and the rereview", len(ag.calls))
	}
}

// A round that changed nothing has nothing unverified to commit, whatever it
// reports, and proceeds to the rereview as before.
func TestReviewStep_UnverifiedRoundWithNoChangesStillRereviews(t *testing.T) {
	t.Parallel()
	sctx, ag, dir, headSHA := reviewFixContext(t, func(string) (json.RawMessage, error) {
		return json.RawMessage(`{"summary":"no changes","verification":"blocked"}`), nil
	})

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatalf("no-change round failed: %v", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want %s", got, headSHA)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("agent calls = %d, want the fixer and the rereview", len(ag.calls))
	}
}

func TestReviewStep_FixTurnAsksForTheVerificationOutcome(t *testing.T) {
	t.Parallel()
	sctx, ag, _, _ := reviewFixContext(t, func(string) (json.RawMessage, error) {
		return json.RawMessage(`{"summary":"no changes","verification":"not_needed"}`), nil
	})

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	fixer := ag.calls[0]
	assertFixSchema(t, fixer.JSONSchema, true)
	for _, want := range []string{
		"run that verification with the repository's own commands",
		"a PATH probe is not a dependency check",
		`Report the outcome in the "verification" field`,
		`Return JSON with "summary" and "verification" fields`,
	} {
		if !strings.Contains(strings.ToLower(fixer.Prompt), strings.ToLower(want)) {
			t.Errorf("fixer prompt missing %q", want)
		}
	}
}

// Only the Review fixer must report an outcome: Test, Lint and custom gates
// rerun their own check after every fix round.
func TestExecuteFixMode_VerificationIsOptIn(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			assertFixSchema(t, opts.JSONSchema, false)
			if err := editFeature(dir); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix lint"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true

	summary, err := executeFixMode(sctx, types.StepLint, fixExecutionOptions{FallbackSummary: "fix lint"})
	if err != nil {
		t.Fatal(err)
	}
	if summary != changesAppliedSummary {
		t.Fatalf("summary = %q, want %q", summary, changesAppliedSummary)
	}
}

func assertFixSchema(t *testing.T, raw json.RawMessage, verificationRequired bool) {
	t.Helper()
	var schema struct {
		Type       string   `json:"type"`
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "object" || schema.Properties["summary"].Type != "string" {
		t.Fatalf("schema must describe an object with a string summary: %s", raw)
	}
	wantRequired := []string{"summary"}
	if verificationRequired {
		wantRequired = append(wantRequired, "verification")
		property := schema.Properties["verification"]
		wantOutcomes := []string{"blocked", "failed", "not_needed", "passed"}
		slices.Sort(property.Enum)
		if property.Type != "string" || !slices.Equal(property.Enum, wantOutcomes) {
			t.Fatalf("verification must allow exactly the four specified outcomes: %s", raw)
		}
	} else if _, exists := schema.Properties["verification"]; exists {
		t.Fatalf("ordinary fixer schema exposes verification: %s", raw)
	}
	slices.Sort(schema.Required)
	if !slices.Equal(schema.Required, wantRequired) {
		t.Fatalf("required = %v, want %v", schema.Required, wantRequired)
	}
}

func TestExecuteFixMode_ErrorsDiscardVerificationRequiredChanges(t *testing.T) {
	t.Parallel()
	agentErr := errors.New("adapter rejected structured output")
	hookErr := errors.New("post-agent hook failed")
	for _, failure := range []string{"agent", "timeout", "hook", "summary"} {
		for _, committed := range []bool{false, true} {
			for _, requireVerification := range []bool{false, true} {
				name := failure
				if committed {
					name += "/committed"
				} else {
					name += "/edited"
				}
				if requireVerification {
					name += "/review"
				} else {
					name += "/ordinary"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					dir, baseSHA, headSHA := setupGitRepo(t)
					gitCmd(t, dir, "checkout", "--detach", headSHA)
					sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
					sctx.Fixing = true
					ctx, cancel := context.WithCancel(sctx.Ctx)
					defer cancel()
					sctx.Ctx = ctx
					wantErr := agentErr
					switch failure {
					case "timeout":
						wantErr = context.DeadlineExceeded
					case "hook":
						wantErr = hookErr
					case "summary":
						wantErr = errRejectedCommitSummary
					}
					opts := fixExecutionOptions{
						RequireVerification: requireVerification,
						RunAgent: func(agent.RunOpts) (*agent.Result, error) {
							if err := editFeature(dir); err != nil {
								t.Fatal(err)
							}
							if committed {
								gitCmd(t, dir, "add", "feature.txt")
								gitCmd(t, dir, "commit", "-m", "fixer commit")
							}
							if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new"), 0o644); err != nil {
								t.Fatal(err)
							}
							switch failure {
							case "agent":
								return nil, agentErr
							case "timeout":
								cancel()
								return nil, context.DeadlineExceeded
							case "summary":
								output, err := json.Marshal(map[string]string{"summary": strings.Repeat("x", config.MaxFixMessageSummaryBytes+1), "verification": "blocked"})
								return &agent.Result{Output: output}, err
							}
							return &agent.Result{Output: json.RawMessage(`{"summary":"fix","verification":"passed"}`)}, nil
						},
					}
					if failure == "hook" {
						opts.AfterAgentRun = func(*agent.Result) error { return hookErr }
					}
					_, err := executeFixMode(sctx, types.StepReview, opts)
					if !errors.Is(err, wantErr) {
						t.Fatalf("error = %v, want %v", err, wantErr)
					}
					if got := gitCmd(t, dir, "rev-parse", "HEAD"); (got == headSHA) != (requireVerification || !committed) {
						t.Fatalf("HEAD = %s, starting head = %s, verification required = %v", got, headSHA, requireVerification)
					}
					if status := gitStatusPorcelain(t, dir); (status == "") != requireVerification {
						t.Fatalf("status = %q, verification required = %v", status, requireVerification)
					}
				})
			}
		}
	}
}
