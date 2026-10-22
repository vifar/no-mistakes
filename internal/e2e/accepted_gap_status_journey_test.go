//go:build e2e && !windows

package e2e

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/shipping"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #78 is the oracle: explicit gap acceptance earns an honestly named
// success and preserves its audit; ordinary approvals and overrides cannot.
// Run the real CLI/daemon, replacing only the external agent and forge. No
// execution proof, accepted gap, or certificate is manufactured in the DB.
func TestAcceptedTestGapCommitStatusJourney(t *testing.T) {
	for _, name := range []string{"accepted", "recovered", "ordinary-approval", "command-override", "clean", "non-strict-gap"} {
		t.Run(name, func(t *testing.T) {
			accepted := name == "accepted" || name == "recovered" || name == "non-strict-gap"
			strict := name == "accepted" || name == "recovered" || name == "clean"
			scenario := certifiedWatcherScenario(t)
			if name != "clean" {
				scenario = acceptedTestGapScenario(t)
			}
			j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: scenario})
			configureLocalGitHubSSH(t, j.h)
			testCommand := "true"
			if name == "command-override" {
				testCommand = "exit 7"
			}
			j.setTrusted("commands:\n  test: '" + testCommand + "'\n  lint: 'true'\nauto_fix:\n  test: 0\n")
			j.setForge(forgeState{Protection: protectionJSON(), Checks: []forgeCheck{greenCheck("build")}})
			var mu sync.Mutex
			var posted []struct{ State, Description, Context string }
			var head string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct{ State, Description, Context string }
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if r.Method != "POST" || r.URL.Path != "/repos/example/closes/statuses/"+head || payload.Context != "no-mistakes" {
					t.Errorf("unexpected status request: %s %s %+v", r.Method, r.URL.Path, payload)
				}
				posted = append(posted, payload)
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			t.Setenv("FAKEAGENT_GH_STATUS_URL", server.URL)
			j.init()
			branch := "feature/gap-status-" + name
			committed := j.h.CommitChange(branch, "feature.txt", "accepted-gap status journey\n", "add feature")
			mu.Lock()
			head = committed
			mu.Unlock()
			args := []string{"axi", "run", "--intent", "Validate accepted Test gap commit status", "--wait", "90s"}
			if strict {
				args = append(args, "--shipping-mode", "full")
			} else {
				args = append(args, "--skip", "ci")
			}
			if out, err := j.h.Run(args...); err != nil {
				t.Fatalf("launch: %v\n%s", err, out)
			}
			const reason = "owner accepted isolated broker gap"
			const coveredBy = "broker integration tests"
			if name != "clean" {
				parked := waitForStepStatus(t, j.h, branch, types.StepTest, types.StepStatusAwaitingApproval, 90*time.Second)
				if name == "recovered" {
					if out, err := j.h.Run("daemon", "restart", "--force"); err != nil {
						t.Fatalf("recover Test gate: %v\n%s", err, out)
					}
					waitForStepStatus(t, j.h, branch, types.StepTest, types.StepStatusAwaitingApproval, 30*time.Second)
				}
				if name == "accepted" {
					for _, approvalReason := range []string{"", reason} {
						out, err := j.h.Run("axi", "respond", "--action", "approve", "--step", "test", "--reason", approvalReason)
						if err == nil || !strings.Contains(out, "--accept-gap") {
							t.Fatalf("strict Test admitted plain approval or lost its gap hint: %v\n%s", err, out)
						}
						t.Logf("strict plain approval refused:\n%s", out)
					}
				}
				args := []string{"axi", "respond", "--action", "approve", "--step", "test", "--reason", reason, "--wait", "90s"}
				if accepted {
					args = append(args, "--accept-gap", "test-verdict", "--covered-by", coveredBy)
				}
				if out, err := j.h.Run(args...); err != nil {
					t.Fatalf("approve Test for %s: %v\n%s", parked.ID, err, out)
				} else {
					t.Logf("approval CLI:\n%s", out)
				}
			}
			var runID string
			if strict {
				runID = j.expectReady(branch, 90*time.Second).ID
			} else {
				run := j.h.WaitForRun(branch, 90*time.Second)
				if run.Status != types.RunCompleted {
					t.Fatalf("ordinary approval did not complete: %+v", run)
				}
				runID = run.ID
			}
			database, err := db.OpenReadOnly(paths.WithRoot(j.h.NMHome).DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			wantState, wantDescription := "failure", "no-mistakes pipeline failed or cancelled"
			if strict || name == "non-strict-gap" {
				wantState, wantDescription = "success", "no-mistakes validation passed"
			}
			if accepted {
				wantDescription = "passed with accepted test gap: " + reason
			}
			var publication db.CommitStatusPublication
			deadline := time.Now().Add(10 * time.Second)
			for {
				publications, err := database.GetCommitStatusPublications(t.Context(), runID)
				if err != nil {
					t.Fatal(err)
				}
				if len(publications) == 1 && publications[0].LastState == wantState {
					publication = publications[0]
					break
				}
				if time.Now().After(deadline) {
					j.h.dumpDebugState()
					t.Fatalf("missing %s publication: %+v", wantState, publications)
				}
				time.Sleep(50 * time.Millisecond)
			}
			mu.Lock()
			receipts, _ := json.Marshal(posted)
			if len(posted) != 2 || posted[0].State != "pending" || posted[1].State != wantState || posted[1].Description != wantDescription {
				t.Errorf("HTTP status receipts: %s", receipts)
			}
			mu.Unlock()
			t.Logf("HTTP status receipts at pushed head %s: %s", head, receipts)
			if j.h.UpstreamBranchSHA(branch) != head || publication.SHA != head {
				t.Fatal("status did not name the exact pushed head")
			}
			if !strict {
				if name == "non-strict-gap" {
					// The run published a real success at the exact head, so
					// it earns the same exact-head status certificate a clean
					// run earns (the certificate follows the gates, not the
					// shipping mode). The certificate binds the accepted gap.
					if publication.StrictCertificate == nil {
						t.Fatal("the published success did not earn a status certificate")
					}
					assertNonStrictAcceptedGapJourneyAudit(t, j, runID, reason, coveredBy)
					return
				}
				if publication.StrictCertificate != nil {
					t.Fatal("ordinary approval/override earned a certificate")
				}
				return
			}
			if accepted {
				assertAcceptedGapJourneyAudit(t, j, runID, branch, head, reason, coveredBy)
			}
			if name == "accepted" {
				j.setForge(forgeState{Protection: protectionJSON()})
				out, err := j.receipt(j.h.WorkDir, runID)
				if err == nil || !strings.Contains(out, "strict readiness") {
					t.Fatalf("strict receipt admitted an empty observed rollup: %v\n%s", err, out)
				}
				j.setForge(forgeState{Protection: protectionJSON(), Checks: []forgeCheck{greenCheck("build")}})
			}
			if name == "recovered" {
				// The Push contract revokes all earlier execution proofs on
				// recovery. Test may earn fresh proof, but Review has not rerun,
				// so this success cannot earn cancellation preservation.
				if publication.StrictCertificate != nil {
					t.Fatal("recovery certified Review without fresh execution")
				}
				assertRecoveredGapProof(t, j.h, database, runID, head)
				if out, err := j.h.Run("axi", "abort"); err != nil {
					t.Fatalf("cancel uncertified watcher: %v\n%s", err, out)
				}
				publications, err := database.GetCommitStatusPublications(t.Context(), runID)
				if err != nil || len(publications) != 1 || publications[0].LastState != "failure" || publications[0].StrictCertificate != nil {
					t.Fatalf("uncertified cancellation preserved success: %+v %v", publications, err)
				}
				if out, err := j.h.Run("daemon", "stop"); err != nil {
					t.Fatalf("stop disposable daemon: %v\n%s", err, out)
				}
				assertNoDaemonProcessesForRoot(t, j.h, "after recovered gap journey")
				mu.Lock()
				defer mu.Unlock()
				if len(posted) != 3 || posted[2].State != "failure" {
					t.Fatalf("uncertified cancellation HTTP receipts: %+v", posted)
				}
				t.Log("recovered approval earned current Test proof; missing Review proof prevented certification and cancellation posted failure")
				return
			}
			if publication.StrictCertificate == nil {
				t.Fatal("live execution did not earn a status certificate")
			}
			var certificate db.StatusCertificate
			if err := json.Unmarshal([]byte(*publication.StrictCertificate), &certificate); err != nil {
				t.Fatal(err)
			}
			durable, err := database.GetRun(runID)
			if err != nil {
				t.Fatal(err)
			}
			epoch, err := database.StatusValidationEpoch(runID)
			if err != nil {
				t.Fatal(err)
			}
			if durable.LastPushedSHA == nil || *durable.LastPushedSHA != head || durable.PushGeneration == nil || certificate.SHA != head || certificate.Epoch != epoch || certificate.PushGeneration != *durable.PushGeneration || certificate.ConfigDigest == "" || certificate.ProofDigest == "" {
				t.Fatalf("certificate lost head/epoch/config/push bindings: %+v %+v", certificate, durable)
			}
			assertWatcherExecutedBindings(t, j.h, runID, certificate)
			t.Logf("earned certificate: %s", *publication.StrictCertificate)
			if out, err := j.h.Run("axi", "abort"); err != nil {
				t.Fatalf("cancel certified watcher: %v\n%s", err, out)
			}
			assertPreservedWatcher(t, j.h, runID, head, types.RunCancelReasonAbortedByUser, publication)
			if out, err := j.h.Run("daemon", "restart"); err != nil {
				t.Fatalf("recover cancelled watcher: %v\n%s", err, out)
			}
			assertPreservedWatcher(t, j.h, runID, head, types.RunCancelReasonAbortedByUser, publication)
			if out, err := j.h.Run("daemon", "stop"); err != nil {
				t.Fatalf("stop disposable daemon: %v\n%s", err, out)
			}
			assertNoDaemonProcessesForRoot(t, j.h, "after gap journey")
			mu.Lock()
			defer mu.Unlock()
			if len(posted) != 2 {
				t.Fatalf("cancellation/recovery replaced success: %+v", posted)
			}
			t.Log("certified watcher cancellation and restart preserved success")
		})
	}
}

func assertRecoveredGapProof(t *testing.T, h *Harness, database *db.DB, runID, head string) {
	t.Helper()
	control, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	var testedHead, digest, findingsJSON string
	var epoch int64
	err = control.QueryRow(`SELECT v.head_sha,v.epoch,v.config_digest,s.findings_json
	 FROM status_validation_steps v JOIN step_results s ON s.id=v.step_id
	 WHERE s.run_id=? AND s.step_name='test'`, runID).Scan(&testedHead, &epoch, &digest, &findingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	currentEpoch, err := database.StatusValidationEpoch(runID)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil || findings.TestedHeadSHA != head || testedHead != head || digest == "" || epoch != currentEpoch || epoch == 0 {
		t.Fatalf("recovered Test proof lost durable bindings: head=%s epoch=%d digest=%s findings=%+v error=%v", testedHead, epoch, digest, findings, err)
	}
	var fresh, staleReview int
	err = control.QueryRow(`SELECT COUNT(*) FROM status_validation_steps v JOIN step_results s ON s.id=v.step_id
	 WHERE s.run_id=? AND s.step_name IN ('test','document','lint') AND v.head_sha=? AND v.epoch=? AND v.config_digest=?`, runID, head, epoch, digest).Scan(&fresh)
	if err != nil || fresh != 3 {
		t.Fatalf("recovered gates do not share current bindings: %d %v", fresh, err)
	}
	err = control.QueryRow(`SELECT COUNT(*) FROM status_validation_steps v JOIN step_results s ON s.id=v.step_id
	 WHERE s.run_id=? AND s.step_name='review'`, runID).Scan(&staleReview)
	if err != nil || staleReview != 0 {
		t.Fatalf("recovery retained stale Review proof: %d %v", staleReview, err)
	}
	t.Logf("fresh recovered Test/Document/Lint proof: head=%s epoch=%d config=%s; Review proof absent", head, epoch, digest)
}

func assertAcceptedGapJourneyAudit(t *testing.T, j *scopeJourney, runID, branch, head, reason, coveredBy string) {
	t.Helper()
	out, err := j.h.Run("axi", "status", "--json")
	if err != nil {
		t.Fatalf("inspect accepted gap: %v\n%s", err, out)
	}
	// The status document and receipt payload are public output contracts.
	if !strings.Contains(out, reason) || !strings.Contains(out, coveredBy) {
		t.Fatalf("run status lost gap audit: %s", out)
	}
	t.Logf("recorded gap status: %s", out)
	raw, err := j.receipt(j.h.WorkDir, runID)
	if err != nil {
		t.Fatal(err)
	}
	var receipt shipping.Payload
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		t.Fatal(err)
	}
	var gap *db.AcceptedGaps
	for _, step := range receipt.Steps {
		if step.Step == "test" {
			gap = step.AcceptedGaps
		}
	}
	if receipt.Phase != "ci-ready" || receipt.LocalHeadSHA != head || receipt.ForgeHeadSHA != head || gap == nil || gap.Reason != reason || gap.CoveredBy != coveredBy || !reflect.DeepEqual(gap.FindingIDs, []string{"test-verdict"}) {
		t.Fatalf("receipt lost accepted gap: %s", raw)
	}
	body := livePRBody(t, j.statePath, branch)
	_, tail, ok := strings.Cut(body, "<!-- no-mistakes-pipeline-attestation:v1 ")
	if !ok {
		t.Fatal("published PR lacks pipeline attestation")
	}
	rawAttestation, _, ok := strings.Cut(tail, " -->")
	if !ok {
		t.Fatal("published attestation is unterminated")
	}
	var attestation struct {
		HeadSHA string `json:"head_sha"`
		Steps   []struct {
			Step string `json:"step"`
			Gap  *struct {
				FindingIDs      []string `json:"finding_ids"`
				ReasonDigest    string   `json:"reason_sha256"`
				CoveredByDigest string   `json:"covered_by_sha256"`
			} `json:"accepted_gaps"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(rawAttestation), &attestation); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, step := range attestation.Steps {
		if step.Step == "test" && step.Gap != nil {
			found = reflect.DeepEqual(step.Gap.FindingIDs, []string{"test-verdict"}) && step.Gap.ReasonDigest == fmt.Sprintf("%x", sha256.Sum256([]byte(reason))) && step.Gap.CoveredByDigest == fmt.Sprintf("%x", sha256.Sum256([]byte(coveredBy)))
		}
	}
	if attestation.HeadSHA != head || !found || !strings.Contains(body, reason) || !strings.Contains(body, coveredBy) {
		t.Fatalf("published attestation lost gap audit: %s", body)
	}
	t.Logf("published gap attestation: %s", rawAttestation)
}

// A plain run has no strict receipt, so its audit is read from the run's own
// records: the durable gap on the Test step and the status document that
// names it.
func assertNonStrictAcceptedGapJourneyAudit(t *testing.T, j *scopeJourney, runID, reason, coveredBy string) {
	t.Helper()
	out, err := j.h.Run("axi", "status", "--json")
	if err != nil {
		t.Fatalf("inspect accepted gap: %v\n%s", err, out)
	}
	if !strings.Contains(out, reason) || !strings.Contains(out, coveredBy) {
		t.Fatalf("run status lost gap audit: %s", out)
	}
	database, err := db.OpenReadOnly(paths.WithRoot(j.h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rows, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.StepName != types.StepTest {
			continue
		}
		gaps, err := row.RecordedAcceptedGaps()
		if err != nil || gaps == nil {
			t.Fatalf("the Test step lost the recorded gap: %+v, %v", row, err)
		}
		if gaps.Reason != reason || gaps.CoveredBy != coveredBy || !reflect.DeepEqual(gaps.FindingIDs, []string{"test-verdict"}) {
			t.Fatalf("recorded gap = %+v", gaps)
		}
		found = true
	}
	if !found {
		t.Fatal("the run holds no Test step")
	}
	t.Logf("non-strict recorded gap audit: reason=%q covered_by=%q", reason, coveredBy)
}
