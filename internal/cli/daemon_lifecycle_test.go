package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDaemonStopRefusesWithActiveRunsAndListsThem(t *testing.T) {
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop")
	if err == nil {
		t.Fatal("daemon stop should refuse while active runs exist")
	}
	if stopCalled {
		t.Fatal("daemon stop should not stop the daemon after refusing")
	}
	for _, want := range []string{
		"refusing daemon stop",
		"2 active pipeline runs",
		"feature-a",
		"aaa111",
		"feature-b",
		"bbb222",
		"--force",
	} {
		if !strings.Contains(out+err.Error(), want) {
			t.Fatalf("daemon stop refusal should contain %q, got output %q error %v", want, out, err)
		}
	}
}

func TestDaemonStopForceOverridesActiveRunGuard(t *testing.T) {
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

	stopCalled := false
	prevStop := daemonStopFn
	daemonStopFn = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	t.Cleanup(func() { daemonStopFn = prevStop })

	out, err := executeCmd("daemon", "stop", "--force")
	if err != nil {
		t.Fatalf("daemon stop --force failed: %v\n%s", err, out)
	}
	if !stopCalled {
		t.Fatal("daemon stop --force should stop the daemon")
	}
	if !strings.Contains(out, "FORCE: daemon stop") {
		t.Fatalf("force output should be loud, got %q", out)
	}
}

func TestDaemonRestartRefusesWithActiveRuns(t *testing.T) {
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

	stopCalled := false
	startCalled := false
	prevStop := daemonStopFn
	prevStart := daemonStartFn
	daemonStopFn = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	daemonStartFn = func(*paths.Paths) error {
		startCalled = true
		return nil
	}
	t.Cleanup(func() {
		daemonStopFn = prevStop
		daemonStartFn = prevStart
	})

	out, err := executeCmd("daemon", "restart")
	if err == nil {
		t.Fatal("daemon restart should refuse while active runs exist")
	}
	if stopCalled || startCalled {
		t.Fatalf("daemon restart should not stop/start after refusing; stop=%t start=%t", stopCalled, startCalled)
	}
	if !strings.Contains(out+err.Error(), "refusing daemon restart") || !strings.Contains(out+err.Error(), "feature-a") {
		t.Fatalf("daemon restart refusal should list active runs, got output %q error %v", out, err)
	}
}

// TestDaemonStartRefusesToRestartForDriftWithActiveRuns covers #1425: a
// running daemon whose service definition drifted is restarted by
// `daemon start`, so that restart must pass the same active-run guard.
func TestDaemonStartRefusesToRestartForDriftWithActiveRuns(t *testing.T) {
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

	restarted := false
	prevStart := daemonStartGuardedFn
	daemonStartGuardedFn = func(_ *paths.Paths, beforeRestart func() error) error {
		if err := beforeRestart(); err != nil {
			return err
		}
		restarted = true
		return nil
	}
	t.Cleanup(func() { daemonStartGuardedFn = prevStart })

	out, err := executeCmd("daemon", "start")
	if err == nil {
		t.Fatal("daemon start should refuse to restart while active runs exist")
	}
	if restarted {
		t.Fatal("daemon start should not restart the daemon after refusing")
	}
	for _, want := range []string{"refusing daemon start", "2 active pipeline runs", "feature-a", "feature-b", "--force"} {
		if !strings.Contains(out+err.Error(), want) {
			t.Fatalf("daemon start refusal should contain %q, got output %q error %v", want, out, err)
		}
	}

	out, err = executeCmd("daemon", "start", "--force")
	if err != nil {
		t.Fatalf("daemon start --force failed: %v\n%s", err, out)
	}
	if !restarted {
		t.Fatal("daemon start --force should restart the daemon")
	}
	if !strings.Contains(out, "FORCE: daemon start") {
		t.Fatalf("force output should be loud, got %q", out)
	}
	data, err := os.ReadFile(filepath.Join(nmHome, "logs", "cli.log"))
	if err != nil || !strings.Contains(string(data), "lifecycle FORCE command=daemon.start") {
		t.Fatalf("cli.log should record the forced restart, got %q (err %v)", data, err)
	}
}

func TestLifecycleCommandsWriteCallerAttributionToCLILog(t *testing.T) {
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)

	prevStop := daemonStopFn
	prevStart := daemonStartFn
	prevUninstall := daemonUninstallFn
	daemonStopFn = func(*paths.Paths) error { return nil }
	daemonStartFn = func(*paths.Paths) error { return nil }
	daemonUninstallFn = func(*paths.Paths) (string, error) { return "", nil }
	prevSupported := daemonUninstallSupportedFn
	daemonUninstallSupportedFn = func() bool { return true }
	t.Cleanup(func() {
		daemonUninstallSupportedFn = prevSupported
		daemonStopFn = prevStop
		daemonStartFn = prevStart
		daemonUninstallFn = prevUninstall
	})

	out, err := executeCmd("daemon", "stop", "--force")
	if err != nil {
		t.Fatalf("daemon stop --force failed: %v\n%s", err, out)
	}
	out, err = executeCmd("daemon", "restart", "--force")
	if err != nil {
		t.Fatalf("daemon restart --force failed: %v\n%s", err, out)
	}
	out, err = executeCmd("update", "--force")
	if err != nil {
		t.Fatalf("update --force failed: %v\n%s", err, out)
	}
	out, err = executeCmd("daemon", "uninstall", "--force")
	if err != nil {
		t.Fatalf("daemon uninstall --force failed: %v\n%s", err, out)
	}

	data, err := os.ReadFile(filepath.Join(nmHome, "logs", "cli.log"))
	if err != nil {
		t.Fatalf("read cli.log: %v", err)
	}
	log := string(data)
	for _, want := range []string{
		"lifecycle FORCE command=daemon.stop",
		"lifecycle FORCE command=daemon.restart",
		"lifecycle FORCE command=daemon.uninstall",
		"lifecycle FORCE command=update",
		"force=true",
		"pid=",
		"ppid=",
		"parent_cmdline=",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("cli.log should contain %q, got %q", want, log)
		}
	}
}

func createLifecycleGuardRuns(t *testing.T, p *paths.Paths) {
	t.Helper()
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	repo, err := database.InsertRepo("/tmp/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := database.InsertRun(repo.ID, "feature-a", "aaa111", "000"); err != nil {
		t.Fatalf("insert pending run: %v", err)
	}
	running, err := database.InsertRun(repo.ID, "feature-b", "bbb222", "000")
	if err != nil {
		t.Fatalf("insert running run: %v", err)
	}
	if err := database.UpdateRunStatus(running.ID, types.RunRunning); err != nil {
		t.Fatalf("mark running: %v", err)
	}
}

// A forced restart whose stop step reports an error must not silently leave no
// daemon: once the old daemon no longer answers, it starts the new one, and
// while the old one still answers it fails loudly and starts nothing.
func TestDaemonRestartForceAfterStopError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		oldAlive  bool
		wantStart bool
	}{
		{name: "old daemon gone", oldAlive: false, wantStart: true},
		{name: "old daemon still answering", oldAlive: true, wantStart: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nmHome := makeSocketSafeTempDir(t)
			t.Setenv("NM_HOME", nmHome)
			createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

			startCalled := false
			prevStop, prevStart, prevRunning := daemonStopFn, daemonStartFn, daemonIsRunningFn
			daemonStopFn = func(*paths.Paths) error {
				return errors.New("inspect daemon pid 3973: exit status 1")
			}
			daemonStartFn = func(*paths.Paths) error {
				startCalled = true
				return nil
			}
			daemonIsRunningFn = func(*paths.Paths) (bool, error) { return tc.oldAlive, nil }
			t.Cleanup(func() {
				daemonStopFn, daemonStartFn, daemonIsRunningFn = prevStop, prevStart, prevRunning
			})

			out, err := executeCmd("daemon", "restart", "--force")
			if startCalled != tc.wantStart {
				t.Fatalf("start called = %t, want %t (output %q, error %v)", startCalled, tc.wantStart, out, err)
			}
			if tc.wantStart {
				if err != nil {
					t.Fatalf("daemon restart --force = %v, want the new daemon started\n%s", err, out)
				}
				if !strings.Contains(out, "inspect daemon pid 3973") {
					t.Fatalf("restart should still report the stop error, got %q", out)
				}
				return
			}
			if err == nil {
				t.Fatalf("daemon restart --force should fail while the old daemon still answers, got output %q", out)
			}
			for _, want := range []string{"stop daemon", "inspect daemon pid 3973", "may still be running", "no new daemon was started"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("restart error should contain %q, got %v", want, err)
				}
			}
		})
	}
}
