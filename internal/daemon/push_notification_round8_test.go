package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestNotificationValidationMarkerFailurePreservesOriginal(t *testing.T) {
	f := newPushRecoveryFixture(t, true)
	ctx := context.Background()
	repo, err := f.d.GetRepo("recovery")
	if err != nil {
		t.Fatal(err)
	}
	retained, err := f.m.retainPush(f.params, repo)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(f.m.notificationPath(retained.ID))
	if err != nil {
		t.Fatal(err)
	}
	checked := false
	f.m.beforeNotificationRename = func() error {
		saved, err := f.m.readNotification(retained.ID)
		if err != nil || saved.Validated {
			t.Fatalf("old final not intact before atomic replacement: %+v %v", saved, err)
		}
		rows, err := f.d.GetRunsByRepo("recovery")
		if err != nil || len(rows) != 1 || rows[0].ID != f.predecessor || rows[0].Status != types.RunQueued {
			t.Fatalf("marker write followed mutation: %+v %v", rows, err)
		}
		entries, err := os.ReadDir(filepath.Dir(f.m.notificationPath(retained.ID)))
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".notification-") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(filepath.Dir(f.m.notificationPath(retained.ID)), entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var temp pushNotification
			if err := json.Unmarshal(raw, &temp); err != nil {
				t.Fatal(err)
			}
			if !temp.Validated || temp.ReceiveID != retained.ID || !sameNotificationOptions(temp.Params, retained.Record.Params) || temp.Checksum != notificationChecksum(&temp) {
				t.Fatalf("invalid staged marker: %+v", temp)
			}
			found = true
		}
		if !found {
			t.Fatal("no complete staged marker")
		}
		checked = true
		return errors.New("injected marker rename failure")
	}
	if _, err := f.m.ReplayPushNotification(ctx, retained.ID); err == nil || !strings.Contains(err.Error(), "persist notification validation: injected marker rename failure") {
		t.Fatalf("marker failure not named: %v", err)
	}
	if !checked {
		t.Fatal("marker write boundary not reached")
	}
	after, err := os.ReadFile(f.m.notificationPath(retained.ID))
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("failed marker update damaged original notification")
	}
	reopenRecovery(t, f)
	saved, err := f.m.readNotification(retained.ID)
	if err != nil || saved.Validated {
		t.Fatalf("interrupted update fabricated provenance: %+v %v", saved, err)
	}
	id, err := f.m.ReplayPushNotification(ctx, retained.ID)
	if err != nil {
		t.Fatal(err)
	}
	marked, err := f.m.readNotification(retained.ID)
	if err != nil || !marked.Validated || marked.ReceiveID != retained.ID || !sameNotificationOptions(saved.Params, marked.Params) {
		t.Fatalf("successful retry lost original identity/options: %+v %v", marked, err)
	}
	markedBytes, err := os.ReadFile(f.m.notificationPath(retained.ID))
	if err != nil {
		t.Fatal(err)
	}
	reopenRecovery(t, f)
	again, err := f.m.ReplayPushNotification(ctx, retained.ID)
	if err != nil || again != id {
		t.Fatalf("marked reopen replay duplicated run: %s %v", again, err)
	}
	finalBytes, err := os.ReadFile(f.m.notificationPath(retained.ID))
	if err != nil || !bytes.Equal(markedBytes, finalBytes) {
		t.Fatal("bound replay rewrote validation provenance")
	}
}

func TestNotificationValidationMarkerPrecedesAdmissionFaults(t *testing.T) {
	for _, fault := range []string{"config", "insert"} {
		t.Run(fault, func(t *testing.T) {
			f := newPushRecoveryFixture(t, true)
			writer, err := sql.Open("sqlite", f.p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if fault == "config" {
				if err := os.WriteFile(f.p.ConfigFile(), []byte("intent: [broken"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := writer.Exec("CREATE TRIGGER marker_insert_fault BEFORE INSERT ON runs BEGIN SELECT RAISE(ABORT, 'marker insert fault'); END"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.m.HandlePushReceived(context.Background(), f.params); err == nil {
				t.Fatal("admission fault did not fire")
			}
			pending := recoveryPending(t, f)
			saved, err := f.m.readNotification(pending.ID)
			if err != nil || !saved.Validated || saved.ReceiveID != pending.ID {
				t.Fatalf("admission fault preceded durable marker: %+v %v", saved, err)
			}
			prior, err := f.d.GetRun(f.predecessor)
			if err != nil || prior.Status != types.RunQueued {
				t.Fatalf("fault cancelled predecessor: %+v %v", prior, err)
			}
			rows, err := f.d.GetRunsByRepo("recovery")
			if err != nil || len(rows) != 1 {
				t.Fatalf("fault inserted replacement: %+v %v", rows, err)
			}
			before, err := os.ReadFile(f.m.notificationPath(pending.ID))
			if err != nil {
				t.Fatal(err)
			}
			reopenRecovery(t, f)
			record, err := f.m.readNotification(pending.ID)
			if err != nil || !record.Validated {
				t.Fatalf("marker lost on reopen: %+v %v", record, err)
			}
			if fault == "insert" {
				if _, err := writer.Exec("DROP TRIGGER marker_insert_fault"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(f.p.ConfigFile(), []byte("intent:\n  require_owner: true\n"), 0600); err != nil {
				t.Fatal(err)
			}
			id, err := f.m.ReplayPushNotification(context.Background(), pending.ID)
			if err != nil || id == "" {
				t.Fatalf("marked receive could not recover: %s %v", id, err)
			}
			after, err := os.ReadFile(f.m.notificationPath(pending.ID))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("replay rewrote original marker/parameters")
			}
		})
	}
}

func TestNotificationUnmarkedLegacyIsNotTransitionProof(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			f := newPushRecoveryFixture(t, false)
			ctx := context.Background()
			first, err := f.m.HandlePushReceived(ctx, f.params)
			if err != nil {
				t.Fatal(err)
			}
			gitCmd(t, f.params.Gate, "commit", "--allow-empty", "-m", "unmarked C")
			headC := gitOutput(t, f.params.Gate, "rev-parse", "HEAD")
			repo, err := f.d.GetRepo("recovery")
			if err != nil {
				t.Fatal(err)
			}
			params := *f.params
			params.Old, params.New = f.params.New, headC
			retained, err := f.m.retainPush(&params, repo)
			if err != nil {
				t.Fatal(err)
			}
			record := *retained.Record
			record.Version = version
			if version != 3 {
				record.ReceiveID = ""
			}
			id := notificationID(&record)
			if id != retained.ID {
				if err := os.Remove(f.m.notificationPath(retained.ID)); err != nil {
					t.Fatal(err)
				}
			}
			record.Checksum = notificationChecksum(&record)
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.m.notificationPath(id), raw, 0600); err != nil {
				t.Fatal(err)
			}
			returning := *f.params
			returning.Old = headC
			if _, err := f.m.HandlePushReceived(ctx, &returning); err == nil {
				t.Fatal("unmarked record licensed a new validation")
			}
			after, err := os.ReadFile(f.m.notificationPath(id))
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatal("unknown legacy history was backfilled or modified")
			}
			prior, err := f.d.GetRun(first)
			if err != nil || prior.Status != types.RunQueued {
				t.Fatalf("unknown history cancelled predecessor: %+v %v", prior, err)
			}
		})
	}
}

func TestNotificationValidationMarkerInterruptedBeforeRename(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	repo, err := f.d.GetRepo("recovery")
	if err != nil {
		t.Fatal(err)
	}
	retained, err := f.m.retainPush(f.params, repo)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(f.m.notificationPath(retained.ID))
	if err != nil {
		t.Fatal(err)
	}
	f.m.beforeNotificationRename = func() error { panic("simulated marker-update interruption") }
	interrupted := false
	func() {
		defer func() {
			if recover() != nil {
				interrupted = true
			}
		}()
		_, _ = f.m.ReplayPushNotification(context.Background(), retained.ID)
	}()
	if !interrupted {
		t.Fatal("marker interruption boundary not reached")
	}
	reopenRecovery(t, f)
	old, err := os.ReadFile(f.m.notificationPath(retained.ID))
	if err != nil || !bytes.Equal(original, old) {
		t.Fatal("interruption left a torn final record")
	}
	saved, err := f.m.readNotification(retained.ID)
	if err != nil || saved.Validated {
		t.Fatalf("interruption falsely marked validation: %+v %v", saved, err)
	}
	rows, err := f.d.GetRunsByRepo("recovery")
	if err != nil || len(rows) != 0 {
		t.Fatalf("interrupted marker admitted work: %+v %v", rows, err)
	}
	id, err := f.m.ReplayPushNotification(context.Background(), retained.ID)
	if err != nil || id == "" {
		t.Fatalf("original receive could not recover: %s %v", id, err)
	}
	saved, err = f.m.readNotification(retained.ID)
	if err != nil || !saved.Validated || saved.ReceiveID != retained.ID {
		t.Fatalf("recovery lost original record: %+v %v", saved, err)
	}
}
