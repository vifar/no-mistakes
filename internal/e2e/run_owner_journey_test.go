//go:build e2e

package e2e

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Claims cross the real daemon IPC boundary. Seeded SQL failures exercise
// cancellation settlement; the oracle is one created receipt and no consumption
// for failed admission or mismatched proof. Deterministic internal race windows
// are covered separately by TestNotificationReceiptClaimSerializesAdmission.
func TestRunOwnerReceiptJourney(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprintf("setup_failure=%t", failure), func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t), GlobalConfigExtra: "daemon:\n  max_concurrent_runs: 1\n  hold_slot_while_parked: true\n"})
			statusCommand(t, h, h.WorkDir, "init")
			h.CommitChange("feature/holder", "holder.txt", "hold slot\n", "holder")
			h.PushToGate("feature/holder")
			holder := waitForStepStatus(t, h, "feature/holder", types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
			branch := "feature/receipt"
			h.CommitChange(branch, "feature.txt", "predecessor\n", "predecessor")
			h.PushToGate(branch)
			prior := h.ActiveRun(branch)
			if prior == nil || prior.Status != types.RunQueued {
				t.Fatalf("missing queued predecessor: %+v", prior)
			}
			writer, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB())
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if failure {
				if _, err := writer.Exec("CREATE TRIGGER receipt_cancel_fault BEFORE UPDATE OF status ON runs WHEN OLD.id='" + prior.ID + "' AND NEW.status='cancelled' BEGIN SELECT RAISE(ABORT,'receipt cancellation fault'); END"); err != nil {
					t.Fatal(err)
				}
			}
			head := h.CommitChange(branch, "feature.txt", "replacement\n", "replacement")
			claim := ipc.ClaimLaunchReceiptParams{RepoID: h.repoID(), Branch: branch, Owner: "receipt-owner", LaunchNonce: "receipt-nonce", SubmittedHeadSHA: head, ValidationGeneration: "receipt-generation", IntentDigest: fmt.Sprintf("%x", sha256.Sum256([]byte("receipt intent")))}
			call := func(p ipc.ClaimLaunchReceiptParams) (ipc.ClaimLaunchReceiptResult, error) {
				c, err := ipc.Dial(paths.WithRoot(h.NMHome).Socket())
				if err != nil {
					return ipc.ClaimLaunchReceiptResult{}, err
				}
				defer c.Close()
				var result ipc.ClaimLaunchReceiptResult
				err = c.CallWithContext(t.Context(), ipc.MethodClaimLaunchReceipt, &p, &result, 5*time.Second)
				return result, err
			}
			if result, err := call(claim); err != nil || result.Receipt != nil {
				t.Fatalf("absent receipt: %+v %v", result, err)
			}
			encode := func(key, value string) string {
				return "no-mistakes." + key + "=" + base64.StdEncoding.EncodeToString([]byte(value))
			}
			args := []string{"push", "no-mistakes", branch}
			for _, option := range []string{encode("owner", claim.Owner), encode("launch-nonce", claim.LaunchNonce), encode("validation-generation", claim.ValidationGeneration), encode("intent", "receipt intent")} {
				args = append(args, "--push-option", option)
			}
			type outcome struct {
				result ipc.ClaimLaunchReceiptResult
				err    error
			}
			// Overlap real Git/hook admission with mismatched observers. None
			// may consume created or expose a healthy transient as an orphan.
			start := make(chan struct{})
			racing := make(chan outcome, 4)
			for i := 0; i < 4; i++ {
				go func() {
					<-start
					mismatch := claim
					mismatch.Owner = "other"
					result, err := call(mismatch)
					racing <- outcome{result, err}
				}()
			}
			close(start)
			if out, err := h.runGit(t.Context(), h.WorkDir, args...); err != nil {
				t.Fatalf("receive: %v\n%s", err, out)
			}
			for i := 0; i < 4; i++ {
				got := <-racing
				if got.result.Receipt != nil || (got.err != nil && strings.Contains(got.err.Error(), "orphan")) {
					t.Fatalf("concurrent observer exposed transient admission: %+v %v", got.result, got.err)
				}
			}
			var id string
			if err := writer.QueryRow("SELECT id FROM runs WHERE launch_nonce=?", claim.LaunchNonce).Scan(&id); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"owner", "head", "nonce", "generation", "intent", "base", "omit", "external"} {
				mismatch := claim
				switch field {
				case "owner":
					mismatch.Owner = "other"
				case "head":
					mismatch.SubmittedHeadSHA = strings.Repeat("f", 40)
				case "nonce":
					mismatch.LaunchNonce = "other"
				case "generation":
					mismatch.ValidationGeneration = "other"
				case "intent":
					mismatch.IntentDigest = "other"
				case "base":
					mismatch.PRBaseBranch = "other"
				case "omit":
					mismatch.OmitIntent = true
				case "external":
					mismatch.ExternalCIOwner = "controller-ship-pr"
				}
				if result, _ := call(mismatch); result.Receipt != nil {
					t.Fatalf("mismatch %s accepted: %+v", field, result)
				}
			}
			var claimed sql.NullInt64
			if err := writer.QueryRow("SELECT launch_receipt_claimed_at FROM runs WHERE id=?", id).Scan(&claimed); err != nil || claimed.Valid {
				t.Fatalf("mismatched requests consumed created: %+v %v", claimed, err)
			}
			outcomes := make(chan outcome, 4)
			for i := 0; i < 4; i++ {
				go func() {
					result, err := call(claim)
					outcomes <- outcome{result, err}
				}()
			}
			created := 0
			for i := 0; i < 4; i++ {
				select {
				case got := <-outcomes:
					if failure {
						if got.err == nil || !strings.Contains(got.err.Error(), "supersede predecessor") || got.result.Receipt != nil {
							t.Errorf("failure hidden: %+v %v", got.result, got.err)
						}
						continue
					}
					proof := got.result.Receipt
					if got.err != nil || proof == nil || proof.RunID != id || proof.Owner != claim.Owner || proof.SubmittedHeadSHA != head || proof.IntentDigest != claim.IntentDigest || proof.LaunchNonce != claim.LaunchNonce || proof.ValidationGeneration != claim.ValidationGeneration {
						t.Errorf("invalid receipt: %+v %v", proof, got.err)
						continue
					}
					if proof.Disposition == "created" {
						created++
					} else if proof.Disposition != "reused" {
						t.Errorf("invalid disposition: %+v", proof)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("concurrent claims did not settle")
				}
			}
			if !failure && created != 1 {
				t.Fatalf("created count=%d, want one", created)
			}
			if failure {
				if got := h.RunInfo(id); got.Status != types.RunFailed {
					t.Fatalf("failed setup did not settle: %+v", got)
				}
				if err := writer.QueryRow("SELECT launch_receipt_claimed_at FROM runs WHERE id=?", id).Scan(&claimed); err != nil || claimed.Valid {
					t.Fatalf("failed setup consumed receipt: %+v %v", claimed, err)
				}
				if _, err := writer.Exec("DROP TRIGGER receipt_cancel_fault"); err != nil {
					t.Fatal(err)
				}
				h.CancelRun(prior.ID)
			} else {
				h.CancelRun(id)
			}
			t.Logf("run=%s setup_failure=%t created=%d mismatches unconsumed", id, failure, created)
			h.CancelRun(holder.ID)
		})
	}
}

// Responsibility is caller supplied: a refused launch changes no gate ref,
// reattachment retains responsibility, and rerun takes its caller's owner.
// Real CLI/daemon/Git execute here; only the agent and forge are fixtures.
func TestRunOwnerLaunchJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t), GlobalConfigExtra: "intent:\n  require_owner: true\n"})
	t.Setenv("NO_MISTAKES_OWNER", "")
	statusCommand(t, h, h.WorkDir, "init")
	branch := "feature/owner"
	head := h.CommitChange(branch, "feature.txt", "owner lifecycle\n", "owner fixture")
	gate := paths.WithRoot(h.NMHome).RepoDir(h.repoID())
	for _, args := range [][]string{{"axi", "run", "--intent", "exercise ownership"}, {"axi", "run", "--intent", "exercise ownership", "--owner", "bad\nowner"}} {
		out, err := h.Run(args...)
		if err == nil {
			t.Fatalf("invalid launch accepted: %s", out)
		}
		if runs := h.Runs(); len(runs) != 0 {
			t.Fatalf("refused launch created runs: %+v", runs)
		}
		if out, err := h.runGit(t.Context(), gate, "show-ref", "--verify", "refs/heads/"+branch); err == nil {
			t.Fatalf("refused launch mutated mirror: %s", out)
		}
		t.Logf("refused before mirror mutation: %s", out)
	}
	out, err := h.RunInDirWithEnv(h.WorkDir, map[string]string{"NO_MISTAKES_OWNER": " environment-owner "}, "axi", "run", "--intent", "exercise ownership", "--owner", " flag-owner ")
	if err != nil {
		t.Fatalf("immediate owned retry: %v\n%s", err, out)
	}
	t.Logf("owned retry: %s", out)
	run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if run.Owner != "flag-owner" || run.SubmittedHeadSHA == nil || *run.SubmittedHeadSHA != head {
		t.Fatalf("flag precedence/trim or head lost: %+v", run)
	}
	statusCommand(t, h, h.WorkDir, "axi", "run", "--owner", "observer-owner")
	if got := h.ActiveRun(branch); got == nil || got.ID != run.ID || got.Owner != "flag-owner" {
		t.Fatalf("reattach changed responsibility: %+v", got)
	}
	h.CancelRun(run.ID)
	out, err = h.RunInDirWithEnv(h.WorkDir, map[string]string{"NO_MISTAKES_OWNER": " rerun-owner "}, "rerun")
	if err != nil {
		t.Fatalf("environment-owned rerun: %v\n%s", err, out)
	}
	t.Logf("rerun: %s", out)
	rerun := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if rerun.ID == run.ID || rerun.Owner != "rerun-owner" {
		t.Fatalf("rerun did not take caller responsibility: %+v", rerun)
	}
	h.CancelRun(rerun.ID)
}

// Fault injection seeds failed INSERTs, not successful admissions or validation
// markers. The real receive hook and daemon must produce the marked B-C-D path,
// retain the queued predecessor, and replay D-B under its original receive ID.
func TestRunOwnerRetainedMultihopJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t), GlobalConfigExtra: "intent:\n  require_owner: true\ndaemon:\n  max_concurrent_runs: 1\n  hold_slot_while_parked: true\n"})
	statusCommand(t, h, h.WorkDir, "init")
	h.CommitChange("feature/holder", "holder.txt", "hold slot\n", "holder")
	h.PushToGate("feature/holder")
	holder := waitForStepStatus(t, h, "feature/holder", types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	branch := "feature/multihop"
	headB := h.CommitChange(branch, "feature.txt", "B\n", "B")
	h.PushToGate(branch)
	prior := h.ActiveRun(branch)
	if prior == nil || prior.Status != types.RunQueued {
		t.Fatalf("predecessor is not queued: %+v", prior)
	}
	writer, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec("CREATE TRIGGER multihop_fault BEFORE INSERT ON runs BEGIN SELECT RAISE(ABORT,'multihop insert fault'); END"); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"C", "D"} {
		h.CommitChange(branch, "feature.txt", label+"\n", label)
		h.PushToGate(branch)
		if got := h.ActiveRun(branch); got == nil || got.ID != prior.ID || got.Status != types.RunQueued {
			t.Fatalf("failed admission lost predecessor: %+v", got)
		}
	}
	if out, err := h.runGit(t.Context(), h.WorkDir, "push", "--force", "no-mistakes", headB+":refs/heads/"+branch); err != nil {
		t.Fatalf("return ref: %v\n%s", err, out)
	}
	list := func() []ipc.PushNotificationInfo {
		t.Helper()
		out := statusCommand(t, h, h.WorkDir, "daemon", "notifications", "list")
		var records []ipc.PushNotificationInfo
		if err := json.Unmarshal([]byte(out), &records); err != nil {
			t.Fatalf("notifications: %v\n%s", err, out)
		}
		return records
	}
	id := ""
	for _, info := range list() {
		if !info.Pending {
			continue
		}
		out := statusCommand(t, h, h.WorkDir, "daemon", "notifications", "inspect", info.ID)
		var detail ipc.PushNotificationInfo
		if err := json.Unmarshal([]byte(out), &detail); err != nil {
			t.Fatal(err)
		}
		if detail.Params != nil && detail.Params.New == headB {
			id = info.ID
		}
		t.Logf("retained failed receive: %s", out)
	}
	if id == "" {
		t.Fatal("failed returning receive was not retained")
	}
	beforeReplay := list()
	// Restart only the harness-owned daemon. Recovery must use the durable
	// original journal while the INSERT fault still prevents admission.
	gateJourneyDaemon(t, h, "stop")
	gateJourneyDaemon(t, h, "start")
	if got := h.ActiveRun(branch); got == nil || got.ID != prior.ID || got.Status != types.RunQueued {
		t.Fatalf("recovery lost predecessor: %+v", got)
	}
	if _, err := writer.Exec("DROP TRIGGER multihop_fault"); err != nil {
		t.Fatal(err)
	}
	out := statusCommand(t, h, h.WorkDir, "daemon", "notifications", "replay", id)
	t.Logf("original-ID replay: %s", out)
	newRun := h.ActiveRun(branch)
	if newRun == nil || newRun.ID == prior.ID || newRun.Status != types.RunQueued || h.RunInfo(prior.ID).Status != types.RunCancelled {
		t.Fatalf("multihop failed to create replacement: %+v", newRun)
	}
	count := len(h.Runs())
	for i := 0; i < 2; i++ {
		statusCommand(t, h, h.WorkDir, "daemon", "notifications", "replay", id)
	}
	if len(h.Runs()) != count || h.ActiveRun(branch).ID != newRun.ID {
		t.Fatal("original-ID replay duplicated admission")
	}
	afterReplay := list()
	if len(afterReplay) != len(beforeReplay) {
		t.Fatal("recovery/replay minted another receive identity")
	}
	for i := range beforeReplay {
		if beforeReplay[i].ID != afterReplay[i].ID {
			t.Fatal("recovery/replay changed receive identities")
		}
	}
	database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	event, err := database.GetOwnerAdmission(newRun.ID)
	if err != nil || event == nil || event.RunID != newRun.ID {
		t.Fatalf("replacement lacks durable audit: %+v %v", event, err)
	}
	t.Logf("replay=%s new_run=%s prior=%s runs=%d audit=%+v", id, newRun.ID, prior.ID, count, event)
	h.CancelRun(newRun.ID)
	h.CancelRun(holder.ID)
}

// The receive has already changed Git. Even malformed ownership must admit the
// ref and durably audit bypass; owned, deletion and policy-off controls do not.
func TestRunOwnerReceiveJourney(t *testing.T) {
	for _, policy := range []bool{true, false} {
		name := "required"
		if !policy {
			name = "optional"
		}
		t.Run(name, func(t *testing.T) {
			var extra string
			if policy {
				extra = "intent:\n  require_owner: true\n"
			} else {
				extra = "intent:\n  require_owner: false\n"
			}
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t), GlobalConfigExtra: extra})
			// Suppress warning logs while keeping the durable warning observable.
			config := filepath.Join(h.NMHome, "config.yaml")
			raw, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config, []byte(strings.Replace(string(raw), "log_level: debug", "log_level: error", 1)), 0600); err != nil {
				t.Fatal(err)
			}
			statusCommand(t, h, h.WorkDir, "init")
			encode := func(owner string) string {
				return "no-mistakes.owner=" + base64.StdEncoding.EncodeToString([]byte(owner))
			}
			for _, tc := range []struct {
				name    string
				options []string
				owner   string
				invalid bool
			}{
				{"ownerless", nil, "", false},
				{"malformed", []string{"no-mistakes.owner=%%%"}, "", true},
				{"duplicate", []string{encode("A"), encode("B")}, "", true},
				{"invalid", []string{encode("bad\nowner")}, "", true},
				{"owned", []string{encode("receive-owner")}, "receive-owner", false},
			} {
				branch := "feature/" + tc.name
				head := h.CommitChange(branch, "feature.txt", tc.name+"\n", tc.name)
				args := []string{"push", "no-mistakes", branch}
				for _, option := range tc.options {
					args = append(args, "--push-option", option)
				}
				if out, err := h.runGit(t.Context(), h.WorkDir, args...); err != nil {
					t.Fatalf("receive %s: %v\n%s", tc.name, err, out)
				}
				run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
				if run.Owner != tc.owner || run.SubmittedHeadSHA == nil || *run.SubmittedHeadSHA != head {
					t.Fatalf("receive stranded or misowned: %+v", run)
				}
				database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
				if err != nil {
					t.Fatal(err)
				}
				event, err := database.GetOwnerAdmission(run.ID)
				database.Close()
				if err != nil {
					t.Fatal(err)
				}
				if tc.owner == "" && (policy || tc.invalid) {
					if event == nil || event.Event != "launch_owner_policy_bypassed" || event.RunID != run.ID || event.Branch != branch || !event.OwnerlessAdmitted || event.Warning == "" || (event.OwnerValidationError != "") != tc.invalid {
						t.Fatalf("incomplete durable audit: %+v", event)
					}
				} else if event != nil {
					t.Fatalf("unexpected bypass audit: %+v", event)
				}
				t.Logf("receive=%s policy=%t run=%s owner=%q audit=%+v", tc.name, policy, run.ID, run.Owner, event)
				h.CancelRun(run.ID)
				before := len(h.Runs())
				if out, err := h.runGit(t.Context(), h.WorkDir, "push", "no-mistakes", ":refs/heads/"+branch); err != nil {
					t.Fatalf("delete: %v\n%s", err, out)
				}
				if len(h.Runs()) != before {
					t.Fatal("deletion created a run/audit")
				}
			}
		})
	}
}
