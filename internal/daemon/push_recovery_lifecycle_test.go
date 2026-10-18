package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func recoveryPending(t *testing.T, f *pushRecoveryFixture) ipc.PushNotificationInfo {
	t.Helper()
	records, err := f.m.ListPushNotifications()
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Pending && record.Error == "" {
			return record
		}
	}
	t.Fatalf("no pending notification: %+v", records)
	return ipc.PushNotificationInfo{}
}

func reopenRecovery(t *testing.T, f *pushRecoveryFixture) {
	t.Helper()
	if err := f.d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(f.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	f.d = d
	f.m = NewRunManager(d, f.p, nil)
	h := newSchedHarness(1, false)
	h.tryAdmit("blocker")
	f.m.sched = h.slotScheduler
}

func injectAuditFailure(t *testing.T, f *pushRecoveryFixture) *sql.DB {
	t.Helper()
	writer, err := sql.Open("sqlite", f.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	if _, err := writer.Exec("CREATE TRIGGER reject_admission BEFORE INSERT ON owner_admission_events BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END"); err != nil {
		t.Fatal(err)
	}
	return writer
}

func TestPushRecoveryFreshInsertFailureReopensAndReplays(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	f.params.OwnerValidationError = "duplicate owner push option"
	f.params.RawPushOptions = []string{"no-mistakes.owner=QQ==", "no-mistakes.owner=Qg=="}
	writer := injectAuditFailure(t, f)
	_, err := f.m.HandlePushReceived(context.Background(), f.params)
	f.assertPreserved(t, err)
	pending := recoveryPending(t, f)
	inspected, err := f.m.InspectPushNotification(pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Params.OwnerValidationError != f.params.OwnerValidationError || len(inspected.Params.RawPushOptions) != 2 || inspected.Params.Old != f.params.Old || inspected.Params.New != f.params.New {
		t.Fatalf("original notification lost: %+v", inspected)
	}
	if _, err := writer.Exec("DROP TRIGGER reject_admission"); err != nil {
		t.Fatal(err)
	}
	reopenRecovery(t, f)
	id, err := f.m.ReplayPushNotification(context.Background(), pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := f.m.InspectPushNotification(pending.ID)
	if err != nil || admitted.Pending || admitted.RunID != id {
		t.Fatalf("not resolved: %+v %v", admitted, err)
	}
	run, err := f.d.GetRun(id)
	if err != nil || run == nil || run.Owner() != "" || run.Status != types.RunQueued {
		t.Fatalf("wrong run: %+v %v", run, err)
	}
	event, err := f.d.GetOwnerAdmission(id)
	if err != nil || event == nil || event.OwnerValidationError != f.params.OwnerValidationError || !strings.Contains(event.Warning, "#82") {
		t.Fatalf("lost original admission warning: %+v %v", event, err)
	}
	reopenRecovery(t, f)
	replay, err := f.m.ReplayPushNotification(context.Background(), pending.ID)
	if err != nil || replay != id {
		t.Fatalf("repeat replay: %s %v", replay, err)
	}
	rows, err := f.d.GetRunsByRepo("recovery")
	if err != nil || len(rows) != 1 {
		t.Fatalf("duplicate admission: %+v %v", rows, err)
	}
}

func TestPushRecoveryCrashWindows(t *testing.T) {
	for _, point := range []string{"retained_before_ack", "committed_before_ack"} {
		t.Run(point, func(t *testing.T) {
			f := newPushRecoveryFixture(t, false)
			crash := func() { panic("simulated request process loss") }
			if point == "retained_before_ack" {
				f.m.afterPushRetained = crash
			} else {
				f.m.afterPushCommitted = crash
			}
			crashed := false
			func() {
				defer func() {
					if recover() != nil {
						crashed = true
					}
				}()
				_, _ = f.m.HandlePushReceived(context.Background(), f.params)
			}()
			if !crashed {
				t.Fatal("crash seam not reached")
			}
			records, err := f.m.ListPushNotifications()
			if err != nil || len(records) != 1 {
				t.Fatalf("lost durable notification: %+v %v", records, err)
			}
			id := records[0].ID
			if records[0].Pending != (point == "retained_before_ack") {
				t.Fatalf("wrong acknowledgement state: %+v", records[0])
			}
			reopenRecovery(t, f)
			runID, err := f.m.ReplayPushNotification(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			second, err := f.m.ReplayPushNotification(context.Background(), id)
			if err != nil || second != runID {
				t.Fatalf("replay duplicated: %s %v", second, err)
			}
			rows, err := f.d.GetRunsByRepo("recovery")
			if err != nil || len(rows) != 1 {
				t.Fatalf("run count: %+v %v", rows, err)
			}
			event, err := f.d.GetOwnerAdmission(runID)
			if err != nil || event == nil {
				t.Fatalf("missing committed event: %+v %v", event, err)
			}
		})
	}
}

func TestPushRecoveryRefusesStaleCorruptAndNewerWork(t *testing.T) {
	for _, fault := range []string{"ref_moved", "newer_same_head_run", "corrupt", "gate_context"} {
		t.Run(fault, func(t *testing.T) {
			f := newPushRecoveryFixture(t, false)
			writer := injectAuditFailure(t, f)
			if _, err := f.m.HandlePushReceived(context.Background(), f.params); err == nil {
				t.Fatal("missing injected failure")
			}
			pending := recoveryPending(t, f)
			before, err := os.ReadFile(pending.Path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec("DROP TRIGGER reject_admission"); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "ref_moved":
				gitCmd(t, f.params.Gate, "update-ref", f.params.Ref, f.params.Old)
			case "newer_same_head_run":
				if _, err := f.d.InsertRun("recovery", "fresh", f.params.New, f.params.Old); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				before = []byte("{broken")
				if err := os.WriteFile(pending.Path, before, 0600); err != nil {
					t.Fatal(err)
				}
			case "gate_context":
				if _, err := f.d.UpdateRepoWorkingPath("recovery", t.TempDir()); err != nil {
					t.Fatal(err)
				}
			}
			oldRows, err := f.d.GetRunsByRepo("recovery")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.m.ReplayPushNotification(context.Background(), pending.ID); err == nil {
				t.Fatal("unsafe replay accepted")
			}
			after, err := os.ReadFile(pending.Path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("refused replay lost evidence: %v", err)
			}
			rows, err := f.d.GetRunsByRepo("recovery")
			if err != nil || len(rows) != len(oldRows) {
				t.Fatalf("refused replay mutated runs: %+v %v", rows, err)
			}
			for i := range rows {
				if rows[i].Status != oldRows[i].Status {
					t.Fatal("refused replay cancelled newer work")
				}
			}
		})
	}
}

func TestPushRecoveryRetentionStoreFailureIsNamed(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	if err := os.WriteFile(f.p.Root()+"/push-notifications", []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := f.m.HandlePushReceived(context.Background(), f.params)
	if err == nil || !strings.Contains(err.Error(), "retain post-receive notification failed") || !strings.Contains(err.Error(), "accepted Git ref may lack a run") {
		t.Fatalf("unnamed retention failure: %v", err)
	}
	rows, err := f.d.GetRunsByRepo("recovery")
	if err != nil || len(rows) != 0 {
		t.Fatalf("admitted without retention: %+v %v", rows, err)
	}
}

func TestPushRecoveryLegacyDiagnosticStaysPending(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	if err := os.WriteFile(f.p.ConfigFile(), []byte("agent: ["), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := f.m.HandlePushReceived(context.Background(), f.params); err == nil || !strings.Contains(err.Error(), "load global config") {
			t.Fatalf("configuration error: %v", err)
		}
	}
	pending := recoveryPending(t, f)
	if pending.DiagnosticRunID == "" {
		t.Fatal("legacy failed diagnostic not recorded")
	}
	rows, err := f.d.GetRunsByRepo("recovery")
	if err != nil || len(rows) != 1 || rows[0].Status != types.RunFailed {
		t.Fatalf("diagnostic rows: %+v %v", rows, err)
	}
	if event, err := f.d.GetOwnerAdmission(rows[0].ID); err != nil || event != nil {
		t.Fatalf("diagnostic falsely admitted: %+v %v", event, err)
	}
	if err := os.WriteFile(f.p.ConfigFile(), []byte("intent:\n  require_owner: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reopenRecovery(t, f)
	id, err := f.m.ReplayPushNotification(context.Background(), pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if id == pending.DiagnosticRunID {
		t.Fatal("historical diagnostic promoted to admission")
	}
	rows, err = f.d.GetRunsByRepo("recovery")
	if err != nil || len(rows) != 2 {
		t.Fatalf("diagnostic/admission rows: %+v %v", rows, err)
	}
	event, err := f.d.GetOwnerAdmission(id)
	if err != nil || event == nil {
		t.Fatalf("corrected config not admitted: %+v %v", event, err)
	}
}

func TestPushRecoverySuccessSupersedesOnlyPredecessor(t *testing.T) {
	for _, nonce := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "nonce"}[nonce], func(t *testing.T) {
			f := newPushRecoveryFixture(t, true)
			if nonce {
				f.params.LaunchNonce = "replace-nonce"
				f.params.ValidationGeneration = "replace-generation"
			}
			id, err := f.m.HandlePushReceived(context.Background(), f.params)
			if err != nil {
				t.Fatal(err)
			}
			previous, _ := f.d.GetRun(f.predecessor)
			current, _ := f.d.GetRun(id)
			if previous.Status != types.RunCancelled || current.Status != types.RunQueued {
				t.Fatalf("supersession: previous=%+v current=%+v", previous, current)
			}
			if _, ok := f.m.runs[id]; !ok {
				t.Fatal("replacement ownership lost")
			}
			replay, err := f.m.HandlePushReceived(context.Background(), f.params)
			if err != nil || replay != id {
				t.Fatalf("duplicate notification: %s %v", replay, err)
			}
			if nonce {
				f.params.RawPushOptions = []string{"same-nonce-redelivery"}
				duplicate, err := f.m.HandlePushReceived(context.Background(), f.params)
				if err != nil || duplicate != id {
					t.Fatalf("matching nonce reattachment: %s %v", duplicate, err)
				}
				notifications, err := f.m.ListPushNotifications()
				if err != nil {
					t.Fatal(err)
				}
				for _, notification := range notifications {
					if notification.Pending {
						t.Fatalf("reattachment not bound: %+v", notification)
					}
				}
			}
			current, _ = f.d.GetRun(id)
			if current.Status != types.RunQueued {
				t.Fatal("duplicate notification cancelled replacement")
			}
		})
	}
}

func TestPushRecoveryConfigRepairPreservesLiveHandleThenAdmits(t *testing.T) {
	for _, nonce := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "nonce"}[nonce], func(t *testing.T) {
			f := newPushRecoveryFixture(t, true)
			if nonce {
				f.params.LaunchNonce = "repair-nonce"
				f.params.ValidationGeneration = "repair-generation"
			}
			f.params.OwnerValidationError = "owner contains control characters"
			runCtx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			done := make(chan struct{})
			close(done)
			owned := f.m.runs[f.predecessor]
			owned.state = ownershipClaimed
			owned.handle = &runHandle{runCtx: runCtx, cancel: cancel, done: done}
			if err := os.WriteFile(f.p.ConfigFile(), []byte("intent: ["), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.m.HandlePushReceived(context.Background(), f.params); err == nil {
				t.Fatal("bad config admitted")
			}
			if runCtx.Err() != nil || f.m.runs[f.predecessor].handle != owned.handle {
				t.Fatal("bad config cancelled or replaced predecessor handle")
			}
			pending := recoveryPending(t, f)
			if err := os.WriteFile(f.p.ConfigFile(), []byte("intent:\n  require_owner: true\n"), 0600); err != nil {
				t.Fatal(err)
			}
			id, err := f.m.ReplayPushNotification(context.Background(), pending.ID)
			if err != nil {
				t.Fatal(err)
			}
			if runCtx.Err() == nil {
				t.Fatal("committed replacement failed to cancel predecessor")
			}
			current, err := f.d.GetRun(id)
			if err != nil || current == nil || current.Status != types.RunQueued {
				t.Fatalf("replacement: %+v %v", current, err)
			}
			event, err := f.d.GetOwnerAdmission(id)
			if err != nil || event == nil || event.OwnerValidationError != f.params.OwnerValidationError {
				t.Fatalf("lost warning: %+v %v", event, err)
			}
		})
	}
}
