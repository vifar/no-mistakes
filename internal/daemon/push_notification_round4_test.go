package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestNotificationReceiveCycleCreatesNewAdmissions(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	first, err := f.m.HandlePushReceived(context.Background(), f.params)
	if err != nil {
		t.Fatal(err)
	}
	reverse := *f.params
	reverse.Old, reverse.New = f.params.New, f.params.Old
	gitCmd(t, reverse.Gate, "update-ref", reverse.Ref, reverse.New, reverse.Old)
	second, err := f.m.HandlePushReceived(context.Background(), &reverse)
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.params.Gate, "update-ref", f.params.Ref, f.params.New, f.params.Old)
	third, err := f.m.HandlePushReceived(context.Background(), f.params)
	if err != nil {
		t.Fatal(err)
	}
	if third == first || third == second || first == second {
		t.Fatalf("distinct receives reused admission: first=%s second=%s third=%s", first, second, third)
	}
	for _, id := range []string{first, second} {
		run, _ := f.d.GetRun(id)
		if run.Status != types.RunCancelled {
			t.Fatalf("predecessor not superseded: %+v", run)
		}
	}
	current, _ := f.d.GetRun(third)
	if current.Status != types.RunQueued {
		t.Fatalf("replacement cancelled: %+v", current)
	}
	duplicate, err := f.m.HandlePushReceived(context.Background(), f.params)
	if err != nil || duplicate != third {
		t.Fatalf("redelivery duplicated: %s %v", duplicate, err)
	}
	records, err := f.m.ListPushNotifications()
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.RunID == first || record.RunID == third {
			replay, err := f.m.ReplayPushNotification(context.Background(), record.ID)
			if err != nil || replay != record.RunID {
				t.Fatalf("exact replay changed identity: %s %v", replay, err)
			}
		}
	}
	current, _ = f.d.GetRun(third)
	if current.Status != types.RunQueued {
		t.Fatal("historical replay cancelled newer work")
	}
	rows, err := f.d.GetRunsByRepo("recovery")
	if err != nil || len(rows) != 3 {
		t.Fatalf("duplicate run rows: %+v %v", rows, err)
	}
}

func notificationIPCClient(t *testing.T, f *pushRecoveryFixture) *ipc.Client {
	t.Helper()
	server := ipc.NewServer()
	registerHandlers(server, f.m, f.d, func() {})
	dir, err := os.MkdirTemp("", "nm-page-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := server.Listen(paths.WithRoot(dir).Socket()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.ServeReady() }()
	t.Cleanup(func() { server.Close(); <-done })
	client, err := ipc.Dial(paths.WithRoot(dir).Socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestNotificationListTransportPagination(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	intent := strings.Repeat("x", 49000)
	rawOption := "no-mistakes.intent=" + base64.StdEncoding.EncodeToString([]byte(intent))
	for i := 0; i < 12; i++ {
		p := *f.params
		p.Ref = fmt.Sprintf("refs/heads/history-%02d", i)
		p.Intent = intent
		p.RawPushOptions = []string{rawOption}
		gitCmd(t, p.Gate, "update-ref", p.Ref, p.New)
		if _, err := f.m.HandlePushReceived(context.Background(), &p); err != nil {
			t.Fatal(err)
		}
	}
	writer := injectAuditFailure(t, f)
	f.params.Intent = intent
	f.params.RawPushOptions = []string{rawOption}
	if _, err := f.m.HandlePushReceived(context.Background(), f.params); err == nil {
		t.Fatal("missing pending fixture")
	}
	pending := recoveryPending(t, f)
	corruptID := strings.Repeat("f", 64)
	corruptPath := f.m.notificationPath(corruptID)
	if err := os.WriteFile(corruptPath, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	client := notificationIPCClient(t, f)
	seen := map[string]bool{}
	after := ""
	pendingFound := false
	corruptFound := false
	for page := 0; page < 20; page++ {
		var raw json.RawMessage
		if err := client.Call(ipc.MethodListPushNotifications, map[string]any{"after": after, "limit": 5}, &raw); err != nil {
			t.Fatalf("list discovery failed over real IPC: %v", err)
		}
		if len(raw) >= 128<<10 {
			t.Fatalf("oversized list page: %d", len(raw))
		}
		var records []struct {
			ID         string          `json:"id"`
			Pending    bool            `json:"pending"`
			Error      string          `json:"error"`
			Params     json.RawMessage `json:"params"`
			NextCursor string          `json:"next_cursor"`
		}
		if err := json.Unmarshal(raw, &records); err != nil {
			t.Fatal(err)
		}
		if len(records) > 5 {
			t.Fatalf("page limit ignored: %d", len(records))
		}
		for _, record := range records {
			if seen[record.ID] {
				t.Fatalf("duplicate discovery: %s", record.ID)
			}
			seen[record.ID] = true
			if len(record.Params) != 0 && string(record.Params) != "null" {
				t.Fatal("list exported full original payload")
			}
			if record.ID == pending.ID && record.Pending {
				pendingFound = true
			}
			if record.ID == corruptID && record.Error != "" {
				corruptFound = true
			}
		}
		if len(records) == 0 || records[len(records)-1].NextCursor == "" {
			break
		}
		after = records[len(records)-1].NextCursor
	}
	if len(seen) != 14 || !pendingFound || !corruptFound {
		t.Fatalf("records missing: count=%d pending=%v corrupt=%v", len(seen), pendingFound, corruptFound)
	}
	var inspected ipc.PushNotificationInfo
	if err := client.Call(ipc.MethodInspectPushNotification, &ipc.PushNotificationParams{ID: pending.ID}, &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.Params == nil || inspected.Params.Intent != intent || len(inspected.Params.RawPushOptions) != 1 || inspected.Params.RawPushOptions[0] != rawOption {
		t.Fatal("inspect lost full original payload")
	}
	if _, err := writer.Exec("DROP TRIGGER reject_admission"); err != nil {
		t.Fatal(err)
	}
	var replay ipc.PushReceivedResult
	if err := client.Call(ipc.MethodReplayPushNotification, &ipc.PushNotificationParams{ID: pending.ID}, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.RunID == "" {
		t.Fatal("discovered pending ID could not resolve")
	}
}

func TestNotificationOwnedStartupCleansOnlyTemps(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	writer := injectAuditFailure(t, f)
	if _, err := f.m.HandlePushReceived(context.Background(), f.params); err == nil {
		t.Fatal("pending fixture not retained")
	}
	if _, err := writer.Exec("DROP TRIGGER reject_admission"); err != nil {
		t.Fatal(err)
	}
	pending := recoveryPending(t, f)
	before, err := os.ReadFile(pending.Path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(pending.Path)
	temp, err := os.CreateTemp(dir, ".notification-")
	if err != nil {
		t.Fatal(err)
	}
	tempPath := temp.Name()
	temp.WriteString("killed writer")
	temp.Close()
	keep := map[string][]byte{strings.Repeat("e", 64) + ".json": []byte("corrupt final"), "unrelated": []byte("keep"), ".notification-unrelated": []byte("keep too")}
	for name, data := range keep {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	tempDir := filepath.Join(dir, ".notification-777")
	if err := os.Mkdir(tempDir, 0700); err != nil {
		t.Fatal(err)
	}
	unrelatedLink := filepath.Join(dir, ".notification-888")
	if err := os.Symlink(filepath.Join(dir, "unrelated"), unrelatedLink); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireSingletonLock(f.p)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	cfg := config.DefaultGlobalConfig()
	cfg.WorktreeRoots = map[string]string{f.params.Gate: filepath.Join(f.p.Root(), "invalid-placement")}
	err = runWithOptionsLocked(f.p, f.d, cfg, nil, time.Now())
	if err == nil || !strings.Contains(err.Error(), "placement is unusable") {
		t.Fatalf("startup preflight did not stop before services: %v", err)
	}
	if _, err := os.Lstat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("owned startup left killed-writer temp: %v", err)
	}
	for name, data := range keep {
		actual, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(actual, data) {
			t.Fatalf("cleanup touched non-temp %s: %v", name, err)
		}
	}
	actual, err := os.ReadFile(pending.Path)
	if err != nil || !bytes.Equal(actual, before) {
		t.Fatalf("cleanup changed pending bytes: %v", err)
	}
	if info, err := os.Lstat(tempDir); err != nil || !info.IsDir() {
		t.Fatalf("cleanup touched unrelated directory: %v", err)
	}
	if info, err := os.Lstat(unrelatedLink); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("cleanup touched symlink: %v", err)
	}
	if _, err := f.m.ReplayPushNotification(context.Background(), pending.ID); err != nil {
		t.Fatalf("cleanup lost replay: %v", err)
	}
}

func TestNotificationStartupRefusesInvalidJournal(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	if err := os.WriteFile(filepath.Join(f.p.Root(), "push-notifications"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireSingletonLock(f.p)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	cfg := config.DefaultGlobalConfig()
	cfg.WorktreeRoots = map[string]string{f.params.Gate: filepath.Join(f.p.Root(), "invalid-placement")}
	err = runWithOptionsLocked(f.p, f.d, cfg, nil, time.Now())
	if err == nil || !strings.Contains(err.Error(), "clean push notification temporary files") {
		t.Fatalf("cleanup failure not named: %v", err)
	}
}

func TestNotificationSecondOwnerCannotCleanLiveTemps(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	dir := filepath.Join(f.p.Root(), "push-notifications")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	temp, err := os.CreateTemp(dir, ".notification-")
	if err != nil {
		t.Fatal(err)
	}
	temp.Close()
	lock, err := acquireSingletonLock(f.p)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := RunWithOptions(f.p, f.d, nil); err == nil {
		t.Fatal("second startup acquired owned root")
	}
	if _, err := os.Stat(temp.Name()); err != nil {
		t.Fatalf("unowned startup removed live temp: %v", err)
	}
}

func TestNotificationLegacyPendingSurvivesIdentityUpgrade(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	repo, err := f.d.GetRepo("recovery")
	if err != nil {
		t.Fatal(err)
	}
	record := &pushNotification{Version: 1, RepoID: repo.ID, WorkingPath: repo.WorkingPath, GateIdentity: canonicalRoot(f.params.Gate), Params: *f.params}
	record.Checksum = notificationChecksum(record)
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	id := notificationID(record)
	path := f.m.notificationPath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	reopenRecovery(t, f)
	admitted, err := f.m.HandlePushReceived(context.Background(), f.params)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.m.InspectPushNotification(id)
	if err != nil || info.Pending || info.RunID != admitted {
		t.Fatalf("legacy pending identity lost: %+v %v", info, err)
	}
	reopenRecovery(t, f)
	duplicate, err := f.m.HandlePushReceived(context.Background(), f.params)
	if err != nil || duplicate != admitted {
		t.Fatalf("legacy commit replay duplicated: %s %v", duplicate, err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, raw) {
		t.Fatalf("legacy durable record rewritten: %v", err)
	}
}

func TestNotificationLateRedeliveryCannotCancelNewerSameHeadWork(t *testing.T) {
	f := newPushRecoveryFixture(t, false)
	first, err := f.m.HandlePushReceived(context.Background(), f.params)
	if err != nil {
		t.Fatal(err)
	}
	records, err := f.m.ListPushNotifications()
	if err != nil || len(records) != 1 {
		t.Fatalf("first receipt missing: %+v %v", records, err)
	}
	originalID := records[0].ID
	receipt, err := f.m.HandleStartFreshRun(context.Background(), &ipc.StartFreshRunParams{RepoID: "recovery", Branch: "fresh", HeadSHA: f.params.New, Intent: "new validation of same head", Owner: "Firstmate", LaunchNonce: "newer-same-head", ValidationGeneration: "newer-generation"})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RunID == first {
		t.Fatal("fresh fixture reused old run")
	}
	if _, err := f.m.HandlePushReceived(context.Background(), f.params); err == nil {
		t.Fatal("late ordinary redelivery admitted over newer same-head work")
	}
	if replay, err := f.m.ReplayPushNotification(context.Background(), originalID); err != nil || replay != first {
		t.Fatalf("exact older replay: %s %v", replay, err)
	}
	current, _ := f.d.GetRun(receipt.RunID)
	if current.Status != types.RunQueued {
		t.Fatal("redelivery cancelled newer work")
	}
	rows, err := f.d.GetRunsByRepo("recovery")
	if err != nil || len(rows) != 2 {
		t.Fatalf("redelivery created work: %+v %v", rows, err)
	}
}
