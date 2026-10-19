//go:build e2e

package e2e

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The receipt contract requires rejected claims to leave created available.
// A SQLite trigger makes the persisted run fail at the claim write, after the
// daemon's admission lookup. This deterministic fault rejects the old behavior
// that committed the marker before checking eligibility. CLI, Git, hook, daemon,
// IPC and SQLite are real; only the agent and forge use the repository fixtures.
func TestRunOwnerReceiptFailureBeforeCommitJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t), GlobalConfigExtra: "daemon:\n  max_concurrent_runs: 1\n  hold_slot_while_parked: true\n"})
	statusCommand(t, h, h.WorkDir, "init")
	h.CommitChange("feature/holder", "holder.txt", "hold slot\n", "holder")
	h.PushToGate("feature/holder")
	holder := waitForStepStatus(t, h, "feature/holder", types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	writer, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	for _, path := range []string{"observer", "existing-fresh", "new-fresh"} {
		t.Run(path, func(t *testing.T) {
			branch := "feature/" + path
			nonce := "receipt-commit-" + path
			head := h.CommitChange(branch, "receipt.txt", path+"\n", path)
			owner, intent, generation := "receipt-owner", "receipt commit contract", "receipt-generation"
			args := []string{"push", "no-mistakes", branch}
			if path != "new-fresh" {
				for _, option := range []struct{ key, value string }{{"owner", owner}, {"intent", intent}, {"launch-nonce", nonce}, {"validation-generation", generation}} {
					args = append(args, "--push-option", "no-mistakes."+option.key+"="+base64.StdEncoding.EncodeToString([]byte(option.value)))
				}
			}
			if out, err := h.runGit(t.Context(), h.WorkDir, args...); err != nil {
				t.Fatalf("receive: %v\n%s", err, out)
			}
			prior := h.ActiveRun(branch)
			if prior == nil || prior.Status != types.RunQueued {
				t.Fatalf("missing queued receive: %+v", prior)
			}
			if _, err := writer.Exec(`CREATE TRIGGER receipt_commit_fault BEFORE UPDATE OF launch_receipt_claimed_at ON runs
				WHEN NEW.launch_nonce = '` + nonce + `' AND NEW.launch_receipt_claimed_at IS NOT NULL
				BEGIN UPDATE runs SET status='failed', error='failure before receipt commit' WHERE id=OLD.id; END`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := writer.Exec("DROP TRIGGER IF EXISTS receipt_commit_fault"); err != nil {
					t.Error(err)
				}
			})
			c, err := ipc.Dial(paths.WithRoot(h.NMHome).Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			call := func() (*ipc.LaunchReceipt, error) {
				if path == "observer" {
					var result ipc.ClaimLaunchReceiptResult
					err := c.CallWithContext(t.Context(), ipc.MethodClaimLaunchReceipt, &ipc.ClaimLaunchReceiptParams{RepoID: h.repoID(), Branch: branch, Owner: owner, LaunchNonce: nonce, SubmittedHeadSHA: head, ValidationGeneration: generation, IntentDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(intent)))}, &result, 5*time.Second)
					return result.Receipt, err
				}
				var result ipc.StartFreshRunResult
				err := c.CallWithContext(t.Context(), ipc.MethodStartFreshRun, &ipc.StartFreshRunParams{RepoID: h.repoID(), Branch: branch, HeadSHA: head, Owner: owner, Intent: intent, LaunchNonce: nonce, ValidationGeneration: generation}, &result, 5*time.Second)
				if err != nil {
					return nil, err
				}
				return &result.Receipt, nil
			}
			receipt, err := call()
			t.Logf("%s rejected receipt=%+v error=%v", path, receipt, err)
			if err == nil || !strings.Contains(err.Error(), "failure before receipt commit") || receipt != nil {
				t.Fatalf("failure did not reject claim: %+v %v", receipt, err)
			}
			var id, status string
			var claimed sql.NullInt64
			if err := writer.QueryRow("SELECT id, status, launch_receipt_claimed_at FROM runs WHERE launch_nonce=?", nonce).Scan(&id, &status, &claimed); err != nil {
				t.Fatal(err)
			}
			t.Logf("after rejection run=%s status=%s launch_receipt_claimed_at=%+v", id, status, claimed)
			if claimed.Valid || status != string(types.RunQueued) {
				t.Fatal("rejected claim did not roll back marker and injected failure")
			}
			if path != "new-fresh" && id != prior.ID || path == "new-fresh" && (id == prior.ID || h.RunInfo(prior.ID).Status != types.RunCancelled) {
				t.Fatal("receipt claim changed predecessor or run identity")
			}
			if _, err := writer.Exec("DROP TRIGGER receipt_commit_fault"); err != nil {
				t.Fatal(err)
			}
			for _, disposition := range []string{"created", "reused"} {
				receipt, err := call()
				t.Logf("%s retry receipt=%+v error=%v", path, receipt, err)
				if err != nil || receipt == nil || receipt.RunID != id || receipt.Owner != owner || receipt.SubmittedHeadSHA != head || receipt.Disposition != disposition {
					t.Fatalf("retry lost created or identity: %+v %v", receipt, err)
				}
			}
			h.CancelRun(id)
		})
	}
	h.CancelRun(holder.ID)
}
