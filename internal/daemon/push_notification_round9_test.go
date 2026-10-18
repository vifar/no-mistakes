package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

type notificationResult struct {
	id  string
	err error
}

func TestNotificationConcurrentReceiveCapturesCommittedPredecessor(t *testing.T) {
	for _, firstNonce := range []bool{false, true} {
		for _, secondNonce := range []bool{false, true} {
			name := "ordinary"
			if firstNonce {
				name = "nonce"
			}
			if secondNonce {
				name += "_then_nonce"
			} else {
				name += "_then_ordinary"
			}
			t.Run(name, func(t *testing.T) {
				f := newPushRecoveryFixture(t, true)
				first := *f.params
				if firstNonce {
					first.LaunchNonce = "concurrent-first"
					first.ValidationGeneration = "one"
				}
				gitCmd(t, first.Gate, "commit", "--allow-empty", "-m", "next receive C")
				headC := gitOutput(t, first.Gate, "rev-parse", "HEAD")
				second := first
				second.Old, second.New = first.New, headC
				second.Owner = "Second-owner"
				second.LaunchNonce, second.ValidationGeneration = "", ""
				if secondNonce {
					second.LaunchNonce = "concurrent-second"
					second.ValidationGeneration = "two"
				}
				paused, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unblock()
				var insertCalls atomic.Int32
				f.m.beforeRunInsert = func() {
					if insertCalls.Add(1) == 1 {
						close(paused)
						<-release
					}
				}
				secondRetained := make(chan struct{})
				var retains atomic.Int32
				f.m.afterPushRetained = func() {
					if retains.Add(1) == 2 {
						close(secondRetained)
					}
				}
				firstDone, secondDone := make(chan notificationResult, 1), make(chan notificationResult, 1)
				go func() {
					id, err := f.m.HandlePushReceived(context.Background(), &first)
					firstDone <- notificationResult{id, err}
				}()
				select {
				case <-paused:
				case <-time.After(3 * time.Second):
					t.Fatal("first admission did not reach insert boundary")
				}
				gitCmd(t, first.Gate, "update-ref", first.Ref, headC, first.New)
				started := make(chan struct{})
				go func() {
					close(started)
					id, err := f.m.HandlePushReceived(context.Background(), &second)
					secondDone <- notificationResult{id, err}
				}()
				<-started
				select {
				case <-secondRetained:
				case <-time.After(100 * time.Millisecond):
				}
				unblock()
				var r1, r2 notificationResult
				select {
				case r1 = <-firstDone:
				case <-time.After(3 * time.Second):
					t.Fatal("first admission deadlocked")
				}
				select {
				case r2 = <-secondDone:
				case <-time.After(3 * time.Second):
					t.Fatal("second admission deadlocked")
				}
				if r1.err != nil || r2.err != nil {
					t.Fatalf("serialized accepted receives failed: first=%s %v second=%s %v", r1.id, r1.err, r2.id, r2.err)
				}
				records, err := f.m.ListPushNotifications()
				if err != nil || len(records) != 3 {
					t.Fatalf("wrong retained receive count: %+v %v", records, err)
				}
				receiveID := ""
				for _, info := range records {
					if info.RunID == r2.id {
						receiveID = info.ID
					}
				}
				saved, err := f.m.readNotification(receiveID)
				if err != nil || saved.PreviousRunID != r1.id || saved.ReceiveID != receiveID || !sameNotificationOptions(saved.Params, second) {
					t.Fatalf("obsolete or rewritten receive context: %+v %v", saved, err)
				}
				before, err := os.ReadFile(f.m.notificationPath(receiveID))
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					id, err := f.m.ReplayPushNotification(context.Background(), receiveID)
					if err != nil || id != r2.id {
						t.Fatalf("original receive unreplayable: %s %v", id, err)
					}
				}
				after, err := os.ReadFile(f.m.notificationPath(receiveID))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("replay rewrote original context")
				}
				rows, err := f.d.GetRunsByRepo("recovery")
				if err != nil || len(rows) != 3 {
					t.Fatalf("replay duplicated admission: %+v %v", rows, err)
				}
				prior, _ := f.d.GetRun(r1.id)
				latest, _ := f.d.GetRun(r2.id)
				if prior.Status != types.RunCancelled || latest.Status != types.RunQueued || latest.Owner() != second.Owner {
					t.Fatalf("wrong supersession: %+v %+v", prior, latest)
				}
			})
		}
	}
}

func TestNotificationMarkedMultihopReturn(t *testing.T) {
	for _, delivery := range []string{"receive", "replay"} {
		for _, fault := range []string{"none", "first_unmarked", "second_unmarked", "corrupt", "foreign_root", "foreign_repo", "foreign_ref", "foreign_predecessor"} {
			t.Run(delivery+"/"+fault, func(t *testing.T) {
				f := newPushRecoveryFixture(t, false)
				ctx := context.Background()
				first, err := f.m.HandlePushReceived(ctx, f.params)
				if err != nil {
					t.Fatal(err)
				}
				writer, err := sql.Open("sqlite", f.p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Close()
				if _, err := writer.Exec("CREATE TRIGGER multihop_fault BEFORE INSERT ON runs BEGIN SELECT RAISE(ABORT,'multihop insert fault'); END"); err != nil {
					t.Fatal(err)
				}
				headB := f.params.New
				previous := headB
				var edges []*retainedPush
				for _, label := range []string{"C", "D"} {
					gitCmd(t, f.params.Gate, "commit", "--allow-empty", "-m", label)
					next := gitOutput(t, f.params.Gate, "rev-parse", "HEAD")
					gitCmd(t, f.params.Gate, "update-ref", f.params.Ref, next, previous)
					params := *f.params
					params.Old, params.New = previous, next
					if _, err := f.m.HandlePushReceived(ctx, &params); err == nil || !strings.Contains(err.Error(), "multihop insert fault") {
						t.Fatalf("failed admission not exercised: %v", err)
					}
					records, _ := f.m.ListPushNotifications()
					for _, info := range records {
						record, err := f.m.readNotification(info.ID)
						if err != nil {
							t.Fatal(err)
						}
						if record.Params.New == next {
							if !record.Validated || record.PreviousRunID != first {
								t.Fatalf("missing validated context: %+v", record)
							}
							edges = append(edges, &retainedPush{ID: info.ID, Record: record})
						}
					}
					if label == "C" {
						cycle := params
						cycle.Old = next
						if _, err := f.m.HandlePushReceived(ctx, &cycle); err == nil || !strings.Contains(err.Error(), "multihop insert fault") {
							t.Fatalf("validated cycle not exercised: %v", err)
						}
					}
					previous = next
				}
				prior, _ := f.d.GetRun(first)
				if prior.Status != types.RunQueued {
					t.Fatal("failed insert cancelled predecessor")
				}
				if fault != "none" {
					edge := edges[1]
					if fault == "first_unmarked" {
						edge = edges[0]
					}
					switch fault {
					case "first_unmarked", "second_unmarked":
						edge.Record.Validated = false
					case "corrupt":
						edge.Record.Checksum = "invalid"
					case "foreign_root":
						edge.Record.Params.Gate = t.TempDir()
					case "foreign_repo":
						edge.Record.RepoID = "foreign"
					case "foreign_ref":
						edge.Record.Params.Ref = "refs/heads/foreign"
					case "foreign_predecessor":
						edge.Record.PreviousRunID = "older"
					}
					if fault != "corrupt" {
						edge.Record.Checksum = notificationChecksum(edge.Record)
					}
					raw, _ := json.Marshal(edge.Record)
					if err := os.WriteFile(f.m.notificationPath(edge.ID), raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
				repo, _ := f.d.GetRepo("recovery")
				for _, pair := range [][2]string{{previous, edges[0].Record.Params.New}, {strings.Repeat("f", 40), previous}} {
					unrelated := *f.params
					unrelated.Old, unrelated.New = pair[0], pair[1]
					retained, err := f.m.retainPush(&unrelated, repo)
					if err != nil {
						t.Fatal(err)
					}
					if retained.Record.Validated {
						t.Fatal("retention synthesized validation")
					}
				}
				if _, err := writer.Exec("DROP TRIGGER multihop_fault"); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, f.params.Gate, "update-ref", f.params.Ref, headB, previous)
				returning := *f.params
				returning.Old = previous
				retained, err := f.m.retainPush(&returning, repo)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.m.validateNotification(ctx, retained); err != nil {
					t.Fatalf("fixture failed before provenance boundary: %v", err)
				}
				checkedCommit := false
				f.m.afterSupersedeSnapshot = func() {
					rows, err := f.d.GetRunsByRepo("recovery")
					if err != nil || len(rows) != 2 {
						t.Fatalf("multihop cancelled before commit: %+v %v", rows, err)
					}
					for _, row := range rows {
						if row.ID == first {
							if row.Status != types.RunQueued {
								t.Fatal("predecessor changed before replacement commit")
							}
							continue
						}
						event, err := f.d.GetOwnerAdmission(row.ID)
						if err != nil || event == nil {
							t.Fatalf("multihop cancelled before audit commit: %+v %v", event, err)
						}
						ids, err := f.d.PushNotificationsForRun(row.ID)
						if err != nil || len(ids) != 1 {
							t.Fatalf("multihop cancelled before binding commit: %+v %v", ids, err)
						}
					}
					checkedCommit = true
				}
				var id string
				if delivery == "receive" {
					id, err = f.m.HandlePushReceived(ctx, &returning)
				} else {
					id, err = f.m.ReplayPushNotification(ctx, retained.ID)
				}
				if fault != "none" {
					if err == nil || !strings.Contains(err.Error(), "stale notification: submitted head") {
						t.Fatalf("ineligible edge supplied proof: %s %v", id, err)
					}
					prior, _ := f.d.GetRun(first)
					if prior.Status != types.RunQueued {
						t.Fatal("invalid path cancelled predecessor")
					}
					return
				}
				if !checkedCommit {
					t.Fatal("multihop skipped predecessor settlement")
				}
				f.m.afterSupersedeSnapshot = nil
				if err != nil || id == first || id == "" {
					t.Fatalf("validated multihop return refused: %s %v", id, err)
				}
				records, _ := f.m.ListPushNotifications()
				receiveID := ""
				for _, info := range records {
					if info.RunID == id {
						receiveID = info.ID
					}
				}
				for i := 0; i < 2; i++ {
					again, err := f.m.ReplayPushNotification(ctx, receiveID)
					if err != nil || again != id {
						t.Fatalf("multihop replay duplicated: %s %v", again, err)
					}
				}
				rows, _ := f.d.GetRunsByRepo("recovery")
				if len(rows) != 2 {
					t.Fatal("multihop replay created extra run")
				}
				event, err := f.d.GetOwnerAdmission(id)
				if err != nil || event == nil {
					t.Fatalf("missing ownerless audit: %+v %v", event, err)
				}
			})
		}
	}
}

func TestNotificationMarkerCausalityAtValidReturnBoundary(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	ctx := context.Background()
	first, err := f.m.HandlePushReceived(ctx, f.params)
	if err != nil {
		t.Fatal(err)
	}
	headB := f.params.New
	gitCmd(t, f.params.Gate, "commit", "--allow-empty", "-m", "causality C")
	headC := gitOutput(t, f.params.Gate, "rev-parse", "HEAD")
	repo, _ := f.d.GetRepo("recovery")
	outgoing := *f.params
	outgoing.Old, outgoing.New = headB, headC
	edge, err := f.m.retainPush(&outgoing, repo)
	if err != nil {
		t.Fatal(err)
	}
	returning := *f.params
	returning.Old = headC
	pending, err := f.m.retainPush(&returning, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.validateNotification(ctx, pending); err != nil {
		t.Fatalf("negative failed live context instead of provenance: %v", err)
	}
	if _, err := f.m.ReplayPushNotification(ctx, pending.ID); err == nil || !strings.Contains(err.Error(), "stale notification: submitted head") {
		t.Fatalf("absent marker licensed return: %v", err)
	}
	prior, _ := f.d.GetRun(first)
	info, _ := f.m.InspectPushNotification(pending.ID)
	if prior.Status != types.RunQueued || !info.Pending {
		t.Fatal("absent marker changed work")
	}
	writer, err := sql.Open("sqlite", f.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec("CREATE TRIGGER causality_fault BEFORE INSERT ON runs BEGIN SELECT RAISE(ABORT,'causality insert fault'); END"); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.params.Gate, "update-ref", f.params.Ref, headC, headB)
	if _, err := f.m.ReplayPushNotification(ctx, edge.ID); err == nil || !strings.Contains(err.Error(), "causality insert fault") {
		t.Fatalf("actual edge validation not exercised: %v", err)
	}
	marked, err := f.m.readNotification(edge.ID)
	if err != nil || !marked.Validated {
		t.Fatalf("actual validation did not mark edge: %+v %v", marked, err)
	}
	if !sameNotificationOptions(marked.Params, outgoing) || marked.ReceiveID != edge.ID {
		t.Fatal("annotation changed edge identity")
	}
	if _, err := writer.Exec("DROP TRIGGER causality_fault"); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.params.Gate, "update-ref", f.params.Ref, headB, headC)
	if err := f.m.validateNotification(ctx, pending); err != nil {
		t.Fatalf("positive live context differs: %v", err)
	}
	second, err := f.m.ReplayPushNotification(ctx, pending.ID)
	if err != nil || second == first {
		t.Fatalf("present marker did not license return: %s %v", second, err)
	}
	again, err := f.m.ReplayPushNotification(ctx, pending.ID)
	if err != nil || again != second {
		t.Fatalf("paired original replay duplicated: %s %v", again, err)
	}
}

func TestNotificationWaitingReceiveRefusesMovedTip(t *testing.T) {
	f := newPushRecoveryFixture(t, true)
	paused, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	f.m.afterPushRetained = func() { close(paused); <-release }
	done := make(chan notificationResult, 1)
	go func() {
		id, err := f.m.HandlePushReceived(context.Background(), f.params)
		done <- notificationResult{id, err}
	}()
	select {
	case <-paused:
	case <-time.After(3 * time.Second):
		t.Fatal("receive not retained")
	}
	gitCmd(t, f.params.Gate, "commit", "--allow-empty", "-m", "superseding ref C")
	headC := gitOutput(t, f.params.Gate, "rev-parse", "HEAD")
	gitCmd(t, f.params.Gate, "update-ref", f.params.Ref, headC, f.params.New)
	unblock()
	select {
	case result := <-done:
		if result.err == nil || !strings.Contains(result.err.Error(), "current ref/head differs") {
			t.Fatalf("superseded receive admitted: %s %v", result.id, result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("superseded receive deadlocked")
	}
	records, err := f.m.ListPushNotifications()
	if err != nil || len(records) != 2 {
		t.Fatalf("lost superseded receive: %+v %v", records, err)
	}
	receiveID := ""
	for _, info := range records {
		if info.Pending {
			receiveID = info.ID
		}
	}
	record, err := f.m.readNotification(receiveID)
	if err != nil || record.Validated || !sameNotificationOptions(record.Params, *f.params) || record.PreviousRunID != f.predecessor {
		t.Fatalf("superseded context rewritten or marked: %+v %v", record, err)
	}
	if _, err := f.m.ReplayPushNotification(context.Background(), receiveID); err == nil {
		t.Fatal("stale original-ID replay admitted work")
	}
	prior, _ := f.d.GetRun(f.predecessor)
	if prior.Status != types.RunQueued {
		t.Fatal("stale receive cancelled predecessor")
	}
}
