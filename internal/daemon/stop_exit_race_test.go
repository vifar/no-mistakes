package daemon

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// A daemon draining many runs can outlast the stop timeout and then exit just
// as the PID fallback inspects it. `ps` then fails for the vanished PID; that
// is a finished stop, not an error that aborts a restart.
func TestWaitForDaemonStopTreatsDaemonExitDuringFallbackAsStopped(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	const pid = 424244
	startedAt := time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)
	writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: pid, StartedAt: startedAt})
	if err := os.WriteFile(p.Socket(), []byte("draining"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_TEST_DAEMON_STOP_TIMEOUT", "300ms")
	exitAt := time.Now().Add(300 * time.Millisecond)

	var gone atomic.Bool
	oldHealthCheck := daemonHealthCheck
	oldProcessRunning := daemonProcessRunning
	oldProcessStartTime := daemonProcessStartTime
	oldKill := daemonKillPID
	daemonHealthCheck = func(*paths.Paths, ...time.Duration) (bool, error) { return false, nil }
	daemonProcessRunning = func(checkPID int) (bool, error) {
		if checkPID != pid {
			t.Fatalf("daemonProcessRunning pid = %d, want %d", checkPID, pid)
		}
		return !gone.Load(), nil
	}
	daemonProcessStartTime = func(checkPID int) (time.Time, error) {
		if time.Now().Before(exitAt) {
			return startedAt, nil
		}
		// The daemon exits between the last poll and this inspection.
		gone.Store(true)
		return time.Time{}, fmt.Errorf("exit status 1")
	}
	daemonKillPID = func(killPID int) error {
		t.Fatalf("killed pid %d of a daemon that already exited", killPID)
		return nil
	}
	t.Cleanup(func() {
		daemonHealthCheck = oldHealthCheck
		daemonProcessRunning = oldProcessRunning
		daemonProcessStartTime = oldProcessStartTime
		daemonKillPID = oldKill
	})

	if err := waitForDaemonStop(p, daemonInstance{pid: pid, startedAt: startedAt}); err != nil {
		t.Fatalf("waitForDaemonStop() = %v, want nil once the daemon has exited", err)
	}
	if _, err := os.Stat(p.PIDFile()); !os.IsNotExist(err) {
		t.Fatalf("pid file of the exited daemon should be removed, stat err = %v", err)
	}
}
