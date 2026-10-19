//go:build e2e && !windows

package e2e

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Regression at 2b1f6cdf848f84cc0de7656b80c52714f654df05: sibling
// get_runs polling must name its unmet condition before the package alarm.
// The shared readiness waiter was also red at f5fcfcb23b73d9fddc3767c35a94238fd72ccdba.
func TestSiblingPollingReportsConditionBeforePackageTimeout(t *testing.T) {
	if mode := os.Getenv("NM_E2E_SIBLING_POLL_CHILD"); mode != "" {
		root, err := os.MkdirTemp("/tmp", "nm-sp-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		socket := paths.WithRoot(root).Socket()
		if strings.HasSuffix(mode, "unresponsive") || mode == "runs-unresponsive" {
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					go func() { defer conn.Close(); <-t.Context().Done() }()
				}
			}()
		} else {
			server := ipc.NewServer()
			server.Handle(ipc.MethodGetRun, func(context.Context, json.RawMessage) (interface{}, error) {
				return ipc.GetRunResult{Run: &ipc.RunInfo{ID: "deliberately-pending-run", Status: types.RunRunning, CIReady: false}}, nil
			})
			server.Handle(ipc.MethodGetRuns, func(context.Context, json.RawMessage) (interface{}, error) {
				return ipc.GetRunsResult{Runs: []ipc.RunInfo{{ID: "deliberately-pending-run", Branch: "feature/deliberately-pending", Status: types.RunRunning}}}, nil
			})
			done := make(chan error, 1)
			go func() { done <- server.Serve(socket) }()
			t.Cleanup(func() { server.Close(); <-done })
			ready := time.Now().Add(time.Second)
			for {
				if _, err := os.Stat(socket); err == nil {
					break
				}
				if time.Now().After(ready) {
					t.Fatal("fixture IPC server did not start")
				}
				time.Sleep(time.Millisecond)
			}
		}
		h := &Harness{t: t, NMHome: root, WorkDir: root}
		switch {
		case strings.HasPrefix(mode, "ready-"):
			h.WaitForCIReady("deliberately-pending-run", true, 90*time.Second)
		case strings.HasPrefix(mode, "step-"):
			waitForStepStatus(t, h, "feature/deliberately-pending", types.StepTest, types.StepStatusCompleted, 90*time.Second)
		case strings.HasPrefix(mode, "run-"):
			waitForRunIDStatus(t, h, "deliberately-pending-run", types.RunCompleted, 90*time.Second)
		default:
			h.Runs()
		}
		t.Fatal("fixture unexpectedly satisfied its condition")
	}
	for _, mode := range []string{"ready-responsive", "ready-unresponsive", "step-responsive", "run-responsive", "step-unresponsive", "run-unresponsive", "runs-unresponsive"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSiblingPollingReportsConditionBeforePackageTimeout$", "-test.timeout=3s", "-test.v")
			cmd.Env = append(os.Environ(), "NM_E2E_SIBLING_POLL_CHILD="+mode)
			out, err := cmd.CombinedOutput()
			condition := "deliberately-pending-run"
			if strings.HasPrefix(mode, "step-") {
				condition = "feature/deliberately-pending"
			}
			if mode == "runs-unresponsive" {
				condition = "get runs:"
			}
			if ctx.Err() != nil || err == nil || !strings.Contains(string(out), condition) || strings.Contains(string(out), "panic: test timed out") {
				t.Fatalf("%s must report its named condition before the package alarm: err=%v context=%v\n%s", mode, err, ctx.Err(), out)
			}
		})
	}
}
