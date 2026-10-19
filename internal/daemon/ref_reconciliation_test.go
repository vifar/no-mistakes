package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/refreconcile"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestStartupReconciliationReportsRefMovedBeforeNotificationRename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("crash fixture uses POSIX shell; ref evidence matrix is platform independent")
	}
	root, err := os.MkdirTemp("", "nmh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	repo, oldHead := setupTestGitRepo(t, p, d, "crash-repo")
	seed, err := d.InsertRun(repo.ID, "main", oldHead, oldHead)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(seed.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	shell := filepath.Join(t.TempDir(), "test-shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nexec env -0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)
	t.Setenv("NM_TEST_START_DAEMON", "1")
	t.Setenv("NM_DAEMON_HELPER_PROCESS", "daemon")
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "20s")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "10ms")
	pid := 0
	t.Cleanup(func() {
		if pid != 0 {
			shutdownIsolatedDaemon(t, p, pid)
		}
	})
	start := func() {
		t.Helper()
		if err := startDetachedDaemon(p); err != nil {
			t.Fatalf("start isolated daemon: %v\n%s", err, detachedDaemonStartupDiagnostics(p))
		}
		pid, err = ReadPID(p)
		if err != nil {
			t.Fatal(err)
		}
	}
	start()
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "accepted but not recorded")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gate := p.RepoDir(repo.ID)
	gitCmd(t, gate, "fetch", repo.WorkingPath, "HEAD")
	// Reproduce the durable state at the #66 pre-rename crash window. No
	// push_received call has run and no metadata is fabricated from the ref.
	gitCmd(t, gate, "update-ref", "refs/heads/lost-receive", head)
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil { // SIGKILL on POSIX, no graceful drain.
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		running, err := daemonProcessRunning(pid)
		if err == nil && !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("killed isolated daemon did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pid = 0
	if _, err := os.Stat(filepath.Join(p.Root(), "push-notifications")); !os.IsNotExist(err) {
		t.Fatalf("fixture unexpectedly retained notification: %v", err)
	}
	if runs, err := d.GetRunsByRepoHead(repo.ID, "lost-receive", head); err != nil || len(runs) != 0 {
		t.Fatalf("fixture unexpectedly recorded run: %+v %v", runs, err)
	}
	start() // Production Run() path performs reconciliation before IPC readiness.
	issues := refreconcile.Inspect(t.Context(), d, p, repo)
	if len(issues) != 1 || issues[0].Code != refreconcile.OrphanedRef || issues[0].Ref != "refs/heads/lost-receive" || issues[0].Head != head {
		t.Fatalf("status did not expose exact orphan: %+v", issues)
	}
	log, err := os.ReadFile(filepath.Join(p.Root(), "logs", "ref-reconciliation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var event struct {
		At time.Time `json:"at"`
		refreconcile.Issue
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(log))), &event); err != nil {
		t.Fatalf("invalid event log: %v: %s", err, log)
	}
	if event.At.IsZero() || event.Code != refreconcile.OrphanedRef || event.RepoID != repo.ID || event.Ref != issues[0].Ref || event.Head != head {
		t.Fatalf("event log lost orphan identity: %+v", event)
	}
	lifecycle, err := os.ReadFile(p.DaemonLog())
	if err != nil || !strings.Contains(string(lifecycle), refreconcile.OrphanedRef) {
		t.Fatalf("lifecycle log missing named orphan: %v: %s", err, lifecycle)
	}
	t.Logf("SIGKILL before notification rename; restarted daemon reports %s ref=%s head=%s in status and fsynced event log", event.Code, event.Ref, event.Head)
}

func TestStartupReconciliationEventFailureIsNamed(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	setupTestGitRepo(t, p, d, "unrecorded")
	// A directory where the event file belongs forces a real filesystem error.
	if err := os.Mkdir(filepath.Join(p.Root(), "logs", "ref-reconciliation.jsonl"), 0755); err != nil {
		t.Fatal(err)
	}
	err = reconcileRefsOnStartup(d, p)
	if err == nil || !strings.HasPrefix(err.Error(), refreconcile.Failed+":") {
		t.Fatalf("event write failure silently allowed startup: %v", err)
	}
}
