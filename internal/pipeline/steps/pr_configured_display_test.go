package steps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRConfiguredAttributionEveryDisplay(t *testing.T) {
	t.Parallel()
	const configuredCoauthor = "Co-Authored-By: private:writer <opaque@example.com>"
	const configuredBuild = "Build-Agent: private-identifier"
	const configuredModel = "Build-Identity: acp:private:writer / opaque-model"
	const human = "Co-Authored-By: Claude <claude@example.com>"
	const prose = "The Co-Authored-By: field is documented below.\n- See <https://example.com>."
	const disclosure = "Disclosure: fixtures were generated with the repository recorder."
	const noise = configuredBuild + "\n" + configuredCoauthor + "\n" + configuredModel
	const sample = noise + "\n\n" + human + "\n\n" + prose + "\n\n" + disclosure
	for _, snapshots := range []bool{false, true} {
		for _, templated := range []bool{false, true} {
			for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
				t.Run(fmt.Sprintf("snapshots=%t/template=%t/%s", snapshots, templated, provider), func(t *testing.T) {
					sctx, ag, _ := templateTestContext(t)
					if !templated {
						sctx.Config.PR.Template = ""
					}
					sctx.Config.Commit.Trailers = []string{configuredBuild, configuredCoauthor, "Build-Identity: {{.Agent}} / {{.Model}}"}
					if snapshots {
						sctx.Config.Commit.Trailers = nil
					}
					displaySample := sample
					if !snapshots {
						displaySample += "\nBuild-Identity: acp:late:writer / late-model"
					}
					sctx.UserIntent = "Intent marker\n" + displaySample
					narrative := "## What Changed\n\nNarrative marker\n" + sample
					if templated {
						narrative = filledPRTemplate + "\n\nNarrative marker\n" + displaySample
					}
					ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
						if _, err := sctx.DB.InsertAgentInvocation(db.AgentInvocation{RunID: sctx.Run.ID, StepName: "pr", Round: 1, Purpose: "pr", Agent: "acp:late:writer", Model: "late-model", SessionMode: db.InvocationModeCold, ExitStatus: "ok"}); err != nil {
							t.Fatal(err)
						}
						output, err := json.Marshal(prContent{Title: "fix: attribution context", Body: narrative})
						return &agent.Result{Output: output}, err
					}
					sctx.EvidenceDir = filepath.Join(sctx.WorkDir, "display-evidence")
					if err := os.MkdirAll(sctx.EvidenceDir, 0700); err != nil {
						t.Fatal(err)
					}
					capture := filepath.Join(sctx.EvidenceDir, "display-capture.txt")
					captureText := "Capture marker\nCapture file content proof\n" + displaySample
					if err := os.WriteFile(capture, []byte(captureText), 0600); err != nil {
						t.Fatal(err)
					}
					findings := Findings{Items: verdictFindings(Findings{Verdict: types.TestVerdictInconclusive}), Verdict: types.TestVerdictInconclusive,
						Artifacts: []types.TestArtifact{{Kind: "command-output", Label: "Capture artifact", Path: capture}}}
					payload, err := json.Marshal(findings)
					if err != nil {
						t.Fatal(err)
					}
					rawFindings := string(payload)
					step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
					if err != nil {
						t.Fatal(err)
					}
					if err := sctx.DB.UpdateStepStatus(step.ID, types.StepStatusRunning); err != nil {
						t.Fatal(err)
					}
					if err := sctx.DB.SetStepFindings(step.ID, rawFindings); err != nil {
						t.Fatal(err)
					}
					if _, err := sctx.DB.RecordAcceptedGaps(step.ID, db.AcceptedGaps{FindingIDs: []string{types.FindingIDTestVerdict}, Reason: configuredBuild, CoveredBy: configuredCoauthor}, false); err != nil {
						t.Fatal(err)
					}
					if err := sctx.DB.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
						t.Fatal(err)
					}
					global, repo := []byte(nil), []byte(nil)
					if snapshots {
						global = []byte("{}\n")
						repo = []byte("commit:\n  trailers:\n    - 'Build-Agent: private-identifier'\n    - 'Co-Authored-By: private:writer <opaque@example.com>'\n    - 'Build-Identity: {{.Agent}} / {{.Model}}'\n")
					}
					if _, err := sctx.DB.InsertReviewStepRoundWithProvenance(step.ID, 1, "initial", &rawFindings, nil, sctx.Run.HeadSHA, sctx.Run.BaseSHA, sctx.Run.BaseSHA, global, repo, 1); err != nil {
						t.Fatal(err)
					}
					if _, err := sctx.DB.InsertAgentInvocation(db.AgentInvocation{RunID: sctx.Run.ID, StepName: string(types.StepTest), Round: 1, Purpose: "test", Agent: "acp:private:writer", Model: "opaque-model", SessionMode: db.InvocationModeCold, ExitStatus: "ok"}); err != nil {
						t.Fatal(err)
					}
					before, err := sctx.DB.GetStepResult(step.ID)
					if err != nil {
						t.Fatal(err)
					}
					beforeCommit := gitCmd(t, sctx.WorkDir, "show", "-s", "--format=%B")
					content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, provider, 0)
					if err != nil {
						t.Fatal(err)
					}
					trailers, err := configuredAttributionTrailers(sctx)
					if err != nil {
						t.Fatal(err)
					}
					steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
					md, _, evidence := (&PRStep{}).buildPipelineSectionFor(sctx, provider, templated)
					bodies := []string{content.Body,
						assemblePRBody(sctx, narrative, "Low risk", evidence, md, 0, provider, trailers...),
						buildPRBody(narrative, "Low risk", evidence, md, sctx, provider, trailers...),
					}
					ownedMD := md
					if provider == scm.ProviderBitbucket && !templated {
						ownedMD += "\n\n```text\n" + buildPipelineAttestationWithPolicy(steps, nil, sctx.Run.HeadSHA, pipelineAttestationPolicy{ConfiguredTrailers: trailers}) + "\n```"
					}
					owned, err := composeOwnedPRContent(prOwnedBody{before: "Author text"}, "repair", ownedMD+"\n\nOwned marker\n"+prDisplayText(sample, trailers...), 0)
					if err != nil {
						t.Fatal(err)
					}
					bodies = append(bodies, owned.Body)
					policy := pipelineAttestationPolicy{ConfiguredTrailers: trailers, acceptedGapSteps: steps}
					legacyMD, _ := buildPipelineSummaryFor(steps, nil, sctx.Run.HeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{})
					for _, ownedBody := range []bool{false, true} {
						for _, keepSteps := range []bool{false, true} {
							old := legacyMD + "\n\nRestamp marker\n" + prDisplayText(sample, trailers...)
							if ownedBody {
								old = "Author prefix\n\n" + wrapPRAppendix(old) + "\nAuthor suffix"
							}
							current := steps
							if keepSteps {
								current = nil
							}
							updated, rebound, err := rebindOwnedPRAttestation(old, strings.Repeat("b", 40), current, policy, provider)
							if err != nil || !rebound {
								t.Fatalf("restamp=%t, %v", rebound, err)
							}
							bodies = append(bodies, updated)
						}
					}
					emptyDigest := fmt.Sprintf("%x", sha256.Sum256(nil))
					for i, body := range bodies {
						decoded := html.UnescapeString(body)
						for _, forbidden := range []string{"private-identifier", "opaque@example.com", "opaque-model", "late-model"} {
							if strings.Contains(decoded, forbidden) {
								t.Errorf("body %d retained configured attribution %q", i, forbidden)
							}
						}
						for _, kept := range []string{"preserved human co-author: Claude <claude@example.com>", prose, disclosure} {
							if !strings.Contains(decoded, kept) {
								t.Errorf("body %d lost %q", i, kept)
							}
						}
						if !strings.Contains(decoded, "- Approved: \n- Covered by: \n") {
							t.Errorf("body %d canonical audit differs: %q", i, acceptedGapAuditBlock(decoded))
						}
						if strings.Contains(body, pipelineAttestationCommentPrefix) {
							found := false
							for _, row := range parsePipelineAttestationForTest(t, body).Steps {
								if row.Step == types.StepTest {
									found = true
									if row.AcceptedGaps == nil || row.AcceptedGaps.ReasonDigest != emptyDigest || row.AcceptedGaps.CoveredByDigest != emptyDigest {
										t.Errorf("body %d digests disagree: %+v", i, row.AcceptedGaps)
									}
								}
							}
							if !found {
								t.Fatal("Test attestation missing")
							}
						}
					}
					for _, marker := range []string{"Narrative marker", "Intent marker", "Capture marker", "Capture file content proof"} {
						if !strings.Contains(content.Body, marker) {
							t.Errorf("display missing %q", marker)
						}
					}
					after, err := sctx.DB.GetStepResult(step.ID)
					if err != nil {
						t.Fatal(err)
					}
					if *after.FindingsJSON != rawFindings || *after.AcceptedGapsJSON != *before.AcceptedGapsJSON {
						t.Fatal("raw audit data changed")
					}
					bytes, err := os.ReadFile(capture)
					if err != nil || string(bytes) != captureText {
						t.Fatal("capture file changed")
					}
					if got := gitCmd(t, sctx.WorkDir, "show", "-s", "--format=%B"); got != beforeCommit {
						t.Fatal("commit message changed")
					}
				})
			}
		}
	}
}

func TestPRConfiguredPresentationKeepsProseAndHuman(t *testing.T) {
	t.Parallel()
	trailers := []string{"Build-Agent: private-identifier", "Co-Authored-By: private:writer <opaque@example.com>"}
	const prose = "The Co-Authored-By: field is documented below.\n- See <https://example.com>."
	if got := prDisplayText(prose, trailers...); got != prose {
		t.Errorf("prose changed: %q", got)
	}
	const human = "Co-Authored-By: Claude <claude@example.com>"
	if got := prDisplayText(human, trailers...); got != "preserved human co-author: Claude <claude@example.com>" {
		t.Errorf("human = %q", got)
	}
	for _, trailer := range trailers {
		if got := prDisplayText(trailer, trailers...); got != "" {
			t.Errorf("configured entry relabeled or retained: %q", got)
		}
		if got := prDisplayText(trailer+"\n- Verification passed", trailers...); got != "\n- Verification passed" {
			t.Errorf("bullet absorbed: %q", got)
		}
	}
}

func TestPRWholeBodyRawDisplayBoundary(t *testing.T) {
	t.Parallel()
	for _, configured := range []string{"Build-Agent: /Users/alex/private-identifier", "Build-Agent: /Users/alex/private&#45;identifier", "Build-Agent: /Users/alex/private&amp;copy;"} {
		noise := configured + "\nCo-Authored-By: Codex <ai@example.com>\n&#67;o-Authored-By: &#67;odex &lt;ai@example.com&gt;\nGenerated with &#67;odex\nCo-Authored-By: Claude Smith <human@example.com>\nDisclosure: fixtures were generated with the repository recorder.\nOrdinary prose remains.\nBuild-Agent: public-identifier"
		for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
			for _, templated := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/template=%t/entities=%t", provider, templated, strings.Contains(configured, "&#")), func(t *testing.T) {
					dir, base, head := setupGitRepo(t)
					ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
						body := "## What Changed\n\nNarrative marker\n" + noise
						if templated {
							body = filledPRTemplate + "\n\nNarrative marker\n" + noise
						}
						payload, err := json.Marshal(prContent{Title: "fix: display boundary", Body: body})
						return &agent.Result{Output: payload}, err
					}}
					sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
					sctx.Config.Commit.Trailers = []string{configured}
					enableReviewConversation(sctx.Config)
					sctx.UserIntent = "Intent marker\n" + noise
					if templated {
						if err := os.MkdirAll(filepath.Join(dir, ".github"), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(dir, ".github", "pull_request_template.md"), []byte(testPRTemplate), 0600); err != nil {
							t.Fatal(err)
						}
						gitCmd(t, dir, "add", ".github")
						gitCmd(t, dir, "commit", "-m", "add template fixture")
						sctx.Config.TrustedConfigSHA = gitCmd(t, dir, "rev-parse", "HEAD")
						sctx.Config.PR.Template = ".github/pull_request_template.md"
					}
					sctx.EvidenceDir = filepath.Join(dir, "display-evidence")
					if err := os.MkdirAll(sctx.EvidenceDir, 0700); err != nil {
						t.Fatal(err)
					}
					capture := filepath.Join(sctx.EvidenceDir, "capture.txt")
					if err := os.WriteFile(capture, []byte("Capture marker\n"+noise), 0600); err != nil {
						t.Fatal(err)
					}
					rawByStep := map[string]string{}
					for _, name := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI} {
						sample := func(marker string) string { return marker + " " + string(name) + "\n" + noise }
						items := []Finding{{ID: "f-" + string(name), Severity: "warning", File: sample("File marker"), Description: sample("Description marker")}}
						if name == types.StepTest {
							items = verdictFindings(Findings{Items: items, Verdict: types.TestVerdictInconclusive})
							for i := range items {
								if items[i].ID == types.FindingIDTestVerdict {
									items[i].Description = sample("Verdict marker")
								}
							}
						}
						payload, err := json.Marshal(Findings{Summary: sample("Summary"), RiskLevel: "medium", RiskRationale: sample("Risk marker"),
							Items:  items,
							Tested: []string{sample("Tested marker")}, TestingSummary: sample("Testing marker"), Verdict: types.TestVerdictInconclusive,
							Scenarios: []types.TestScenario{{Name: sample("Scenario marker"), Result: types.ScenarioResultPass, Live: true, Evidence: sample("Evidence marker")}, {Name: sample("Untested marker"), Result: types.ScenarioResultUntested, Reason: sample("Reason marker")}},
							Artifacts: []types.TestArtifact{{Kind: "command-output", Label: sample("Label marker"), Path: capture, Content: sample("Caption marker")}}})
						if err != nil {
							t.Fatal(err)
						}
						raw := string(payload)
						step := insertCompletedStep(t, sctx, name, raw, "")
						fix := sample("Fix marker")
						if _, err := sctx.DB.InsertStepRound(step.ID, 2, "auto_fix", &raw, &fix, 10); err != nil {
							t.Fatal(err)
						}
						rawByStep[step.ID] = raw
						if name == types.StepTest {
							if err := sctx.DB.UpdateStepStatus(step.ID, types.StepStatusRunning); err != nil {
								t.Fatal(err)
							}
							if _, err := sctx.DB.RecordAcceptedGaps(step.ID, db.AcceptedGaps{FindingIDs: []string{types.FindingIDTestVerdict}, Reason: configured, CoveredBy: "Co-Authored-By: Codex <ai@example.com>"}, false); err != nil {
								t.Fatal(err)
							}
							if err := sctx.DB.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
								t.Fatal(err)
							}
						}
					}
					failure := insertCompletedStep(t, sctx, types.StepName("gate.review.failure"), "", "Error marker\n"+noise)
					convDir := reviewConversationDir(sctx)
					for _, entry := range []map[string]string{{"id": "open", "kind": "question", "question": "Question marker\n" + noise}, {"id": "withdrawn", "kind": "question", "question": "Withdrawn question marker\n" + noise}, {"id": "withdrawn", "kind": "retract", "reason": "Withdrawal marker\n" + noise}} {
						payload, err := json.Marshal(entry)
						if err != nil {
							t.Fatal(err)
						}
						if err := appendAgentQuestionLine(convDir, string(payload)); err != nil {
							t.Fatal(err)
						}
					}
					answer := db.ReviewAnswer{RepoID: sctx.Repo.ID, Branch: sctx.Run.Branch, RunID: sctx.Run.ID, QuestionID: "answered", AskOrdinal: 1, Question: "Answered question marker\n" + noise, Answer: "Answer marker\n" + noise, AnsweredBy: "Owner marker\n" + noise}
					if err := sctx.DB.RecordReviewAnswer(answer); err != nil {
						t.Fatal(err)
					}
					content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", base, provider, 0)
					if err != nil {
						t.Fatal(err)
					}
					steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
					policy := pipelineAttestationPolicy{ConfiguredTrailers: []string{configured}, acceptedGapSteps: steps}
					restamped, rebound, err := rebindOwnedPRAttestation(content.Body, strings.Repeat("b", 40), steps, policy, provider)
					if err != nil || !rebound {
						t.Fatalf("restamp=%t, %v", rebound, err)
					}
					for _, body := range []string{content.Body, restamped} {
						decoded := html.UnescapeString(body)
						for _, forbidden := range []string{"private-identifier", "private&#45;identifier", "ai@example.com", "Co-Authored-By:", "Generated with &#67;odex", "Generated with Codex"} {
							if strings.Contains(decoded, forbidden) {
								t.Errorf("published body retained %q", forbidden)
							}
						}
						if !strings.Contains(decoded, "- Approved: \n- Covered by: \n") {
							t.Errorf("configured audit text was not removed: %s", acceptedGapAuditBlock(decoded))
						}
						if strings.Contains(body, pipelineAttestationCommentPrefix) {
							attestation := parsePipelineAttestationForTest(t, body)
							emptyDigest := fmt.Sprintf("%x", sha256.Sum256(nil))
							for _, step := range attestation.Steps {
								if step.Step == types.StepTest && (step.AcceptedGaps == nil || step.AcceptedGaps.ReasonDigest != emptyDigest || step.AcceptedGaps.CoveredByDigest != emptyDigest) {
									t.Error("accepted-gap digests do not bind the empty published text")
								}
							}
						}
						for _, kept := range []string{"Narrative marker", "Intent marker", "File marker", "Description marker", "Tested marker", "Testing marker", "Scenario marker", "Evidence marker", "Reason marker", "Label marker", "Caption marker", "Capture marker", "Risk marker", "Question marker", "Answer marker", "Withdrawal marker", "Error marker", "Accepted gaps", "preserved human co-author: Claude Smith", "human@example.com", "Disclosure: fixtures were generated with the repository recorder.", "Ordinary prose remains.", "Build-Agent: public-identifier"} {
							if !strings.Contains(decoded, kept) {
								t.Errorf("published body lost %q", kept)
							}
						}
					}
					for _, step := range steps {
						if raw, ok := rawByStep[step.ID]; ok {
							if step.FindingsJSON == nil || *step.FindingsJSON != raw {
								t.Fatal("stored findings changed")
							}
							rounds, err := sctx.DB.GetRoundsByStep(step.ID)
							if err != nil {
								t.Fatal(err)
							}
							if len(rounds) != 2 {
								t.Fatalf("round count=%d", len(rounds))
							}
							for _, round := range rounds {
								if round.FindingsJSON == nil || *round.FindingsJSON != raw {
									t.Fatal("stored round changed")
								}
							}
						}
					}
					storedFailure, err := sctx.DB.GetStepResult(failure.ID)
					if err != nil || storedFailure.Error == nil || *storedFailure.Error != "Error marker\n"+noise {
						t.Fatal("stored error changed")
					}
					data, err := os.ReadFile(capture)
					if err != nil || string(data) != "Capture marker\n"+noise {
						t.Fatal("capture changed")
					}
					answers, _, err := sctx.DB.GetBranchReviewAnswers(sctx.Repo.ID, sctx.Run.Branch, db.MaxBranchReviewAnswers)
					if err != nil || len(answers) != 1 || answers[0].Question != answer.Question || answers[0].Answer != answer.Answer {
						t.Fatal("raw conversation answer changed")
					}
				})
			}
		}
	}
}
