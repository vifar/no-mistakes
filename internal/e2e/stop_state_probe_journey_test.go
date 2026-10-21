//go:build e2e && !windows

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestDaemonForceStopWithRealExitDuringStateProbe(t *testing.T) {
	h := NewHarness(t, SetupOpts{})
	statusCommand(t, h, h.WorkDir, "init")
	pid := daemonPIDForRoot(t, h)
	realPS, err := exec.LookPath("ps")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	inspecting := filepath.Join(dir, "inspecting")
	result := filepath.Join(dir, "result")
	keeper, err := ipc.Dial(paths.WithRoot(h.NMHome).Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer keeper.Close()
	var health ipc.HealthResult
	if err := keeper.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		defer keeper.Close()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(inspecting); err == nil {
				released <- nil
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		released <- fmt.Errorf("stat probe never began")
	}()
	// This is the real ps no-match result after a real daemon exit. The
	// keeper prevents exit until processRunning has reached its stat probe.
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = '-p' ] && [ "$2" = '%d' ] && [ "$4" = 'stat=' ] && [ ! -S %s ]; then
 touch %s
 i=0
 while [ "$i" -lt 500 ]; do
  %s "$@" > %s.out 2> %s.err
  rc=$?
  if [ "$rc" -ne 0 ]; then
   echo "$rc" > %s
   cat %s.out
   cat %s.err >&2
   exit "$rc"
  fi
  i=$((i+1))
  sleep 0.01
 done
 echo 'daemon remained alive during state probe' >&2
 exit 2
fi
exec %s "$@"
`, pid, shellQuote(paths.WithRoot(h.NMHome).Socket()), shellQuote(inspecting), shellQuote(realPS), shellQuote(result), shellQuote(result), shellQuote(result), shellQuote(result), shellQuote(result), shellQuote(realPS))
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	out, stopErr := h.RunInDirWithEnv(h.WorkDir, map[string]string{"PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "NM_TEST_DAEMON_STOP_TIMEOUT": "1ns"}, "daemon", "stop", "--force")
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if stopErr != nil || !strings.Contains(out, "daemon stopped") {
		t.Fatalf("stop failed: %v\n%s", stopErr, out)
	}
	data, err := os.ReadFile(result)
	if err != nil || strings.TrimSpace(string(data)) != "1" {
		t.Fatalf("did not exercise ps no-match: %q %v", data, err)
	}
	if alive, err := e2edaemon.ProcessAlive(pid); err != nil || alive {
		t.Fatalf("stop returned before pid %d exited: alive=%v err=%v", pid, alive, err)
	}
	t.Logf("real ps -o stat= exited 1; --force succeeded; pid %d is gone: %s", pid, strings.TrimSpace(out))
}
