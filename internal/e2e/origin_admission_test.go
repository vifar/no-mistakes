//go:build e2e && !windows

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Authentication alone cannot admit a provider that cannot supply the full
// PR comparison. The external CLI supplies only an authenticated status;
// admission and its refusal come from the real binary and daemon.
func TestOriginAdmissionWithoutCompleteFactsOnRealBinary(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	const remote = "https://origin.cursor.com/codebase/owner/repo.git"
	configureGitURLRewrite(t, h, remote, h.UpstreamDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", remote); err != nil {
		t.Fatalf("set Origin remote: %v\n%s", err, out)
	}
	cliLog := filepath.Join(h.HomeDir, "origin-calls.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(cliLog) + "\n" +
		"if [ \"$1\" = auth ] && [ \"$2\" = status ]; then exit 0; fi\nexit 9\n"
	if err := os.WriteFile(filepath.Join(h.BinDir, "origin"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/origin-without-facts"
	h.CommitChange(branch, "feature.txt", "origin admission probe\n", "add Origin admission probe")
	h.PushToGate(branch)
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunFailed || run.Error == nil || !strings.Contains(*run.Error, "provider cannot read complete pull request facts") {
		t.Fatalf("Origin without full facts was admitted: status=%s error=%s", run.Status, deref(run.Error))
	}
	if invocations := h.AgentInvocations(); len(invocations) != 0 {
		t.Fatalf("refused provider invoked an agent: %+v", invocations)
	}
	calls, err := os.ReadFile(cliLog)
	if err != nil || !strings.Contains(string(calls), "auth status") {
		t.Fatalf("authentication boundary was not driven: %q, %v", calls, err)
	}
	t.Logf("authenticated Origin admission refused: %s; agent turns: none; external CLI calls:\n%s", *run.Error, calls)
}
