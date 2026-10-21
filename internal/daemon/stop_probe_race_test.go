package daemon

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestWaitForDaemonStopPostKillProbe(t *testing.T) {
	for _, scenario := range []string{"exited between probes", "probe failed while alive", "still running", "recycled before kill"} {
		t.Run(scenario, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			t.Setenv("NM_TEST_DAEMON_STOP_TIMEOUT", "1ns")
			const pid = 424244
			started := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
			writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: pid, StartedAt: started})
			oldRunning, oldStart, oldKill, oldHealth := daemonProcessRunning, daemonProcessStartTime, daemonKillPID, daemonHealthCheck
			t.Cleanup(func() {
				daemonProcessRunning, daemonProcessStartTime, daemonKillPID, daemonHealthCheck = oldRunning, oldStart, oldKill, oldHealth
			})
			killed, postProbes, startProbes := false, 0, 0
			probeErr := errors.New("process state probe access denied")
			daemonHealthCheck = func(*paths.Paths, ...time.Duration) (bool, error) { return false, nil }
			daemonProcessRunning = func(int) (bool, error) {
				if !killed {
					return true, nil
				}
				postProbes++
				switch scenario {
				case "exited between probes":
					if postProbes == 1 {
						return false, errors.New("exit status 1")
					}
					return false, nil
				case "probe failed while alive":
					return false, probeErr
				default:
					return true, nil
				}
			}
			daemonProcessStartTime = func(int) (time.Time, error) {
				startProbes++
				if scenario == "recycled before kill" && startProbes >= 2 {
					return started.Add(time.Minute), nil
				}
				return started, nil
			}
			daemonKillPID = func(int) error { killed = true; return nil }
			err := waitForDaemonStop(p, daemonInstance{pid: pid, startedAt: started})
			switch scenario {
			case "exited between probes":
				if err != nil || !killed || postProbes < 2 {
					t.Fatalf("stopped daemon: killed=%v probes=%d err=%v", killed, postProbes, err)
				}
				if _, err := os.Stat(p.PIDFile()); !os.IsNotExist(err) {
					t.Fatalf("stale pid file: %v", err)
				}
			case "probe failed while alive":
				if !errors.Is(err, probeErr) || !strings.Contains(err.Error(), "424244") {
					t.Fatalf("lost real probe failure: %v", err)
				}
			case "still running":
				if err == nil || !strings.Contains(err.Error(), "still running after kill") {
					t.Fatalf("unstopped daemon accepted: %v", err)
				}
			case "recycled before kill":
				if killed || err != nil {
					t.Fatalf("recycled pid killed=%v err=%v", killed, err)
				}
			}
		})
	}
}

func TestDaemonInstanceExitDoesNotAdoptRecycledPID(t *testing.T) {
	oldRunning, oldStart := daemonProcessRunning, daemonProcessStartTime
	t.Cleanup(func() { daemonProcessRunning, daemonProcessStartTime = oldRunning, oldStart })
	started := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	daemonProcessRunning = func(int) (bool, error) { return true, nil }
	daemonProcessStartTime = func(int) (time.Time, error) { return started.Add(time.Minute), nil }
	exited, err := (daemonInstance{pid: 424244, startedAt: started}).exited()
	if err != nil || !exited {
		t.Fatalf("recycled pid retained as daemon: exited=%v err=%v", exited, err)
	}
}
