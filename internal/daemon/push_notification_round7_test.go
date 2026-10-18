package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestNotificationReturnAfterFailedInterveningAdmission(t *testing.T) {
	for _, delivery := range []string{"receive", "pending_replay"} {
		for _, ownership := range []string{"ownerless", "owned", "policy_off"} {
			for _, intervening := range []string{"ordinary", "nonce"} {
				t.Run(delivery+"/"+ownership+"/"+intervening, func(t *testing.T) {
					ctx := context.Background()
					f := newPushRecoveryFixture(t, false)
					if ownership == "owned" {
						f.params.Owner = "Firstmate"
					}
					if ownership == "policy_off" {
						if err := os.WriteFile(f.p.ConfigFile(), []byte("intent:\n  require_owner: false\n"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					first, err := f.m.HandlePushReceived(ctx, f.params)
					if err != nil {
						t.Fatal(err)
					}
					headB := f.params.New
					gitCmd(t, f.params.Gate, "commit", "--allow-empty", "-m", "accepted intervening C")
					headC := gitOutput(t, f.params.Gate, "rev-parse", "HEAD")
					gitCmd(t, f.params.Gate, "update-ref", f.params.Ref, headC, headB)
					writer, err := sql.Open("sqlite", f.p.DB())
					if err != nil {
						t.Fatal(err)
					}
					defer writer.Close()
					if _, err := writer.Exec("CREATE TRIGGER reject_intervening BEFORE INSERT ON runs BEGIN SELECT RAISE(ABORT, 'intervening insert fault'); END"); err != nil {
						t.Fatal(err)
					}
					outgoing := *f.params
					outgoing.Old, outgoing.New = headB, headC
					if intervening == "nonce" {
						outgoing.LaunchNonce = "intervening-failure"
						outgoing.ValidationGeneration = "generation-1"
					}
					if _, err := f.m.HandlePushReceived(ctx, &outgoing); err == nil {
						t.Fatal("insertion fault did not fire")
					}
					rows, err := f.d.GetRunsByRepo("recovery")
					if err != nil || len(rows) != 1 || rows[0].ID != first || rows[0].Status != types.RunQueued || rows[0].HeadSHA != headB {
						t.Fatalf("failed admission damaged predecessor: %+v %v", rows, err)
					}
					records, err := f.m.ListPushNotifications()
					if err != nil || len(records) != 2 {
						t.Fatalf("lost failed receive: %+v %v", records, err)
					}
					pendingID := ""
					for _, record := range records {
						if record.Pending {
							pendingID = record.ID
						}
					}
					if pendingID == "" {
						t.Fatal("no pending intervening receive")
					}
					before, err := os.ReadFile(f.m.notificationPath(pendingID))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := writer.Exec("DROP TRIGGER reject_intervening"); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, f.params.Gate, "update-ref", f.params.Ref, headB, headC)
					returning := *f.params
					returning.Old = headC
					checkedCommit := false
					f.m.afterSupersedeSnapshot = func() {
						committed, err := f.d.GetRunsByRepo("recovery")
						if err != nil || len(committed) != 2 {
							t.Fatalf("cancel before replacement commit: %+v %v", committed, err)
						}
						for _, row := range committed {
							if row.ID == first {
								if row.Status != types.RunQueued {
									t.Fatal("predecessor cancelled before check")
								}
								continue
							}
							event, err := f.d.GetOwnerAdmission(row.ID)
							if err != nil || (event != nil) != (ownership == "ownerless") {
								t.Fatalf("cancel before audit commit: %+v %v", event, err)
							}
							ids, err := f.d.PushNotificationsForRun(row.ID)
							if err != nil || len(ids) != 1 {
								t.Fatalf("cancel before binding commit: %+v %v", ids, err)
							}
						}
						checkedCommit = true
					}
					var second, receiveID string
					if delivery == "pending_replay" {
						repo, repoErr := f.d.GetRepo("recovery")
						if repoErr != nil {
							t.Fatal(repoErr)
						}
						retained, retainErr := f.m.retainPush(&returning, repo)
						if retainErr != nil {
							t.Fatal(retainErr)
						}
						receiveID = retained.ID
						second, err = f.m.ReplayPushNotification(ctx, receiveID)
					} else {
						second, err = f.m.HandlePushReceived(ctx, &returning)
					}
					if err != nil || second == first || second == "" {
						t.Fatalf("accepted returning transition lost validation: %s %v", second, err)
					}
					if !checkedCommit {
						t.Fatal("replacement skipped captured predecessor cancellation")
					}
					f.m.afterSupersedeSnapshot = nil
					records, err = f.m.ListPushNotifications()
					if err != nil || len(records) != 3 {
						t.Fatalf("wrong receive count: %+v %v", records, err)
					}
					for _, record := range records {
						if record.RunID == second {
							receiveID = record.ID
						}
					}
					if receiveID == "" || receiveID == pendingID {
						t.Fatal("return lost its receive identity")
					}
					run, err := f.d.GetRun(second)
					if err != nil || run.Status != types.RunQueued || run.Owner() != returning.Owner {
						t.Fatalf("wrong replacement: %+v %v", run, err)
					}
					prior, err := f.d.GetRun(first)
					if err != nil || prior.Status != types.RunCancelled {
						t.Fatalf("predecessor not superseded: %+v %v", prior, err)
					}
					event, err := f.d.GetOwnerAdmission(second)
					if err != nil || (event != nil) != (ownership == "ownerless") {
						t.Fatalf("wrong audit: %+v %v", event, err)
					}
					for i := 0; i < 2; i++ {
						id, err := f.m.ReplayPushNotification(ctx, receiveID)
						if err != nil || id != second {
							t.Fatalf("original replay duplicated: %s %v", id, err)
						}
					}
					if _, err := f.m.ReplayPushNotification(ctx, pendingID); err == nil {
						t.Fatal("stale intervening receive superseded return")
					}
					after, err := os.ReadFile(f.m.notificationPath(pendingID))
					if err != nil || !bytes.Equal(before, after) {
						t.Fatal("pending evidence bytes changed")
					}
					rows, err = f.d.GetRunsByRepo("recovery")
					if err != nil || len(rows) != 2 {
						t.Fatalf("replay created work: %+v %v", rows, err)
					}
					run, err = f.d.GetRun(second)
					if err != nil || run.Status != types.RunQueued {
						t.Fatalf("stale replay damaged return: %+v %v", run, err)
					}
					var events int
					if err := writer.QueryRow("SELECT count(*) FROM owner_admission_events").Scan(&events); err != nil {
						t.Fatal(err)
					}
					want := 0
					if ownership == "ownerless" {
						want = 2
					}
					if events != want {
						t.Fatalf("replay changed event count: %d want %d", events, want)
					}
				})
			}
		}
	}
}

func TestNotificationReturnRejectsUnrelatedTransitionEvidence(t *testing.T) {
	for _, kind := range []string{"branch", "root", "repo", "predecessor", "edge", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			f := newPushRecoveryFixture(t, false)
			ctx := context.Background()
			first, err := f.m.HandlePushReceived(ctx, f.params)
			if err != nil {
				t.Fatal(err)
			}
			gitCmd(t, f.params.Gate, "commit", "--allow-empty", "-m", "unrelated C")
			headC := gitOutput(t, f.params.Gate, "rev-parse", "HEAD")
			candidate := *f.params
			candidate.Old, candidate.New = f.params.New, headC
			repo, err := f.d.GetRepo("recovery")
			if err != nil {
				t.Fatal(err)
			}
			retained, err := f.m.retainPush(&candidate, repo)
			if err != nil {
				t.Fatal(err)
			}
			record := retained.Record
			switch kind {
			case "branch":
				record.Params.Ref = "refs/heads/other"
			case "root":
				record.Params.Gate = t.TempDir()
			case "repo":
				record.RepoID = "other"
			case "predecessor":
				record.PreviousRunID = "older-run"
			case "edge":
				record.Params.Old = f.params.Old
			}
			record.Checksum = notificationChecksum(record)
			if kind == "corrupt" {
				record.Checksum = "invalid"
			}
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.m.notificationPath(retained.ID), raw, 0600); err != nil {
				t.Fatal(err)
			}
			returning := *f.params
			returning.Old = headC
			if _, err := f.m.HandlePushReceived(ctx, &returning); err == nil {
				t.Fatal("unrelated evidence admitted stale receive")
			}
			rows, err := f.d.GetRunsByRepo("recovery")
			if err != nil || len(rows) != 1 || rows[0].ID != first || rows[0].Status != types.RunQueued {
				t.Fatalf("unrelated evidence changed work: %+v %v", rows, err)
			}
		})
	}
}

func TestNotificationStaleSameContextIsNotAcceptedTransitionProof(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	ctx := context.Background()
	first, err := f.m.HandlePushReceived(ctx, f.params)
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.params.Gate, "commit", "--allow-empty", "-m", "unreceived C")
	headC := gitOutput(t, f.params.Gate, "rev-parse", "HEAD")
	stale := *f.params
	stale.Old, stale.New = f.params.New, headC
	if _, err := f.m.HandlePushReceived(ctx, &stale); err == nil {
		t.Fatal("unreceived C passed current-ref validation")
	}
	records, err := f.m.ListPushNotifications()
	if err != nil || len(records) != 2 {
		t.Fatalf("stale receive not retained: %+v %v", records, err)
	}
	var evidence *pushNotification
	for _, record := range records {
		if record.Pending {
			evidence, err = f.m.readNotification(record.ID)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if evidence == nil || evidence.PreviousRunID != first || evidence.Params.Old != f.params.New || evidence.Params.New != headC || evidence.GateIdentity != canonicalRoot(f.params.Gate) || evidence.RepoID != "recovery" {
		t.Fatalf("fixture lacks indistinguishable transition context: %+v", evidence)
	}
	returning := *f.params
	returning.Old = headC
	if _, err := f.m.HandlePushReceived(ctx, &returning); err == nil {
		t.Fatal("stale journal entry was used as accepted ref-transition proof")
	}
	run, err := f.d.GetRun(first)
	if err != nil || run.Status != types.RunQueued {
		t.Fatalf("stale evidence cancelled predecessor: %+v %v", run, err)
	}
	rows, err := f.d.GetRunsByRepo("recovery")
	if err != nil || len(rows) != 1 {
		t.Fatalf("stale evidence created validation: %+v %v", rows, err)
	}
}
