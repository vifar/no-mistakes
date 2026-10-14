package steps

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func strictStatusContext(t *testing.T) (*pipeline.StepContext, string, db.CommitStatusPublication) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	sc := newTestContext(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	path := filepath.Join(t.TempDir(), "status.sqlite")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	repo, err := d.InsertRepo(dir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", head, base)
	if err != nil {
		t.Fatal(err)
	}
	sc.DB, sc.Repo, sc.Run = d, repo, run
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: "fingerprint", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
		step, err := d.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if name == types.StepTest {
			if err := d.SetStepFindings(step.ID, fmt.Sprintf(`{"findings":[],"tested_head_sha":%q}`, head)); err != nil {
				t.Fatal(err)
			}
		}
		if err := d.CompleteStep(step.ID, 0, 1, ""); err != nil {
			t.Fatal(err)
		}
		if err := d.RecordStatusValidationStep(step.ID, run.ID, head, pipeline.StatusConfigDigest(sc.Config), 0); err != nil {
			t.Fatal(err)
		}
	}
	p := db.CommitStatusPublication{SHA: head, Host: "github.com", Repo: "test/repo", Validated: true}
	if err := d.PutCommitStatusPublication(run.ID, p); err != nil {
		t.Fatal(err)
	}
	return sc, path, p
}

func TestCommitStatus_ExistingCertificateCannotSurviveUncertainReplacingPost(t *testing.T) {
	for _, state := range []string{"pending", "failure", "success"} {
		for _, failure := range []string{"failed POST", "ambiguous POST", "ledger save"} {
			for _, cancelFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/cancel_first_%v", state, failure, cancelFirst), func(t *testing.T) {
					sc, path, p := strictStatusContext(t)
					var mu sync.Mutex
					var requests []string
					mode := ""
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var payload struct{ State string }
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Error(err)
						}
						mu.Lock()
						requests = append(requests, payload.State)
						currentMode := mode
						mu.Unlock()
						if currentMode != "" {
							stored, err := sc.DB.GetCommitStatusPublication(context.Background(), sc.Run.ID, p)
							if err != nil || stored.StrictCertificate != nil {
								t.Errorf("request preceded durable revocation: %+v,%v", stored, err)
							}
						}
						if currentMode == "failed POST" {
							http.Error(w, "injected failure", 503)
							return
						}
						if currentMode == "ambiguous POST" {
							conn, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							conn.Close()
							return
						}
						w.WriteHeader(201)
						fmt.Fprint(w, `{}`)
					}))
					defer server.Close()
					env, _ := fakeGH(t, "")
					env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL)
					for key, value := range environmentEntries(t, env) {
						t.Setenv(key, value)
					}
					report := func(state string) { (&PushStep{}).ReportPublishedHeadStatus(sc, state, "") }
					report("success")
					stored, err := sc.DB.GetCommitStatusPublication(context.Background(), sc.Run.ID, p)
					if err != nil || stored.StrictCertificate == nil || stored.LastState != "success" {
						t.Fatalf("initial actual POST missing proof: %+v,%v", stored, err)
					}
					if cancelFirst {
						if err := sc.DB.TerminalizeCertifiedWatcher(sc.Run.ID, types.RunCancelReasonAbortedByUser, p.SHA, pipeline.StatusConfigDigest(sc.Config), types.RunCancelled); err != nil {
							t.Fatal(err)
						}
					}
					if state == "success" && !cancelFirst {
						refreshStatusFixture(t, sc, p)
					}
					control, err := sql.Open("sqlite", path)
					if err != nil {
						t.Fatal(err)
					}
					defer control.Close()
					if failure == "ledger save" {
						condition := fmt.Sprintf("NEW.last_state=%q", state)
						if state == "success" {
							condition += " AND NEW.strict_certificate IS NOT NULL"
						}
						query := fmt.Sprintf(`CREATE TRIGGER fail_status_save BEFORE UPDATE ON commit_status_publications WHEN %s BEGIN SELECT RAISE(FAIL,'injected ledger failure'); END`, condition)
						if _, err := control.Exec(query); err != nil {
							t.Fatal(err)
						}
					}
					mu.Lock()
					mode = failure
					mu.Unlock()
					report(state)
					mu.Lock()
					count := len(requests)
					mode = ""
					mu.Unlock()
					if cancelFirst {
						if count != 1 {
							t.Fatalf("preserved terminal outcome sent %d requests", count)
						}
					} else {
						if count != 2 {
							t.Fatalf("replacing request count=%d,want 2", count)
						}
						stored, err = sc.DB.GetCommitStatusPublication(context.Background(), sc.Run.ID, p)
						if err != nil || stored.StrictCertificate != nil {
							t.Fatalf("uncertainty retained certificate: %+v,%v", stored, err)
						}
						if err := sc.DB.TerminalizeCertifiedWatcher(sc.Run.ID, types.RunCancelReasonAbortedByUser, p.SHA, pipeline.StatusConfigDigest(sc.Config), types.RunCancelled); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := control.Exec(`DROP TRIGGER IF EXISTS fail_status_save`); err != nil {
						t.Fatal(err)
					}
					if err := sc.DB.Close(); err != nil {
						t.Fatal(err)
					}
					reopened, err := db.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					defer reopened.Close()
					sc.DB = reopened
					ids, err := reopened.CommitStatusRecoveryRunIDs(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if cancelFirst && len(ids) != 0 {
						t.Fatalf("restart lost preservation: %v", ids)
					}
					if !cancelFirst && (len(ids) != 1 || ids[0] != sc.Run.ID) {
						t.Fatalf("uncertain failure retry missing: %v", ids)
					}
					report("pending")
					report("success")
					report("failure")
					mu.Lock()
					defer mu.Unlock()
					want := 1
					if !cancelFirst {
						want = 3
					}
					if len(requests) != want {
						t.Fatalf("terminal callbacks = %v,want %d requests", requests, want)
					}
					if !cancelFirst && requests[len(requests)-1] != "failure" {
						t.Fatalf("terminal failure overwritten: %v", requests)
					}
				})
			}
		}
	}
}

func TestCommitStatus_SameHeadFreshValidationRequiresNewSuccessPost(t *testing.T) {
	sc, _, p := strictStatusContext(t)
	var states []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ State string }
		json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		states = append(states, payload.State)
		mu.Unlock()
		w.WriteHeader(201)
	}))
	defer server.Close()
	env, _ := fakeGH(t, "")
	env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL)
	for key, value := range environmentEntries(t, env) {
		t.Setenv(key, value)
	}
	(&PushStep{}).ReportPublishedHeadStatus(sc, "success", "")
	if err := sc.DB.ResetStepsFrom(sc.Run.ID, types.StepReview.Order()); err != nil {
		t.Fatal(err)
	}
	steps, err := sc.DB.GetStepsByRun(sc.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := sc.DB.StatusValidationEpoch(sc.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepTest {
			if err := sc.DB.SetStepFindings(step.ID, fmt.Sprintf(`{"findings":[],"tested_head_sha":%q}`, p.SHA)); err != nil {
				t.Fatal(err)
			}
		}
		if err := sc.DB.CompleteStep(step.ID, 0, 1, ""); err != nil {
			t.Fatal(err)
		}
		if err := sc.DB.RecordStatusValidationStep(step.ID, sc.Run.ID, p.SHA, pipeline.StatusConfigDigest(sc.Config), epoch); err != nil {
			t.Fatal(err)
		}
	}
	(&PushStep{}).ReportPublishedHeadStatus(sc, "success", "")
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(states) != "[success success]" {
		t.Fatalf("fresh proof reused old POST: %v", states)
	}
	if err := sc.DB.TerminalizeCertifiedWatcher(sc.Run.ID, types.RunCancelReasonAbortedByUser, p.SHA, pipeline.StatusConfigDigest(sc.Config), types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	stored, err := sc.DB.GetCommitStatusPublication(context.Background(), sc.Run.ID, p)
	if err != nil || stored.PreserveSuccess == nil || !*stored.PreserveSuccess {
		t.Fatalf("fresh proof not preserved: %+v,%v", stored, err)
	}
}

func refreshStatusFixture(t *testing.T, sc *pipeline.StepContext, p db.CommitStatusPublication) {
	t.Helper()
	sc.Config.Commands.Lint = "new-validation-config"
	steps, err := sc.DB.GetStepsByRun(sc.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := sc.DB.StatusValidationEpoch(sc.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if err := sc.DB.CompleteStep(step.ID, 0, 1, ""); err != nil {
			t.Fatal(err)
		}
		if err := sc.DB.RecordStatusValidationStep(step.ID, sc.Run.ID, p.SHA, pipeline.StatusConfigDigest(sc.Config), epoch); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCommitStatus_FailedInvalidationPreventsPost(t *testing.T) {
	for _, state := range []string{"pending", "failure", "success"} {
		t.Run(state, func(t *testing.T) {
			sc, path, p := strictStatusContext(t)
			var mu sync.Mutex
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests++
				mu.Unlock()
				w.WriteHeader(201)
			}))
			defer server.Close()
			env, _ := fakeGH(t, "")
			env = append(env, "FAKE_CLI_STATUS_API_URL="+server.URL)
			for key, value := range environmentEntries(t, env) {
				t.Setenv(key, value)
			}
			(&PushStep{}).ReportPublishedHeadStatus(sc, "success", "")
			if state == "success" {
				refreshStatusFixture(t, sc, p)
			}
			control, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			if _, err := control.Exec(`CREATE TRIGGER reject_revocation BEFORE UPDATE ON commit_status_publications WHEN OLD.strict_certificate IS NOT NULL AND NEW.strict_certificate IS NULL BEGIN SELECT RAISE(FAIL,'injected revocation failure'); END`); err != nil {
				t.Fatal(err)
			}
			(&PushStep{}).ReportPublishedHeadStatus(sc, state, "")
			mu.Lock()
			defer mu.Unlock()
			if requests != 1 {
				t.Fatalf("failed revocation allowed outbound %s: %d requests", state, requests)
			}
		})
	}
}
