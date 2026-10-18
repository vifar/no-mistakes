package daemon

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestNotificationReturningRefTransitionsAdmitNewValidation(t *testing.T) {
	for _, transition := range []string{"published_head_return", "recreate_same_options", "recreate_changed_options"} {
		for _, delivery := range []string{"receive", "pending_replay"} {
			for _, ownership := range []string{"ownerless", "owned", "policy_off"} {
				t.Run(transition+"/"+delivery+"/"+ownership, func(t *testing.T) {
					f := newPushRecoveryFixture(t, false)
					if transition != "published_head_return" {
						f.params.Old = strings.Repeat("0", 40)
					}
					if ownership == "owned" {
						f.params.Owner = "Firstmate"
					}
					if ownership == "policy_off" {
						if err := os.WriteFile(f.p.ConfigFile(), []byte("intent:\n  require_owner: false\n"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					first, err := f.m.HandlePushReceived(context.Background(), f.params)
					if err != nil {
						t.Fatal(err)
					}
					records, err := f.m.ListPushNotifications()
					if err != nil || len(records) != 1 {
						t.Fatalf("original receive missing: %+v %v", records, err)
					}
					firstReceiveID := records[0].ID
					headB := f.params.New
					if transition == "published_head_return" {
						gitCmd(t, f.params.Gate, "commit", "--allow-empty", "-m", "pipeline rebase C")
						headC := gitOutput(t, f.params.Gate, "rev-parse", "HEAD")
						if err := f.d.UpdateRunHeadSHA(first, headC); err != nil {
							t.Fatal(err)
						}
						f.m.runs[first].run.HeadSHA = headC
						writer, err := sql.Open("sqlite", f.p.DB())
						if err != nil {
							t.Fatal(err)
						}
						_, err = writer.Exec("UPDATE runs SET last_pushed_sha=?,last_pushed_at=?,push_ref=? WHERE id=?", headC, time.Now().Unix(), f.params.Ref, first)
						writer.Close()
						if err != nil {
							t.Fatal(err)
						}
						gitCmd(t, f.params.Gate, "update-ref", f.params.Ref, headC, headB)
						gitCmd(t, f.params.Gate, "update-ref", f.params.Ref, headB, headC)
						f.params.Old = headC
					} else {
						gitCmd(t, f.params.Gate, "update-ref", "-d", f.params.Ref, headB)
						deleted := *f.params
						deleted.Old = headB
						deleted.New = strings.Repeat("0", 40)
						if _, err := f.m.HandlePushReceived(context.Background(), &deleted); err == nil || !strings.Contains(err.Error(), "ref deletion") {
							t.Fatalf("deletion contract changed: %v", err)
						}
						rows, err := f.d.GetRunsByRepo("recovery")
						if err != nil || len(rows) != 1 {
							t.Fatalf("deletion admitted work: %+v %v", rows, err)
						}
						afterDelete, err := f.m.ListPushNotifications()
						if err != nil || len(afterDelete) != 1 {
							t.Fatalf("deletion retained admission: %+v %v", afterDelete, err)
						}
						gitCmd(t, f.params.Gate, "update-ref", f.params.Ref, headB)
						if transition == "recreate_changed_options" {
							f.params.Intent = "validate recreated branch with revised intent"
						}
					}
					var second string
					var newReceiveID string
					if delivery == "pending_replay" {
						repo, repoErr := f.d.GetRepo("recovery")
						if repoErr != nil {
							t.Fatal(repoErr)
						}
						retained, retainErr := f.m.retainPush(f.params, repo)
						if retainErr != nil {
							t.Fatal(retainErr)
						}
						newReceiveID = retained.ID
						second, err = f.m.ReplayPushNotification(context.Background(), retained.ID)
					} else {
						second, err = f.m.HandlePushReceived(context.Background(), f.params)
					}
					if err != nil {
						t.Fatalf("legitimate %s refused new validation: %v", transition, err)
					}
					if second == first {
						t.Fatal("legitimate new transition reused historical validation")
					}
					records, err = f.m.ListPushNotifications()
					if err != nil || len(records) != 2 {
						t.Fatalf("receive identities not preserved: %+v %v", records, err)
					}
					for _, record := range records {
						if record.RunID == second {
							newReceiveID = record.ID
						}
					}
					if newReceiveID == "" || newReceiveID == firstReceiveID {
						t.Fatal("new receive lost unique key")
					}
					run, err := f.d.GetRun(second)
					if err != nil || run == nil || run.Status != types.RunQueued || run.Owner() != f.params.Owner {
						t.Fatalf("wrong new validation: %+v %v", run, err)
					}
					prior, err := f.d.GetRun(first)
					if err != nil || prior.Status != types.RunCancelled {
						t.Fatalf("predecessor not superseded: %+v %v", prior, err)
					}
					event, err := f.d.GetOwnerAdmission(second)
					if err != nil {
						t.Fatal(err)
					}
					wantEvent := ownership == "ownerless"
					if (event != nil) != wantEvent {
						t.Fatalf("owner policy event mismatch: %+v", event)
					}
					for i := 0; i < 2; i++ {
						id, err := f.m.ReplayPushNotification(context.Background(), newReceiveID)
						if err != nil || id != second {
							t.Fatalf("original-ID replay duplicated validation: %s %v", id, err)
						}
					}
					rows, err := f.d.GetRunsByRepo("recovery")
					if err != nil || len(rows) != 2 {
						t.Fatalf("replay created another run: %+v %v", rows, err)
					}
					saved, err := f.m.InspectPushNotification(newReceiveID)
					if err != nil || saved.Params == nil || saved.Params.Intent != f.params.Intent {
						t.Fatalf("received options lost: %+v %v", saved, err)
					}
				})
			}
		}
	}
}

func TestNotificationCompatibleAttachmentWhilePipelineHeadMoves(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	first, err := f.m.HandlePushReceived(context.Background(), f.params)
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.params.Gate, "commit", "--allow-empty", "-m", "pipeline worktree repair")
	headC := gitOutput(t, f.params.Gate, "rev-parse", "HEAD")
	if err := f.d.UpdateRunHeadSHA(first, headC); err != nil {
		t.Fatal(err)
	}
	f.m.runs[first].run.HeadSHA = headC
	id, err := f.m.HandlePushReceived(context.Background(), f.params)
	if err != nil || id != first {
		t.Fatalf("compatible delivery lost run attachment: %s %v", id, err)
	}
	saved, err := f.d.GetRun(first)
	if err != nil || saved.HeadSHA != headC || saved.Status != types.RunQueued {
		t.Fatalf("reattachment changed pipeline work: %+v %v", saved, err)
	}
	rows, err := f.d.GetRunsByRepo("recovery")
	if err != nil || len(rows) != 1 {
		t.Fatalf("compatible delivery created validation: %+v %v", rows, err)
	}
}

func TestNotificationPublishedRefReturnUsesRecordedPushHead(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	first, err := f.m.HandlePushReceived(context.Background(), f.params)
	if err != nil {
		t.Fatal(err)
	}
	headB := f.params.New
	gitCmd(t, f.params.Gate, "commit", "--allow-empty", "-m", "published repair C")
	headC := gitOutput(t, f.params.Gate, "rev-parse", "HEAD")
	writer, err := sql.Open("sqlite", f.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	_, err = writer.Exec("UPDATE runs SET last_pushed_sha=?,last_pushed_at=?,push_ref=? WHERE id=?", headC, time.Now().Unix(), f.params.Ref, first)
	writer.Close()
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.params.Gate, "update-ref", f.params.Ref, headC, headB)
	gitCmd(t, f.params.Gate, "update-ref", f.params.Ref, headB, headC)
	f.params.Old = headC
	second, err := f.m.HandlePushReceived(context.Background(), f.params)
	if err != nil || second == first {
		t.Fatalf("published ref transition did not create validation: %s %v", second, err)
	}
	prior, _ := f.d.GetRun(first)
	if prior.Status != types.RunCancelled {
		t.Fatal("new transition did not supersede captured predecessor")
	}
}
