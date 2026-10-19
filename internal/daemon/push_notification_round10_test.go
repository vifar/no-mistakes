package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type receiptRaceResult struct {
	result ipc.ClaimLaunchReceiptResult
	err    error
}

func TestNotificationReceiptClaimSerializesAdmission(t *testing.T) {
	for _, timing := range []string{"lookup_before_insert", "lookup_after_insert"} {
		for _, failure := range []bool{false, true} {
			name := timing + "/healthy"
			if failure {
				name = timing + "/setup_failure"
			}
			t.Run(name, func(t *testing.T) {
				f := newPushRecoveryFixture(t, true)
				f.params.LaunchNonce = "receipt-race"
				f.params.ValidationGeneration = "race-generation"
				f.params.Owner = "Firstmate"
				if failure {
					writer, err := sql.Open("sqlite", f.p.DB())
					if err != nil {
						t.Fatal(err)
					}
					defer writer.Close()
					if _, err := writer.Exec("CREATE TRIGGER receipt_cancel_fault BEFORE UPDATE OF status ON runs WHEN OLD.id='" + f.predecessor + "' AND NEW.status='cancelled' BEGIN SELECT RAISE(ABORT,'receipt cancellation fault'); END"); err != nil {
						t.Fatal(err)
					}
				}
				lookup := make(chan *db.Run, 1)
				releaseLookup := make(chan struct{})
				cancel := make(chan struct{})
				releaseCancel := make(chan struct{})
				var lookupOnce, cancelOnce, releaseLookupOnce, releaseCancelOnce sync.Once
				unblockLookup := func() { releaseLookupOnce.Do(func() { close(releaseLookup) }) }
				defer unblockLookup()
				unblockCancel := func() { releaseCancelOnce.Do(func() { close(releaseCancel) }) }
				defer unblockCancel()
				f.m.afterReceiptLookup = func(run *db.Run) { lookupOnce.Do(func() { lookup <- run; <-releaseLookup }) }
				f.m.afterSupersedeSnapshot = func() { cancelOnce.Do(func() { close(cancel); <-releaseCancel }) }
				claim := ipc.ClaimLaunchReceiptParams{RepoID: "recovery", Branch: "fresh", LaunchNonce: f.params.LaunchNonce, SubmittedHeadSHA: f.params.New, ValidationGeneration: f.params.ValidationGeneration, IntentDigest: digestIntent(f.params.Intent), Owner: f.params.Owner}
				c := notificationIPCClient(t, f)
				claimDone := make(chan receiptRaceResult, 1)
				launchDone := make(chan notificationResult, 1)
				startClaim := func() {
					go func() {
						var result ipc.ClaimLaunchReceiptResult
						err := c.Call(ipc.MethodClaimLaunchReceipt, &claim, &result)
						claimDone <- receiptRaceResult{result, err}
					}()
				}
				startLaunch := func() {
					go func() {
						id, err := f.m.HandlePushReceived(context.Background(), f.params)
						launchDone <- notificationResult{id, err}
					}()
				}
				var early *receiptRaceResult
				if timing == "lookup_before_insert" {
					startClaim()
					select {
					case row := <-lookup:
						if row != nil {
							t.Fatal("initial lookup did not observe absence")
						}
					case <-time.After(3 * time.Second):
						t.Fatal("lookup did not start")
					}
					startLaunch()
					select {
					case <-cancel:
					case <-time.After(100 * time.Millisecond):
					}
					unblockLookup()
					select {
					case result := <-claimDone:
						early = &result
					case <-time.After(3 * time.Second):
						t.Fatal("initial receipt did not return")
					}
					if early.err != nil || early.result.Receipt != nil {
						t.Errorf("initial absent-row claim raced into transient admission: %+v %v", early.result, early.err)
					}
					select {
					case <-cancel:
					case <-time.After(3 * time.Second):
						t.Fatal("admission did not reach cancellation")
					}
				} else {
					startLaunch()
					select {
					case <-cancel:
					case <-time.After(3 * time.Second):
						t.Fatal("admission did not reach cancellation")
					}
					startClaim()
					select {
					case row := <-lookup:
						if row == nil {
							t.Error("pending-row race fixture saw no row")
						}
					case <-time.After(100 * time.Millisecond):
					}
					unblockLookup()
					select {
					case result := <-claimDone:
						early = &result
						t.Errorf("claim exposed internal pre-admission row: %+v %v", result.result, result.err)
					case <-time.After(100 * time.Millisecond):
					}
				}
				pending, err := f.d.GetRunByLaunchNonce("recovery", "fresh", claim.LaunchNonce)
				if err != nil || pending == nil || pending.Status != types.RunPending {
					t.Fatalf("fixture not paused in pending admission: %+v %v", pending, err)
				}
				if pending.LaunchReceiptClaimedAt != nil {
					t.Error("transient admission consumed created receipt")
				}
				unblockCancel()
				var launched notificationResult
				select {
				case launched = <-launchDone:
				case <-time.After(3 * time.Second):
					t.Fatal("launch did not settle")
				}
				if failure {
					if launched.err == nil || !strings.Contains(launched.err.Error(), "supersede predecessor") {
						t.Fatalf("wrong setup failure: %+v", launched)
					}
				} else if launched.err != nil {
					t.Fatal(launched.err)
				}
				var results []receiptRaceResult
				if timing == "lookup_after_insert" && early == nil {
					select {
					case result := <-claimDone:
						results = append(results, result)
					case <-time.After(3 * time.Second):
						t.Fatal("receipt not released by admission")
					}
				}
				clients := []*ipc.Client{notificationIPCClient(t, f), notificationIPCClient(t, f), notificationIPCClient(t, f)}
				concurrent := make(chan receiptRaceResult, len(clients))
				for _, client := range clients {
					go func(c *ipc.Client) {
						var result ipc.ClaimLaunchReceiptResult
						err := c.Call(ipc.MethodClaimLaunchReceipt, &claim, &result)
						concurrent <- receiptRaceResult{result, err}
					}(client)
				}
				for range clients {
					select {
					case result := <-concurrent:
						results = append(results, result)
					case <-time.After(3 * time.Second):
						t.Fatal("matching claims deadlocked")
					}
				}
				created := 0
				for _, result := range results {
					if failure {
						if result.err == nil || !strings.Contains(result.err.Error(), "supersede predecessor") || result.result.Receipt != nil {
							t.Errorf("claim hid setup failure: %+v %v", result.result, result.err)
						}
						continue
					}
					if result.err != nil || result.result.Receipt == nil {
						t.Fatalf("healthy receipt refused: %+v %v", result.result, result.err)
					}
					receipt := result.result.Receipt
					if receipt.RunID != launched.id || receipt.Owner != claim.Owner || receipt.LaunchNonce != claim.LaunchNonce || receipt.SubmittedHeadSHA != claim.SubmittedHeadSHA || receipt.IntentDigest != claim.IntentDigest || receipt.ValidationGeneration != claim.ValidationGeneration {
						t.Fatalf("wrong receipt proof: %+v", receipt)
					}
					if receipt.Disposition == "created" {
						created++
					} else if receipt.Disposition != "reused" {
						t.Fatalf("wrong disposition: %+v", receipt)
					}
				}
				if !failure && created != 1 {
					t.Errorf("successful created receipts=%d, want exactly one", created)
				}
				saved, err := f.d.GetRun(pending.ID)
				if err != nil {
					t.Fatal(err)
				}
				if failure && (saved.Status != types.RunFailed || saved.LaunchReceiptClaimedAt != nil) {
					t.Fatalf("failed admission claimed or left pending: %+v", saved)
				}
				rows, err := f.d.GetRunsByRepo("recovery")
				if err != nil || len(rows) != 2 {
					t.Fatalf("receipt duplicated run: %+v %v", rows, err)
				}
			})
		}
	}
}

func TestNotificationReceiptMismatchNeverConsumesCreated(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	f.params.LaunchNonce = "receipt-mismatch"
	f.params.ValidationGeneration = "generation"
	f.params.Owner = "Firstmate"
	id, err := f.m.HandlePushReceived(context.Background(), f.params)
	if err != nil {
		t.Fatal(err)
	}
	claim := ipc.ClaimLaunchReceiptParams{RepoID: "recovery", Branch: "fresh", LaunchNonce: f.params.LaunchNonce, SubmittedHeadSHA: f.params.New, ValidationGeneration: f.params.ValidationGeneration, IntentDigest: digestIntent(f.params.Intent), Owner: f.params.Owner}
	before, err := f.d.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	rawBefore, _ := json.Marshal(before)
	c := notificationIPCClient(t, f)
	for _, field := range []string{"owner", "head", "nonce", "generation", "intent", "base", "shipping", "pi", "omit", "external"} {
		t.Run(field, func(t *testing.T) {
			mismatch := claim
			switch field {
			case "owner":
				mismatch.Owner = "Other"
			case "head":
				mismatch.SubmittedHeadSHA = strings.Repeat("f", 40)
			case "nonce":
				mismatch.LaunchNonce = "other"
			case "generation":
				mismatch.ValidationGeneration = "other"
			case "intent":
				mismatch.IntentDigest = "other"
			case "base":
				mismatch.PRBaseBranch = "other/base"
			case "shipping":
				mismatch.Shipping = &types.ShippingOptions{Mode: types.ShippingFull}
			case "pi":
				mismatch.PiProfile = &agentcfg.PiProfile{Model: "openai/other"}
			case "omit":
				mismatch.OmitIntent = true
			case "external":
				mismatch.ExternalCIOwner = "controller-ship-pr"
			}
			var result ipc.ClaimLaunchReceiptResult
			_ = c.Call(ipc.MethodClaimLaunchReceipt, &mismatch, &result)
			if result.Receipt != nil {
				t.Fatalf("mismatch accepted receipt: %+v", result)
			}
			after, err := f.d.GetRun(id)
			if err != nil {
				t.Fatal(err)
			}
			rawAfter, _ := json.Marshal(after)
			if string(rawBefore) != string(rawAfter) {
				t.Fatal("mismatch mutated durable run/receipt state")
			}
		})
	}
	var first, again ipc.ClaimLaunchReceiptResult
	if err := c.Call(ipc.MethodClaimLaunchReceipt, &claim, &first); err != nil || first.Receipt == nil || first.Receipt.Disposition != "created" {
		t.Fatalf("mismatches consumed created: %+v %v", first, err)
	}
	if err := c.Call(ipc.MethodClaimLaunchReceipt, &claim, &again); err != nil || again.Receipt == nil || again.Receipt.Disposition != "reused" {
		t.Fatalf("repeat claim not reused: %+v %v", again, err)
	}
}

func TestReceiptFailureAfterLookupDoesNotConsumeCreated(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	f.params.LaunchNonce = "receipt-failure-race"
	f.params.ValidationGeneration = "generation"
	id, err := f.m.HandlePushReceived(context.Background(), f.params)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunStatus(id, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	f.m.afterReceiptLookup = func(run *db.Run) {
		if run == nil || run.Status != types.RunRunning {
			t.Fatalf("lookup = %+v", run)
		}
		if err := f.d.UpdateRunError(id, "failure before claim"); err != nil {
			t.Fatal(err)
		}
	}
	claim := ipc.ClaimLaunchReceiptParams{RepoID: "recovery", Branch: "fresh", LaunchNonce: f.params.LaunchNonce, SubmittedHeadSHA: f.params.New, ValidationGeneration: f.params.ValidationGeneration, IntentDigest: digestIntent(f.params.Intent)}
	result, err := f.m.claimLaunchReceiptLocked(&claim, "")
	if err == nil || !strings.Contains(err.Error(), "failure before claim") || result != nil {
		t.Fatalf("failed claim = %+v, %v", result, err)
	}
	saved, err := f.d.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != types.RunFailed || saved.LaunchReceiptClaimedAt != nil {
		t.Fatalf("failed claim consumed created: %+v", saved)
	}
}

func TestReceiptOrphanPendingRunDoesNotConsumeCreated(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	f.params.LaunchNonce = "receipt-orphan"
	f.params.ValidationGeneration = "generation"
	id, err := f.m.HandlePushReceived(context.Background(), f.params)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunStatus(id, types.RunPending); err != nil {
		t.Fatal(err)
	}
	f.m.mu.Lock()
	delete(f.m.runs, id)
	f.m.mu.Unlock()
	claim := ipc.ClaimLaunchReceiptParams{RepoID: "recovery", Branch: "fresh", LaunchNonce: f.params.LaunchNonce, SubmittedHeadSHA: f.params.New, ValidationGeneration: f.params.ValidationGeneration, IntentDigest: digestIntent(f.params.Intent)}
	result, err := f.m.claimLaunchReceiptLocked(&claim, "")
	if err == nil || !strings.Contains(err.Error(), "pending without scheduler or manager ownership") || result != nil {
		t.Fatalf("orphan claim = %+v, %v", result, err)
	}
	saved, err := f.d.GetRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if saved.LaunchReceiptClaimedAt != nil {
		t.Fatal("orphan claim consumed created")
	}
}
