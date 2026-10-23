//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the real command and Git hook beyond the old 60s and 30s limits.
// The delays model scheduling pressure at external process boundaries; the
// binary, Git transport, post-receive hook, and daemon remain real.
func TestLoadedRunnerHarnessBudgets(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	realBinary := h.NMBin
	delayed := filepath.Join(h.BinDir, "delayed-no-mistakes")
	script := "#!/bin/sh\nsleep 65\nexec " + shellQuote(realBinary) + " \"$@\"\n"
	if err := os.WriteFile(delayed, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h.NMBin = delayed
	started := time.Now()
	out, err := h.Run("--version")
	h.NMBin = realBinary
	if err != nil {
		t.Fatalf("command delayed beyond the old 60s budget: %v\n%s", err, out)
	}
	if time.Since(started) < 65*time.Second || !strings.Contains(out, "no-mistakes") {
		t.Fatalf("delayed real version command did not complete: %s", out)
	}
	t.Logf("real CLI completed after %s: %s", time.Since(started), out)
	gate, err := h.runGit(context.Background(), h.WorkDir, "remote", "get-url", "no-mistakes")
	if err != nil {
		t.Fatalf("resolve disposable gate: %v\n%s", err, gate)
	}
	hook := filepath.Join(strings.TrimSpace(string(gate)), "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nsleep 35\ncat >/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	const branch = "feature/loaded-runner-budget"
	want := h.CommitChange(branch, "budget.txt", "loaded runner\n", "exercise loaded runner budgets")
	started = time.Now()
	h.PushToGate(branch)
	got, err := h.runGit(context.Background(), strings.TrimSpace(string(gate)), "rev-parse", "refs/heads/"+branch)
	if err != nil || strings.TrimSpace(string(got)) != want || time.Since(started) < 35*time.Second {
		t.Fatalf("delayed push did not publish the submitted head: %v\n%s", err, got)
	}
	t.Logf("real gate push completed after %s; gate ref is %s", time.Since(started), got)
}
