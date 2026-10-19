//go:build e2e && !windows

package e2e

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// R1 regression at 59a2502e1ee2b735e61dda3de9768ee85c9e93cf: an
// accepted socket with no reply must not outlive the poll's diagnostic budget.
func TestHarnessUnresponsiveRPCReportsBeforePackageTimeout(t *testing.T) {
	if mode := os.Getenv("NM_E2E_UNRESPONSIVE_CHILD"); mode != "" {
		// Leave room for the socket name under a workspace-local TMPDIR on
		// macOS, whose Unix-domain socket paths are limited to 104 bytes.
		root, err := os.MkdirTemp("", "nm-w-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		listener, err := net.Listen("unix", paths.WithRoot(root).Socket())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			<-t.Context().Done()
		}()
		h := &Harness{t: t, NMHome: root}
		if mode == "wait" {
			h.WaitForRun("feature/unresponsive-daemon", 90*time.Second)
		} else {
			h.RunInfo("unresponsive-run")
		}
		return
	}
	for _, mode := range []string{"wait", "run-info"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHarnessUnresponsiveRPCReportsBeforePackageTimeout$", "-test.timeout=3s", "-test.v")
			cmd.Env = append(os.Environ(), "NM_E2E_UNRESPONSIVE_CHILD="+mode)
			out, err := cmd.CombinedOutput()
			condition := "no run found for branch feature/unresponsive-daemon within"
			if mode == "run-info" {
				condition = "get run unresponsive-run:"
			}
			if ctx.Err() != nil || err == nil || !strings.Contains(string(out), condition) || strings.Contains(string(out), "panic: test timed out") {
				t.Fatalf("unresponsive RPC must fail with its condition before package alarm: err=%v context=%v\n%s", err, ctx.Err(), out)
			}
		})
	}
}
