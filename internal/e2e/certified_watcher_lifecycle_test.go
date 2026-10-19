//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// This is deterministic integration evidence, not live model/forge validation.
// The oracle is the cancellation contract: four actually executed production
// gates and an acknowledged HTTP SUCCESS earn preservation; cancellation must
// remain visible and cannot issue another POST, even after reopening/recovery.
// Only external agent and forge responses are fixtures; no DB proof is seeded.
func TestCertifiedWatcherProductionLifecycle(t *testing.T) {
	actions := []string{"operator", "planned-stop"}
	if runtime.GOOS != "windows" {
		actions = append(actions, "raw-stop", "raw-stop-storage-recovery")
	}
	if runtime.GOOS == "darwin" {
		actions = append(actions, "uninstall")
	}
	for _, action := range actions {
		t.Run(action, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: certifiedWatcherScenario(t)})
			const repoURL = "https://github.com/example/repo.git"
			configureGitURLRewrite(t, h, repoURL, h.UpstreamDir)
			if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", repoURL); err != nil {
				t.Fatalf("set origin: %v\n%s", err, out)
			}
			// These commands are independent execution receipts, not real linters
			// or a substitute for the Test evidence turn. Both run through the
			// production configured-command paths from trusted main.
			repoConfig := "allow_repo_commands: true\ncommands:\n  test: 'printf \"test\\n\" >> \"$NM_HOME/command-receipts\"'\n  lint: 'printf \"lint\\n\" >> \"$NM_HOME/command-receipts\"'\n"
			if err := os.WriteFile(filepath.Join(h.WorkDir, ".no-mistakes.yaml"), []byte(repoConfig), 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"add", ".no-mistakes.yaml"}, {"commit", "-m", "configure boundary receipts"}, {"push", "origin", "main"}} {
				if out, err := h.runGit(t.Context(), h.WorkDir, args...); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			t.Setenv("FAKEAGENT_GH_MODE", "stateful-pr")
			t.Setenv("FAKEAGENT_GH_STATE", filepath.Join(h.NMHome, "forge.json"))
			t.Setenv("FAKEAGENT_GH_LOG", filepath.Join(h.NMHome, "forge-receipts.jsonl"))
			t.Setenv("FAKEAGENT_GH_PARENT", "example/repo")
			t.Setenv("FAKEAGENT_GH_CHECKS", "success")

			var mu sync.Mutex
			var posted []string
			var head string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					State   string `json:"state"`
					Context string `json:"context"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				mu.Lock()
				expectedHead := head
				mu.Unlock()
				if r.Method != "POST" || r.URL.Path != "/repos/example/repo/statuses/"+expectedHead || payload.Context != "no-mistakes" {
					t.Errorf("unexpected status request: %s %s %+v", r.Method, r.URL.Path, payload)
				}
				// The remote has not acknowledged this request yet. The durable
				// ledger must not claim successful certification before the reply.
				if payload.State == "success" {
					database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
					if err != nil {
						t.Error(err)
					} else {
						runs, err := database.GetRunsByRepo(h.repoID())
						if err != nil || len(runs) != 1 {
							t.Errorf("pre-ACK runs: %v %v", runs, err)
						} else {
							publications, err := database.GetCommitStatusPublications(r.Context(), runs[0].ID)
							if err != nil || len(publications) != 1 || publications[0].StrictCertificate != nil || publications[0].LastState == "success" {
								t.Errorf("certificate before HTTP acknowledgement: %+v %v", publications, err)
							}
						}
						_ = database.Close()
					}
				}
				mu.Lock()
				posted = append(posted, payload.State)
				mu.Unlock()
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			t.Setenv("FAKEAGENT_GH_STATUS_URL", server.URL)
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			const branch = "feature/certified-watcher"
			committedHead := h.CommitChange(branch, "feature.txt", "certified watcher\n", "add feature")
			mu.Lock()
			head = committedHead
			mu.Unlock()
			out, err := h.Run("axi", "run", "--shipping-mode", "full", "--intent", "Validate the feature and keep its PR open", "--wait", "20s")
			if err != nil {
				t.Fatalf("strict launch: %v\n%s", err, out)
			}
			t.Logf("strict launch RESULT:\n%s", out)
			run := h.WaitForRunRunning(branch, 10*time.Second)
			database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			var publication db.CommitStatusPublication
			deadline := time.Now().Add(10 * time.Second)
			for {
				publications, err := database.GetCommitStatusPublications(t.Context(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(publications) == 1 && publications[0].StrictCertificate != nil {
					publication = publications[0]
					break
				}
				if time.Now().After(deadline) {
					logs, _ := h.Run("axi", "logs", "--step", "ci", "--full")
					t.Fatalf("normal strict execution never earned a certificate: %+v\n%s", publications, logs)
				}
				time.Sleep(50 * time.Millisecond)
			}
			stored, err := database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var certificate db.StatusCertificate
			if err := json.Unmarshal([]byte(*publication.StrictCertificate), &certificate); err != nil {
				t.Fatal(err)
			}
			epoch, err := database.StatusValidationEpoch(run.ID)
			if err != nil || stored.Status != types.RunRunning || stored.CIReadyAt == nil || stored.CIReadyNoCI || stored.PushActive || stored.Shipping == nil || stored.Shipping.Mode != types.ShippingFull || stored.LastPushedSHA == nil || *stored.LastPushedSHA != head || stored.PushGeneration == nil || certificate.SHA != head || certificate.Epoch != epoch || certificate.PushGeneration != *stored.PushGeneration || certificate.ConfigDigest == "" || certificate.ProofDigest == "" {
				t.Fatalf("certificate tuple differs from the executed publication: %+v %+v %v", stored, certificate, err)
			}
			comparison, err := database.GetReviewComparison(run.ID)
			if err != nil || comparison == nil || !comparison.Approved || comparison.HeadSHA != head || comparison.TargetBranch != "main" || comparison.SourceBranch != branch || comparison.SourceRepo != "example/repo" || comparison.TargetSHA != h.UpstreamBranchSHA("main") {
				t.Fatalf("strict comparison not approved: %+v %v", comparison, err)
			}
			assertWatcherExecutedBindings(t, h, run.ID, certificate)
			receipts, err := os.ReadFile(filepath.Join(h.NMHome, "command-receipts"))
			if err != nil || string(receipts) != "test\nlint\n" {
				t.Fatalf("production command receipts: %q %v", receipts, err)
			}
			for _, marker := range []string{"Review the code changes and return structured findings", "You are validating a code change by driving the product itself", "Keep the project documentation accurate for this change."} {
				count := 0
				for _, invocation := range h.AgentInvocations() {
					if strings.Contains(invocation.Prompt, marker) {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("production agent invocation %q count=%d, want one fresh turn", marker, count)
				}
			}
			mu.Lock()
			before := append([]string(nil), posted...)
			mu.Unlock()
			if !reflect.DeepEqual(before, []string{"pending", "success"}) {
				t.Fatalf("status HTTP receipts = %v, want pending then success", before)
			}
			if strings.HasPrefix(action, "raw-stop") {
				control, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB())
				if err != nil {
					t.Fatal(err)
				}
				defer control.Close()
				if action == "raw-stop-storage-recovery" {
					if _, err := control.Exec(`CREATE TRIGGER block_raw_fence BEFORE INSERT ON controlled_shutdown_runs BEGIN SELECT RAISE(FAIL,'temporary raw fence storage outage'); END`); err != nil {
						t.Fatal(err)
					}
					defer control.Exec(`DROP TRIGGER IF EXISTS block_raw_fence`)
				}
				pid := daemonPIDForRoot(t, h)
				process, err := os.FindProcess(pid)
				if err != nil {
					t.Fatal(err)
				}
				if err := process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
				if action == "raw-stop-storage-recovery" {
					deadline := time.Now().Add(10 * time.Second)
					for {
						log, err := os.ReadFile(paths.WithRoot(h.NMHome).DaemonLog())
						if err != nil {
							t.Fatal(err)
						}
						if strings.Contains(string(log), "temporary raw fence storage outage") {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("daemon never observed the injected storage outage")
						}
						time.Sleep(20 * time.Millisecond)
					}
					alive, err := e2edaemon.ProcessAlive(pid)
					if err != nil || !alive {
						t.Fatalf("storage failure released daemon custody: alive=%t %v", alive, err)
					}
					stored, err := database.GetRun(run.ID)
					if err != nil || stored.Status != types.RunRunning {
						t.Fatalf("storage failure fabricated terminal outcome: %+v %v", stored, err)
					}
					if _, err := control.Exec(`DROP TRIGGER block_raw_fence`); err != nil {
						t.Fatal(err)
					}
					t.Log("RESULT: failed raw-fence persistence retained live daemon and nonterminal run; restored storage without a second shutdown request")
				}
				deadline := time.Now().Add(15 * time.Second)
				for {
					alive, err := e2edaemon.ProcessAlive(pid)
					if err != nil {
						t.Fatal(err)
					}
					if !alive {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("raw shutdown did not release the captured process")
					}
					time.Sleep(20 * time.Millisecond)
				}
				assertNoDaemonProcessesForRoot(t, h, "after raw signal")
				for _, stage := range []string{"before recovery", "after recovery"} {
					expectedStatus := types.RunRunning
					expectedState := "success"
					if stage == "after recovery" {
						if out, err := h.Run("daemon", "start"); err != nil {
							t.Fatalf("raw recovery: %v\n%s", err, out)
						}
						expectedStatus = types.RunCIMonitorInterrupted
						expectedState = "failure"
					}
					stored, err := database.GetRun(run.ID)
					if err != nil || stored.Status != expectedStatus || (stage == "after recovery" && (stored.Error == nil || *stored.Error != types.RunCIMonitorInterruptedReason)) {
						t.Fatalf("%s raw outcome: %+v %v", stage, stored, err)
					}
					publications, err := database.GetCommitStatusPublications(t.Context(), run.ID)
					deadline := time.Now().Add(10 * time.Second)
					for err == nil && len(publications) == 1 && publications[0].LastState != expectedState && time.Now().Before(deadline) {
						time.Sleep(50 * time.Millisecond)
						publications, err = database.GetCommitStatusPublications(t.Context(), run.ID)
					}
					if err != nil || len(publications) != 1 || publications[0].LastState != expectedState || publications[0].StrictCertificate != nil || (publications[0].PreserveSuccess != nil && *publications[0].PreserveSuccess) {
						t.Fatalf("%s raw publication: %+v %v", stage, publications, err)
					}
				}
				mu.Lock()
				receipts := append([]string(nil), posted...)
				mu.Unlock()
				if !reflect.DeepEqual(receipts, []string{"pending", "success", "failure"}) {
					t.Fatalf("raw shutdown HTTP receipts: %v", receipts)
				}
				t.Logf("RESULT: raw signal revoked certification and left monitor for recovery; restart recorded %s and HTTP states %v", types.RunCIMonitorInterrupted, receipts)
				return
			}
			events, closeEvents, err := ipc.Subscribe(paths.WithRoot(h.NMHome).Socket(), &ipc.SubscribeParams{RunID: run.ID})
			if err != nil {
				t.Fatal(err)
			}
			defer closeEvents()
			reason := types.RunCancelReasonAbortedByUser
			if action == "planned-stop" {
				reason = "cancelled: planned daemon shutdown"
				stopCertifiedWatcherDaemon(t, h, run.ID, reason)
			} else if action == "uninstall" {
				reason = "cancelled: planned daemon shutdown"
				root, err := filepath.EvalSymlinks(h.NMHome)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256([]byte(root))
				definition := filepath.Join(h.HomeDir, "Library", "LaunchAgents", "com.kunchenguid.no-mistakes.daemon."+hex.EncodeToString(digest[:4])+".plist")
				if err := os.MkdirAll(filepath.Dir(definition), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(definition, []byte("isolated service fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				// Only the service-manager boundary is simulated. Its bootout
				// sends a real raw signal to the actual fixture daemon, so an
				// uninstall that bootouts before its planned handoff loses proof.
				script := "#!/bin/sh\n[ \"$1\" = bootout ] || exit 70\nprintf '%s\\n' \"$*\" >> \"$NM_HOME/bootout-receipt\"\nkill -TERM " + strconv.Itoa(daemonPIDForRoot(t, h)) + " 2>/dev/null || true\n"
				if err := os.WriteFile(filepath.Join(h.BinDir, "launchctl"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				out, err := h.RunInDirWithEnv(h.WorkDir, map[string]string{"NM_TEST_START_DAEMON": "0"}, "daemon", "uninstall", "--force")
				if err != nil {
					t.Fatalf("uninstall: %v\n%s", err, out)
				}
				if _, err := os.Stat(definition); !os.IsNotExist(err) {
					t.Fatalf("uninstall retained service definition: %v", err)
				}
				if _, err := os.Stat(filepath.Join(h.NMHome, "bootout-receipt")); err != nil {
					t.Fatalf("uninstall bypassed service manager: %v", err)
				}
				t.Logf("uninstall CLI RESULT (isolated launchctl fixture): %s", strings.TrimSpace(out))
			} else if out, err := h.Run("axi", "abort"); err != nil {
				t.Fatalf("abort: %v\n%s", err, out)
			} else {
				t.Logf("terminal command EXIT=0 RESULT: %s", strings.TrimSpace(out))
			}
			select {
			case <-time.After(5 * time.Second):
				t.Fatal("no truthful terminal event")
			case event, ok := <-events:
				for ok && event.Type != ipc.EventRunCompleted {
					select {
					case event, ok = <-events:
					case <-time.After(5 * time.Second):
						t.Fatal("no terminal event")
					}
				}
				if !ok || event.Status == nil || *event.Status != string(types.RunCancelled) || event.Error == nil || *event.Error != reason || event.StateRev == 0 {
					t.Fatalf("terminal event: %+v (open=%t), want cancelled %q", event, ok, reason)
				}
			}
			// Opening a new SQLite handle and restarting the real daemon exercise
			// the durable decision, rather than an in-memory cancellation flag.
			assertPreservedWatcher(t, h, run.ID, head, reason, publication)
			if out, err := h.Run("daemon", "restart"); err != nil {
				t.Fatalf("restart/recovery: %v\n%s", err, out)
			}
			assertPreservedWatcher(t, h, run.ID, head, reason, publication)
			// Delayed reporter callbacks consume the real production reporter
			// against the certificate earned above; they cannot recreate proof.
			callbackDB, err := db.Open(paths.WithRoot(h.NMHome).DB())
			if err != nil {
				t.Fatal(err)
			}
			defer callbackDB.Close()
			durable, err := callbackDB.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			repo, err := database.GetRepo(h.repoID())
			if err != nil {
				t.Fatal(err)
			}
			globalConfig, err := config.LoadGlobal(filepath.Join(h.NMHome, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			trustedConfig, err := config.LoadRepo(h.WorkDir)
			if err != nil {
				t.Fatal(err)
			}
			sctx := &pipeline.StepContext{Ctx: context.Background(), DB: callbackDB, Run: durable, Repo: repo, Config: config.Merge(globalConfig, trustedConfig), WorkDir: h.WorkDir, Log: func(line string) { t.Error(line) }}
			for _, state := range []string{"pending", "success", "failure"} {
				(&steps.PushStep{}).ReportPublishedHeadStatus(sctx, state, head)
			}
			if out, err := h.Run("daemon", "stop"); err != nil {
				t.Fatalf("final cleanup: %v\n%s", err, out)
			}
			assertNoDaemonProcessesForRoot(t, h, "after cleanup")
			assertPreservedWatcher(t, h, run.ID, head, reason, publication)
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(posted, before) {
				t.Fatalf("cancellation/recovery/late callbacks posted new status: %v -> %v", before, posted)
			}
			t.Log("RESULT: production strict gates, HTTP issuance, cancellation, late callbacks and restarted recovery preserved SUCCESS")
		})
	}
}

func certifiedWatcherScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "certified-watcher.yaml")
	// A deterministic backend response allows the production steps to run;
	// it makes no claim to have performed live validation of this fixture.
	content := `actions:
  - text: "deterministic external agent response"
    structured:
      findings: []
      summary: "deterministic fixture"
      risk_level: low
      risk_rationale: "fixture response"
      risk_scope: source-or-external
      tested: ["configured command receipt"]
      testing_summary: "deterministic integration boundary"
      artifacts: []
      scenarios:
        - name: "live model and forge journey"
          result: untested
          live: false
          evidence: ""
          reason: "external agent and forge are deterministic fixtures"
      verdict: go
      title: "feat: watcher fixture"
      body: "Deterministic watcher fixture"
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Use the supported shutdown wire contract with the fixture daemon's exact
// captured identity. This exercises the same production drain used by Stop
// and managed refresh without a CLI force flag or a synthetic shutdown cause.
func stopCertifiedWatcherDaemon(t *testing.T, h *Harness, runID, reason string) {
	t.Helper()
	p := paths.WithRoot(h.NMHome)
	data, err := os.ReadFile(p.PIDFile())
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		Instance  string    `json:"instance"`
		PID       int       `json:"pid"`
		StartedAt time.Time `json:"started_at"`
	}
	if err := json.Unmarshal(data, &identity); err != nil || identity.PID <= 0 || identity.Instance == "" || identity.StartedAt.IsZero() {
		t.Fatalf("captured daemon identity: %+v %v", identity, err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	request := ipc.ShutdownParams{ExpectedPID: identity.PID, ExpectedInstance: identity.Instance, ExpectedStartedAt: identity.StartedAt.UTC().Format(time.RFC3339Nano)}
	for _, field := range []string{"pid", "instance", "started_at"} {
		wrong := request
		switch field {
		case "pid":
			wrong.ExpectedPID++
		case "instance":
			wrong.ExpectedInstance += "-wrong"
		case "started_at":
			wrong.ExpectedStartedAt = identity.StartedAt.Add(-500 * time.Millisecond).UTC().Format(time.RFC3339Nano)
		}
		var refused ipc.ShutdownResult
		if err := client.CallWithTimeout(ipc.MethodShutdown, &wrong, &refused, 5*time.Second); err == nil || refused.OK {
			t.Fatalf("shutdown accepted mismatched %s: %+v %v", field, refused, err)
		}
		var health ipc.HealthResult
		if err := client.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, 5*time.Second); err != nil || health.Status != "ok" {
			t.Fatalf("mismatched %s shut down the live daemon: %+v %v", field, health, err)
		}
	}
	t.Log("shutdown IPC RESULT: mismatched PID, instance and exact timestamp refused; daemon remained healthy")
	var ack ipc.ShutdownResult
	err = client.CallWithTimeout(ipc.MethodShutdown, &request, &ack, 35*time.Second)
	_ = client.Close() // The server drains this connection before exiting.
	if err != nil || !ack.OK || ack.PID != request.ExpectedPID || ack.Instance != request.ExpectedInstance || ack.StartedAt != request.ExpectedStartedAt || ack.Lifecycle == "" {
		t.Fatalf("matching shutdown acknowledgement: %+v %v", ack, err)
	}
	database, err := db.OpenReadOnly(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	matches, err := database.ControlledShutdownMatches(runID, ack.Instance, ack.Lifecycle, reason)
	_ = database.Close()
	if err != nil || !matches {
		t.Fatalf("acknowledgement lacks durable lifecycle binding: %t %v", matches, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		alive, err := e2edaemon.ProcessAlive(identity.PID)
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("acknowledged daemon did not exit after socket close")
		}
		time.Sleep(20 * time.Millisecond)
	}
	assertNoDaemonProcessesForRoot(t, h, "after acknowledged shutdown")
	t.Log("shutdown IPC RESULT: matching durable acknowledgement, closed client, captured process exited")
}

func assertWatcherExecutedBindings(t *testing.T, h *Harness, runID string, certificate db.StatusCertificate) {
	t.Helper()
	control, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	rows, err := control.Query(`SELECT s.step_name,s.status,s.exit_code,s.override_reason,s.approval_reason,v.head_sha,v.epoch,v.config_digest
	 FROM step_results s LEFT JOIN status_validation_steps v ON v.step_id=s.id
	 WHERE s.run_id=? AND s.step_name IN ('review','test','document','lint')`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var name, status, head, digest string
		var exit, epoch int64
		var override, approval *string
		if err := rows.Scan(&name, &status, &exit, &override, &approval, &head, &epoch, &digest); err != nil {
			t.Fatal(err)
		}
		if seen[name] || status != "completed" || exit != 0 || override != nil || approval != nil || head != certificate.SHA || epoch != certificate.Epoch || digest != certificate.ConfigDigest {
			t.Fatalf("non-executed/overridden/stale binding: %s %s %d %v %v %s %d %s", name, status, exit, override, approval, head, epoch, digest)
		}
		seen[name] = true
	}
	if err := rows.Err(); err != nil || len(seen) != 4 {
		t.Fatalf("fresh production gate bindings = %v %v", seen, err)
	}
}

func assertPreservedWatcher(t *testing.T, h *Harness, runID, head, reason string, original db.CommitStatusPublication) {
	t.Helper()
	database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run, err := database.GetRun(runID)
	if err != nil || run == nil || run.Status != types.RunCancelled || run.Error == nil || *run.Error != reason || run.HeadSHA != head || run.LastPushedSHA == nil || *run.LastPushedSHA != head || run.PushActive || run.TerminalHeadVerifiedAt == nil {
		t.Fatalf("terminal outcome: %+v %v", run, err)
	}
	publications, err := database.GetCommitStatusPublications(t.Context(), runID)
	if err != nil || len(publications) != 1 {
		t.Fatalf("preserved publication: %+v %v", publications, err)
	}
	p := publications[0]
	if p.PreserveSuccess == nil || !*p.PreserveSuccess || p.StrictCertificate == nil || *p.StrictCertificate != *original.StrictCertificate || p.LastState != "success" || p.SHA != head || p.Host != original.Host || p.Repo != original.Repo {
		t.Fatalf("durable preservation differs: %+v", p)
	}
}
