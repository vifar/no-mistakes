package steps

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// acceptedGapCommitStatusFixture completes Review, Test, Document and Lint for
// head. mutate shapes the Test row while it is still active, so each case
// records its exception exactly as the executor would before completion.
func acceptedGapCommitStatusFixture(t *testing.T, mutate func(sc *pipeline.StepContext, testStepID, head string)) (*pipeline.StepContext, string) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sc.ForgeContext = &forgecontext.Context{Provider: scm.ProviderGitHub, Host: "github.com"}
	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
		step, err := sc.DB.InsertStepResult(sc.Run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := sc.DB.SetStepFindings(step.ID, fmt.Sprintf(`{"findings":[],"tested_head_sha":%q}`, head)); err != nil {
			t.Fatal(err)
		}
		if name == types.StepTest && mutate != nil {
			if err := sc.DB.UpdateStepStatus(step.ID, types.StepStatusRunning); err != nil {
				t.Fatal(err)
			}
			mutate(sc, step.ID, head)
			if row, err := sc.DB.GetStepResult(step.ID); err != nil || row.Status == types.StepStatusFailed {
				continue
			}
		}
		if name == types.StepReview {
			err = sc.DB.CompleteReviewStep(step.ID, sc.Run.ID, head, 0, 1, "review.log")
		} else {
			err = sc.DB.CompleteStep(step.ID, 0, 1, "step.log")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return sc, head
}

func inconclusiveTestFindings(head string) string {
	return fmt.Sprintf(`{"findings":[{"id":"test-verdict","severity":"warning","action":"ask-user","category":"test-verdict","description":"live validation verdict: inconclusive (9 of 13 scenarios were driven live); untested: authenticated broker workflows"}],"tested_head_sha":%q,"verdict":"inconclusive"}`, head)
}

func recordAcceptedTestGap(t *testing.T, sc *pipeline.StepContext, id, head, reason string) {
	t.Helper()
	if err := sc.DB.SetStepFindings(id, inconclusiveTestFindings(head)); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.DB.RecordAcceptedGaps(id, db.AcceptedGaps{FindingIDs: []string{"test-verdict"}, Reason: reason, CoveredBy: acceptedGapTestCoveredBy}, true); err != nil {
		t.Fatal(err)
	}
}

// postCommitStatus runs the reporter for one owned publication and returns
// every request it sent. completeRun marks the run completed after the
// publication is owned, as the executor does before its terminal report.
func postCommitStatus(t *testing.T, sc *pipeline.StepContext, publication db.CommitStatusPublication, state string, completeRun bool) []statusRequest {
	t.Helper()
	if err := sc.DB.PutCommitStatusPublication(sc.Run.ID, publication); err != nil {
		t.Fatal(err)
	}
	if completeRun {
		if err := sc.DB.UpdateRunStatus(sc.Run.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var requests []statusRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ State, Context, Description string }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		mu.Lock()
		requests = append(requests, statusRequest{Path: r.URL.Path, State: payload.State, Context: payload.Context, Description: payload.Description})
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	env, _ := fakeGH(t, "")
	sc.Env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL)
	commitStatusReporter(sc, &publication)(sc.Ctx, state, "", func(line string) { t.Log(line) })
	mu.Lock()
	defer mu.Unlock()
	return append([]statusRequest(nil), requests...)
}

func TestCommitStatusAcceptedTestGapPostsSuccessNamingTheGap(t *testing.T) {
	reason := "owner approved via firstmate on controller#8821, 2026-10-19: 9 of 13 scenarios ran live and the 4 authenticated Kraken workflows have no live broker runtime"
	sc, head := acceptedGapCommitStatusFixture(t, func(sc *pipeline.StepContext, id, head string) {
		recordAcceptedTestGap(t, sc, id, head, reason)
	})
	if !localValidationPassed(sc, head) || !localValidationPassed(sc, "") {
		t.Fatal("an accepted, recorded Test gap must count as local validation")
	}
	requests := postCommitStatus(t, sc, db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true, LastState: "pending"}, "success", false)
	if len(requests) != 1 || requests[0].State != "success" || requests[0].Context != "no-mistakes" {
		t.Fatalf("requests = %+v, want one no-mistakes success", requests)
	}
	description := requests[0].Description
	if !strings.HasPrefix(description, "passed with accepted test gap: owner approved via firstmate") {
		t.Fatalf("description %q does not name the accepted gap", description)
	}
	if n := utf8.RuneCountInString(description); n > maxCommitStatusDescriptionRunes {
		t.Fatalf("description has %d runes, over GitHub's %d", n, maxCommitStatusDescriptionRunes)
	}
	if !strings.HasSuffix(description, "...") {
		t.Fatalf("long reason was not visibly truncated: %q", description)
	}
	// The gap stays recorded and reported; it is not converted to a clean pass.
	steps, err := sc.DB.GetStepsByRun(sc.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepTest && (step.ValidAcceptedGaps() == nil || step.TestOverrideReason() == "") {
			t.Fatalf("accepted gap no longer recorded on the Test step: %+v", step)
		}
	}
	// A completed run keeps the same honest success instead of turning red.
	if again := postCommitStatus(t, sc, db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true, LastState: "pending"}, "success", true); len(again) != 1 || again[0].State != "success" || again[0].Description != description {
		t.Fatalf("completed accepted-gap run posted %+v", again)
	}
}

func TestCommitStatusFailedTestStillPostsFailure(t *testing.T) {
	sc, head := acceptedGapCommitStatusFixture(t, func(sc *pipeline.StepContext, id, head string) {
		if err := sc.DB.CompleteStepWithStatus(id, types.StepStatusFailed, 1, 1, "test.log"); err != nil {
			t.Fatal(err)
		}
	})
	if localValidationPassed(sc, head) {
		t.Fatal("a failed Test counted as local validation")
	}
	requests := postCommitStatus(t, sc, db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", LastState: "pending"}, "success", true)
	if len(requests) != 1 || requests[0].State != "failure" {
		t.Fatalf("requests = %+v, want failure", requests)
	}
}

func TestCommitStatusOtherTestExceptionsStillBlockSuccess(t *testing.T) {
	cases := map[string]func(sc *pipeline.StepContext, id, head string){
		"unresolved error override": func(sc *pipeline.StepContext, id, head string) {
			recordAcceptedTestGap(t, sc, id, head, acceptedGapTestReason)
			if err := sc.DB.SetStepOverrideReason(id, "unresolved error: configured test command failed"); err != nil {
				t.Fatal(err)
			}
		},
		"ordinary Test approval": func(sc *pipeline.StepContext, id, head string) {
			if err := sc.DB.SetStepFindings(id, inconclusiveTestFindings(head)); err != nil {
				t.Fatal(err)
			}
			if err := sc.DB.SetTestApprovalReason(id, "approved without an accepted gap"); err != nil {
				t.Fatal(err)
			}
		},
		"gap hiding an error finding": func(sc *pipeline.StepContext, id, head string) {
			recordAcceptedTestGap(t, sc, id, head, acceptedGapTestReason)
			findings := strings.Replace(inconclusiveTestFindings(head), `"findings":[`, `"findings":[{"id":"broken","severity":"error","action":"auto-fix","description":"scenario failed"},`, 1)
			if err := sc.DB.SetStepFindings(id, findings); err != nil {
				t.Fatal(err)
			}
		},
		"no-go verdict": func(sc *pipeline.StepContext, id, head string) {
			recordAcceptedTestGap(t, sc, id, head, acceptedGapTestReason)
			if err := sc.DB.SetStepFindings(id, strings.Replace(inconclusiveTestFindings(head), `"verdict":"inconclusive"`, `"verdict":"no-go"`, 1)); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			sc, head := acceptedGapCommitStatusFixture(t, mutate)
			if localValidationPassed(sc, head) || localValidationPassed(sc, "") {
				t.Fatal("a Test exception other than a valid accepted gap counted as local validation")
			}
			requests := postCommitStatus(t, sc, db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", LastState: "pending"}, "success", true)
			if len(requests) != 1 || requests[0].State != "failure" {
				t.Fatalf("requests = %+v, want failure", requests)
			}
		})
	}
	t.Run("overridden Review beside an accepted gap", func(t *testing.T) {
		sc, head := acceptedGapCommitStatusFixture(t, func(sc *pipeline.StepContext, id, head string) {
			recordAcceptedTestGap(t, sc, id, head, acceptedGapTestReason)
		})
		steps, err := sc.DB.GetStepsByRun(sc.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range steps {
			if step.StepName == types.StepReview {
				if err := sc.DB.SetStepOverrideReason(step.ID, "unresolved error finding approved"); err != nil {
					t.Fatal(err)
				}
			}
		}
		if localValidationPassed(sc, head) {
			t.Fatal("an accepted Test gap excused an overridden Review")
		}
	})
}

func TestCommitStatusCleanRunIsUnchanged(t *testing.T) {
	sc, head := acceptedGapCommitStatusFixture(t, nil)
	if !localValidationPassed(sc, head) {
		t.Fatal("clean run did not validate")
	}
	requests := postCommitStatus(t, sc, db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", CIEligible: true, LastState: "pending"}, "success", false)
	if len(requests) != 0 {
		t.Fatalf("unvalidated CI-eligible publication posted before exact-head readiness: %+v", requests)
	}
	publication := db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true, LastState: "pending"}
	requests = postCommitStatus(t, sc, publication, "success", false)
	if len(requests) != 1 || requests[0].State != "success" || requests[0].Description != "no-mistakes validation passed" {
		t.Fatalf("requests = %+v, want the unchanged clean success", requests)
	}
}
