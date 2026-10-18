package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDocumentRestartReadsNoOpPass(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":""}`)}, nil
	}}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	passes, err := sc.DB.GetDocumentPasses(sc.Run.ID)
	if err != nil || len(passes) != 1 || passes[0].CommitCount != 0 {
		t.Fatalf("no-op not persisted: %+v %v", passes, err)
	}
	writeAndCommit("feature.txt", "formatted\n")(t, dir)
	sc.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, sc.Run.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("restart made %d calls, want 2", calls)
	}
}

func TestDocumentCarryDoesNotRebindLiveAttestation(t *testing.T) {
	for _, caller := range []string{"Document", "Push", "no carry"} {
		t.Run(caller, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if opts.Purpose == "document" {
					writeAndCommit("README.md", "guide\n")(t, dir)
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"guide"}`)}, nil
				}
				return &agent.Result{Output: json.RawMessage(`{"title":"fix: guide","body":"Documentation repair."}`)}, nil
			}}
			sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
			recordReviewApproval(t, sc, head)
			raw := liveValidatedFindingsJSON(t, []types.TestScenario{{Name: "command completes", Result: types.ScenarioResultPass, Live: true, Evidence: "command output"}}, types.TestVerdictGo, head)
			testStep := insertCompletedStep(t, sc, types.StepTest, raw, "")
			before, err := (&PRStep{}).buildPRContent(sc, "feature", "main", base, scm.ProviderGitHub, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := parsePipelineAttestationForTest(t, before.Body).LiveValidation; got == nil || got.Live != 1 {
				t.Fatalf("original live claim missing: %+v", got)
			}
			if caller == "Document" {
				if _, err := (&DocumentStep{}).Execute(sc); err != nil {
					t.Fatal(err)
				}
			} else if caller == "Push" {
				writeAndCommit("README.md", "guide\n")(t, dir)
				to := gitCmd(t, dir, "rev-parse", "HEAD")
				recordDocumentPass(t, sc, head, to)
				sc.Run.HeadSHA = to
				if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, to); err != nil {
					t.Fatal(err)
				}
				if restart, err := reviewRestartNeededForPush(sc, "", to); err != nil || restart {
					t.Fatalf("credit not carried: %v %v", restart, err)
				}
			}
			run, err := sc.DB.GetRun(sc.Run.ID)
			if err != nil || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != sc.Run.HeadSHA || testedHead(t, sc) != sc.Run.HeadSHA {
				t.Fatalf("atomic credit missing: %+v %v", run, err)
			}
			rounds, err := sc.DB.GetRoundsByStep(testStep.ID)
			if err != nil || len(rounds) != 1 || rounds[0].FindingsJSON == nil || *rounds[0].FindingsJSON != raw {
				t.Fatalf("historical evidence changed: %+v %v", rounds, err)
			}
			after, err := (&PRStep{}).buildPRContent(sc, "feature", "main", base, scm.ProviderGitHub, 0)
			if err != nil {
				t.Fatal(err)
			}
			wantLive := caller == "no carry"
			if got := parsePipelineAttestationForTest(t, after.Body).LiveValidation; (got != nil) != wantLive {
				t.Errorf("PR live validation=%+v want present=%v", got, wantLive)
			}
			bodyFile := filepath.Join(t.TempDir(), "pr-body.md")
			if err := os.WriteFile(bodyFile, []byte(before.Body), 0o644); err != nil {
				t.Fatal(err)
			}
			prURL := "https://github.com/test/repo/pull/42"
			sc.Run.PRURL = &prURL
			bindPublicationComparison(t, sc, sc.Run.HeadSHA)
			sc.Env = append(fakeCIGH(t, "OPEN", `[]`), "FAKE_CLI_PR_HEAD_SHA="+sc.Run.HeadSHA, "FAKE_CLI_PR_BODY_FILE="+bodyFile, "FAKE_CLI_PR_TITLE=fix: guide")
			steps, err := sc.DB.GetStepsByRun(sc.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := attestHeadBeforePush(sc, sc.Run.HeadSHA, steps); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(bodyFile)
			if err != nil {
				t.Fatal(err)
			}
			if got := parsePipelineAttestationForTest(t, string(body)); got.HeadSHA != sc.Run.HeadSHA || (got.LiveValidation != nil) != wantLive {
				t.Errorf("restamp claims unexercised head: %+v", got)
			}
		})
	}
}

func TestDocumentHistoryStillRequiresSummaryForWork(t *testing.T) {
	for _, commits := range []int{0, 1} {
		t.Run(fmt.Sprint(commits), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) { calls++; return nil, nil }}
			sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
			id, err := sc.DB.BeginDocumentPass(sc.Run.ID, head)
			if err != nil {
				t.Fatal(err)
			}
			raw := `{"findings":[{"severity":"warning","description":"gap","action":"ask-user"}],"summary":""}`
			if commits > 0 {
				writeAndCommit("README.md", "guide\n")(t, dir)
				sc.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
				if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, sc.Run.HeadSHA); err != nil {
					t.Fatal(err)
				}
				raw = `{"findings":[],"summary":""}`
			}
			if err := sc.DB.CompleteDocumentPass(id, sc.Run.HeadSHA, commits, raw); err != nil {
				t.Fatal(err)
			}
			if _, err := (&DocumentStep{}).Execute(sc); err == nil || !strings.Contains(err.Error(), "missing summary") || calls != 0 {
				t.Fatalf("invalid history accepted: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestAttestationRoundEvidenceFailsClosed(t *testing.T) {
	raw := liveValidatedFindingsJSON(t, []types.TestScenario{{Name: "command completes", Result: types.ScenarioResultPass, Live: true, Evidence: "command output"}}, types.TestVerdictGo)
	stale := liveValidatedFindingsJSON(t, []types.TestScenario{{Name: "command completes", Result: types.ScenarioResultPass, Live: true, Evidence: "command output"}}, types.TestVerdictGo, strings.Repeat("b", 40))
	noAnchor := `{"findings":[],"scenarios":[{"name":"command completes","result":"pass","live":true}],"verdict":"go"}`
	malformed := `{"scenarios":`
	for _, tc := range []struct {
		name     string
		raw      *string
		wantLive bool
	}{
		{"same head", &raw, true}, {"carried head", &stale, false}, {"missing anchor", &noAnchor, false}, {"malformed evidence", &malformed, false}, {"missing evidence", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []*db.StepResult{{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &raw}}
			rounds := map[string][]*db.StepRound{"test": {{Round: 1, FindingsJSON: tc.raw}}}
			got := parsePipelineAttestationForTest(t, buildPipelineAttestation(steps, rounds, testPipelineHeadSHA))
			if (got.LiveValidation != nil) != tc.wantLive {
				t.Fatalf("live validation=%+v want present=%v", got.LiveValidation, tc.wantLive)
			}
			projected := testStepWithRoundEvidence(steps[0], rounds["test"])
			rebound, ok := rebindPipelineAttestationWithSteps(buildPipelineAttestation(steps, nil, testPipelineHeadSHA), testPipelineHeadSHA, []*db.StepResult{projected}, pipelineAttestationPolicy{})
			if !ok || (parsePipelineAttestationForTest(t, rebound).LiveValidation != nil) != tc.wantLive {
				t.Fatalf("restamp disagrees: %s", rebound)
			}
		})
	}
}
