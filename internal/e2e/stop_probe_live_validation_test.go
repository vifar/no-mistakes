//go:build e2e && !windows

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Keep the real foreground daemon as our child, so its post-SIGKILL zombie
// remains available for the state probe until this test explicitly reaps it.
// The oracle is the CLI contract: stop succeeds on confirmed absence, but
// refuses a failed or still-running probe. No daemon function is substituted.
func TestDaemonStopLivePostKillProbe(t *testing.T) {
	for _, mode := range []string{"exit-during-probe", "probe-error", "reports-running", "zombie"} {
		t.Run(mode, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{})
			cmd := exec.Command(h.NMBin, "daemon", "run", "--root", h.NMHome)
			cmd.Dir, cmd.Env = h.WorkDir, os.Environ()
			log, err := os.Create(filepath.Join(h.NMHome, "foreground.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			cmd.Stdout, cmd.Stderr = log, log
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			pid := cmd.Process.Pid
			reap := make(chan struct{})
			done := make(chan error, 1)
			var once sync.Once
			release := func() { once.Do(func() { close(reap) }) }
			go func() { <-reap; done <- cmd.Wait() }()
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				release()
				<-done
			})
			p := paths.WithRoot(h.NMHome)
			var keeper *ipc.Client
			deadline := time.Now().Add(45 * time.Second)
			for time.Now().Before(deadline) {
				client, err := ipc.Dial(p.Socket())
				if err == nil {
					var health ipc.HealthResult
					if client.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, time.Second) == nil {
						keeper = client
						break
					}
					client.Close()
				}
				time.Sleep(20 * time.Millisecond)
			}
			if keeper == nil {
				t.Fatal("foreground daemon did not become healthy")
			}
			defer keeper.Close()
			h.syncDaemonOwnership()
			dir := t.TempDir()
			marker := filepath.Join(dir, "post-kill")
			ack := filepath.Join(dir, "reaped")
			t.Cleanup(func() { _ = os.WriteFile(ack, []byte("cleanup"), 0600) })
			count := filepath.Join(dir, "count")
			script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = '-p' ] && [ "$2" = '%d' ] && [ "$4" = 'stat=' ] && [ ! -S %s ]; then
 n=0
 [ ! -f %s ] || n=$(cat %s)
 n=$((n+1))
 echo "$n" > %s
 if [ "$n" -ge 3 ]; then
  actual=$(/bin/ps "$@")
  case "$actual" in *Z*) ;; *) echo "expected killed zombie, got $actual" >&2; exit 2;; esac
  echo "$actual" > %s
  case %s in
   exit-during-probe)
    i=0
    while [ ! -f %s ] && [ "$i" -lt 1000 ]; do sleep 0.05; i=$((i+1)); done
    echo 'state probe raced the actual daemon exit' >&2
    exit 2;;
   probe-error) echo 'isolated probe access denied' >&2; exit 2;;
   reports-running) echo R; exit 0;;
  esac
 fi
fi
exec /bin/ps "$@"
`, pid, shellQuote(p.Socket()), shellQuote(count), shellQuote(count), shellQuote(count), shellQuote(marker), shellQuote(mode), shellQuote(ack))
			if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			released := make(chan error, 1)
			if mode == "exit-during-probe" {
				go func() {
					deadline := time.Now().Add(75 * time.Second)
					for time.Now().Before(deadline) {
						if _, err := os.Stat(marker); err == nil {
							release()
							// cmd.Wait is the reaping authority; its result remains
							// available to the cleanup through the replacement send.
							waitErr := <-done
							done <- waitErr
							released <- os.WriteFile(ack, []byte("reaped"), 0600)
							return
						}
						time.Sleep(10 * time.Millisecond)
					}
					released <- fmt.Errorf("post-kill state probe not reached")
				}()
			}
			out, stopErr := h.RunInDirWithEnv(h.WorkDir, map[string]string{"PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "NM_TEST_DAEMON_STOP_TIMEOUT": "1ns"}, "daemon", "stop", "--force")
			if mode == "exit-during-probe" {
				if err := <-released; err != nil {
					t.Fatalf("%v; CLI error=%v, output=%s", err, stopErr, out)
				}
			}
			if data, err := os.ReadFile(marker); err != nil || !strings.Contains(string(data), "Z") {
				t.Fatalf("did not reach a real post-kill zombie probe: %q %v\n%s", data, err, out)
			}
			switch mode {
			case "probe-error":
				if stopErr == nil || !strings.Contains(out, "after kill") || !strings.Contains(out, "isolated probe access denied") || !strings.Contains(out, fmt.Sprint(pid)) {
					t.Fatalf("probe failure lost: %v\n%s", stopErr, out)
				}
			case "reports-running":
				if stopErr == nil || !strings.Contains(out, "still running after kill") {
					t.Fatalf("unconfirmed stop accepted: %v\n%s", stopErr, out)
				}
			default:
				if stopErr != nil || !strings.Contains(out, "daemon stopped") {
					t.Fatalf("confirmed stop failed: %v\n%s", stopErr, out)
				}
			}
			t.Logf("mode=%s, actual post-kill state=Z, pid=%d, stop error=%v, CLI output:\n%s", mode, pid, stopErr, out)
		})
	}
}

// Inject the OS identity change precisely on the last pre-kill reread. The
// protected PID must remain alive; comparing the captured timestamp to itself
// instead would SIGKILL this real, keeper-blocked fixture daemon. Because the
// actual fixture still holds the root lock, the full CLI must also refuse to
// claim success, even though the injected identity says the old process exited.
func TestDaemonStopLiveFinalIdentityGuard(t *testing.T) {
	h := NewHarness(t, SetupOpts{})
	statusCommand(t, h, h.HomeDir, "daemon", "start")
	pid := daemonPIDForRoot(t, h)
	p := paths.WithRoot(h.NMHome)
	keeper, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer keeper.Close()
	var health ipc.HealthResult
	if err := keeper.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, time.Second); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	count := filepath.Join(dir, "identity-reads")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = '-p' ] && [ "$2" = '%d' ] && [ "$4" = 'lstart=' ] && [ ! -S %s ]; then
 n=0
 [ ! -f %s ] || n=$(cat %s)
 n=$((n+1))
 echo "$n" > %s
 if [ "$n" -ge 2 ]; then
  echo 'Sun Jan 1 00:00:00 2040'
  exit 0
 fi
fi
exec /bin/ps "$@"
`, pid, shellQuote(p.Socket()), shellQuote(count), shellQuote(count), shellQuote(count))
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	out, err := h.RunInDirWithEnv(h.HomeDir, map[string]string{"PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "NM_TEST_DAEMON_STOP_TIMEOUT": "1ns"}, "daemon", "stop", "--force")
	if err == nil || !strings.Contains(out, "already running for this NM_HOME") {
		t.Fatalf("held root was falsely reported stopped: %v\n%s", err, out)
	}
	if data, err := os.ReadFile(count); err != nil || strings.TrimSpace(string(data)) != "2" {
		t.Fatalf("final identity reread not exercised: %q %v", data, err)
	}
	if alive, err := e2edaemon.ProcessAlive(pid); err != nil || !alive {
		t.Fatalf("protected PID %d was targeted: alive=%v err=%v", pid, alive, err)
	}
	t.Logf("CLI refused: %s; changed identity injected on final OS reread; protected pid %d remains alive", strings.TrimSpace(out), pid)
}

func TestDaemonStopLiveFailedFixtureCleanup(t *testing.T) {
	nmBin, _, _ := buildBinaries(t)
	marker := filepath.Join(t.TempDir(), "fixture")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDaemonStopLiveFailedFixtureChild$", "-test.v")
	cmd.Env = append(os.Environ(), "NM_STOP_LIVE_FAILURE_CHILD="+marker, "NM_STOP_LIVE_CHILD_BIN_DIR="+filepath.Dir(nmBin))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected real stop probe failure: %s", out)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("fixture marker missing: %v\n%s", err, out)
	}
	parts := strings.SplitN(string(data), "\n", 2)
	if len(parts) != 2 {
		t.Fatalf("invalid fixture marker: %q", data)
	}
	root := parts[0]
	defer os.RemoveAll(root)
	if owners, err := e2edaemon.FindDaemonsForRoot(filepath.Join(root, "h")); err != nil || len(owners) != 0 {
		t.Fatalf("failed fixture cleanup left daemon owners: %v %v", owners, err)
	}
	for path, want := range map[string]string{
		"logs/daemon.log":                "shutdown",
		"logs/cli.log":                   "stop",
		"diagnostics/daemon.pid":         parts[1],
		"diagnostics/command-output.log": "isolated live probe access denied",
	} {
		data, err := os.ReadFile(filepath.Join(root, "h", path))
		if err != nil || !strings.Contains(string(data), want) {
			t.Errorf("real diagnostic %s lost after cleanup: %q %v\n%s", path, data, err, out)
		}
		if evidence := os.Getenv("NM_STOP_LIVE_EVIDENCE"); evidence != "" {
			dest := filepath.Join(evidence, "failed-live-fixture", path)
			if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dest, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Logf("real stop failed with probe stderr; pre-command pid %s and daemon/CLI logs survived statusCommand failure and actual cleanup; no daemon remains", parts[1])
}

func TestDaemonStopLiveFailedFixtureChild(t *testing.T) {
	marker := os.Getenv("NM_STOP_LIVE_FAILURE_CHILD")
	if marker == "" {
		t.Skip("intentional failure subprocess only")
	}
	// Reuse the exact binaries already built by the parent, rather than
	// spending the child's failure/cleanup budget rebuilding three CLIs.
	if dir := os.Getenv("NM_STOP_LIVE_CHILD_BIN_DIR"); dir != "" {
		buildOnce.Do(func() {
			builtNM = filepath.Join(dir, executableName("no-mistakes"))
			builtFake = filepath.Join(dir, executableName("fakeagent"))
			builtCLI = filepath.Join(dir, executableName("fakecli"))
		})
	}
	var keeper *ipc.Client
	t.Cleanup(func() {
		if keeper != nil {
			keeper.Close()
		}
	})
	h := NewHarness(t, SetupOpts{})
	statusCommand(t, h, h.HomeDir, "daemon", "start")
	pid := daemonPIDForRoot(t, h)
	if err := os.WriteFile(marker, []byte(fmt.Sprintf("%s\n%d", filepath.Dir(h.NMHome), pid)), 0600); err != nil {
		t.Fatal(err)
	}
	p := paths.WithRoot(h.NMHome)
	var err error
	keeper, err = ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	var health ipc.HealthResult
	if err := keeper.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, time.Second); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = '-p' ] && [ \"$2\" = '%d' ] && [ \"$4\" = 'stat=' ] && [ ! -S %s ]; then\n echo 'isolated live probe access denied' >&2\n exit 2\nfi\nexec /bin/ps \"$@\"\n", pid, shellQuote(p.Socket()))
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NM_TEST_DAEMON_STOP_TIMEOUT", "1ns")
	statusCommand(t, h, h.HomeDir, "daemon", "stop", "--force")
}

func TestDaemonStopLiveSuccessfulFixtureCleanup(t *testing.T) {
	var root string
	t.Run("fixture", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{})
		root = filepath.Dir(h.NMHome)
		statusCommand(t, h, h.HomeDir, "daemon", "start")
	})
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("successful fixture root remains: %s: %v", root, err)
	}
	if owners, err := e2edaemon.FindDaemonsForRoot(filepath.Join(root, "h")); err != nil || len(owners) != 0 {
		t.Fatalf("successful fixture cleanup left daemon owners: %v %v", owners, err)
	}
	t.Log("successful fixture cleanup removed its root and left no daemon owner")
}
