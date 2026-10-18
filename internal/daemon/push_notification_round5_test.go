package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestNotificationCancellationFailureSettlesReplacement(t *testing.T) {
	for _, mode := range []string{"ordinary", "nonce", "rerun"} {
		t.Run(mode, func(t *testing.T) {
			f := newPushRecoveryFixture(t, true)
			if mode == "nonce" {
				f.params.LaunchNonce = "cancel-failure"
				f.params.ValidationGeneration = "cancel-generation"
			}
			writer, err := sql.Open("sqlite", f.p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if _, err := writer.Exec("CREATE TRIGGER reject_predecessor_cancel BEFORE UPDATE OF status ON runs WHEN OLD.id='" + f.predecessor + "' AND NEW.status='cancelled' BEGIN SELECT RAISE(ABORT,'injected predecessor cancellation failure'); END"); err != nil {
				t.Fatal(err)
			}
			var failure error
			if mode == "rerun" {
				_, failure = f.m.HandleRerun(types.WithRunOwner(context.Background(), "Firstmate"), "recovery", "fresh", f.predecessor, nil, "rerun accepted head", "", false, "", "", "", nil)
			} else {
				_, failure = f.m.HandlePushReceived(context.Background(), f.params)
			}
			if failure == nil {
				t.Fatal("predecessor cancellation unexpectedly succeeded")
			}
			rows, err := f.d.GetRunsByRepo("recovery")
			if err != nil || len(rows) != 2 {
				t.Fatalf("replacement was not committed before cancellation: %+v %v", rows, err)
			}
			replacement := rows[0]
			if replacement.ID == f.predecessor {
				replacement = rows[1]
			}
			if replacement.Status != types.RunFailed || replacement.Error == nil || !strings.Contains(*replacement.Error, "supersede predecessor") {
				t.Errorf("unowned replacement not durably settled: %+v", replacement)
			}
			if f.m.runs[replacement.ID] != nil {
				t.Error("failed replacement unexpectedly owns manager state")
			}
			predecessor, _ := f.d.GetRun(f.predecessor)
			if predecessor.Status != types.RunQueued {
				t.Error("failed cancellation changed predecessor")
			}
			if mode != "rerun" {
				event, err := f.d.GetOwnerAdmission(replacement.ID)
				if err != nil || event == nil {
					t.Fatalf("admission/event did not commit before cancellation: %+v %v", event, err)
				}
				records, err := f.m.ListPushNotifications()
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, record := range records {
					if record.RunID == replacement.ID {
						found = true
						if id, err := f.m.ReplayPushNotification(context.Background(), record.ID); err == nil || id != "" || !strings.Contains(err.Error(), "supersede predecessor") {
							t.Errorf("failed admission replay reported success: %s %v", id, err)
						}
					}
				}
				if !found {
					t.Fatal("committed notification binding lost")
				}
				if _, err := f.m.HandlePushReceived(context.Background(), f.params); err == nil || !strings.Contains(err.Error(), "supersede predecessor") {
					t.Errorf("duplicate failed receive reported success: %v", err)
				}
				if mode == "nonce" {
					if _, err := f.m.HandleStartFreshRun(context.Background(), &ipc.StartFreshRunParams{RepoID: "recovery", Branch: "fresh", HeadSHA: f.params.New, Intent: f.params.Intent, LaunchNonce: f.params.LaunchNonce, ValidationGeneration: f.params.ValidationGeneration}); err == nil || !strings.Contains(err.Error(), "supersede predecessor") {
						t.Errorf("failed nonce reattachment reported success: %v", err)
					}
					client := notificationIPCClient(t, f)
					var receipt ipc.ClaimLaunchReceiptResult
					claim := &ipc.ClaimLaunchReceiptParams{RepoID: "recovery", Branch: "fresh", LaunchNonce: f.params.LaunchNonce, SubmittedHeadSHA: f.params.New, ValidationGeneration: f.params.ValidationGeneration, IntentDigest: digestIntent(f.params.Intent)}
					if err := client.Call(ipc.MethodClaimLaunchReceipt, claim, &receipt); err == nil || !strings.Contains(err.Error(), "supersede predecessor") {
						t.Errorf("failed nonce claim reported success: %v", err)
					}
					saved, _ := f.d.GetRun(replacement.ID)
					if saved.LaunchReceiptClaimedAt != nil {
						t.Error("failed nonce claim consumed receipt")
					}
				}
				savedRows, err := f.d.GetRunsByRepo("recovery")
				if err != nil || len(savedRows) != 2 {
					t.Errorf("failed replay duplicated run: %+v %v", savedRows, err)
				}
			}
		})
	}
}

func TestNotificationReceivesMintIndependentIDs(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	repo, err := f.d.GetRepo("recovery")
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.m.retainPush(f.params, repo)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.m.retainPush(f.params, repo)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("separate receives reused content-derived identity: %s", first.ID)
	}
	for _, retained := range []*retainedPush{first, second} {
		raw, err := os.ReadFile(f.m.notificationPath(retained.ID))
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			ReceiveID string `json:"receive_id"`
		}
		if err := json.Unmarshal(raw, &record); err != nil || record.ReceiveID != retained.ID {
			t.Fatalf("minted identity not stored verbatim: %+v %v", record, err)
		}
	}

	reopenRecovery(t, f)
	id, err := f.m.ReplayPushNotification(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.m.ReplayPushNotification(context.Background(), first.ID)
	if err != nil || again != id {
		t.Fatalf("stored receive replay changed identity: %s %v", again, err)
	}
	info, err := f.m.InspectPushNotification(first.ID)
	if err != nil || info.ID != first.ID || info.RunID != id {
		t.Fatalf("original stored ID lost: %+v %v", info, err)
	}
	rows, err := f.d.GetRunsByRepo("recovery")
	if err != nil || len(rows) != 1 {
		t.Fatalf("replay duplicated run: %+v %v", rows, err)
	}
}

func TestNotificationStartupCutoffProtectsEqualAndNewerTemps(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	dir := filepath.Join(f.p.Root(), "push-notifications")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Unix(1700000000, 0)
	fixtures := map[string]time.Time{".notification-1": cutoff.Add(-time.Second), ".notification-2": cutoff, ".notification-3": cutoff.Add(time.Second)}
	for name, stamp := range fixtures {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("temporary"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.DefaultGlobalConfig()
	cfg.WorktreeRoots = map[string]string{f.params.Gate: filepath.Join(f.p.Root(), "invalid-placement")}
	lock, err := acquireSingletonLock(f.p)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	err = runWithOptionsLocked(f.p, f.d, cfg, nil, cutoff)
	if err == nil || !strings.Contains(err.Error(), "placement is unusable") {
		t.Fatalf("startup did not stop in preflight: %v", err)
	}
	for name, stamp := range fixtures {
		_, err := os.Stat(filepath.Join(dir, name))
		if stamp.Before(cutoff) {
			if !os.IsNotExist(err) {
				t.Errorf("old temp retained: %s %v", name, err)
			}
		} else if err != nil {
			t.Errorf("equal/newer temp removed: %s %v", name, err)
		}
	}
}

func TestNotificationUnownedPendingAndTerminalOutcomes(t *testing.T) {
	for _, status := range []types.RunStatus{types.RunPending, types.RunFailed, types.RunCompleted} {
		t.Run(string(status), func(t *testing.T) {
			f := newPushRecoveryFixture(t, false)
			repo, err := f.d.GetRepo("recovery")
			if err != nil {
				t.Fatal(err)
			}
			retained, err := f.m.retainPush(f.params, repo)
			if err != nil {
				t.Fatal(err)
			}
			run, err := f.d.InsertRunWithAllOptions("recovery", "fresh", f.params.New, f.params.Old, nil, "outcome-nonce", "outcome-generation", "intent-digest", "", false, false, "", nil, nil, nil, db.RunCreation{NotificationID: retained.ID})
			if err != nil {
				t.Fatal(err)
			}
			if status == types.RunFailed {
				if _, err := f.d.TerminateUnstartedRun(run.ID, "queue run: injected setup failure", status); err != nil {
					t.Fatal(err)
				}
			} else if status != types.RunPending {
				if err := f.d.UpdateRunStatus(run.ID, status); err != nil {
					t.Fatal(err)
				}
			}
			id, err := f.m.ReplayPushNotification(context.Background(), retained.ID)
			if status == types.RunCompleted {
				if err != nil || id != run.ID {
					t.Fatalf("completed replay lost binding: %s %v", id, err)
				}
			} else if err == nil || id != "" {
				t.Fatalf("unscheduled/failed replay reported success: %s %v", id, err)
			}
			info, inspectErr := f.m.InspectPushNotification(retained.ID)
			if inspectErr != nil || info.RunID != run.ID {
				t.Fatalf("outcome binding lost: %+v %v", info, inspectErr)
			}
			if status != types.RunCompleted && info.Error == "" {
				t.Error("failed or unowned outcome invisible in inspect")
			}
		})
	}
}
