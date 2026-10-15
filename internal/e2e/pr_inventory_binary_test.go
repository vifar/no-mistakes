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

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The generated PR-agent prompt is an owned interface. A large inventory
// must remain within the existing 32 KiB native prompt contract, retain the
// authored modification, and supply an executable way to recover every path.
func TestLargePRInventoryOnRealBinary(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	state := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/large-pr-inventory"
	h.CommitChange(branch, "README.md", "# authored modification\n", "modify authored README")
	assets := filepath.Join(h.WorkDir, "assets")
	if err := os.MkdirAll(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{"README.md": true}
	for i := range 1200 {
		name := fmt.Sprintf("%04d-%s.js", i, strings.Repeat("asset-long-name-", 6))
		if err := os.WriteFile(filepath.Join(assets, name), []byte("export const value = 1;\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		expected["assets/"+name] = true
	}
	for _, args := range [][]string{{"add", "assets"}, {"commit", "-m", "add large asset inventory"}} {
		if out, err := h.runGit(t.Context(), h.WorkDir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	h.PushToGate(branch)
	defer func() {
		if active := h.ActiveRun(branch); active != nil {
			h.CancelRun(active.ID)
		}
	}()
	run := waitForStepStatus(t, h, branch, types.StepPR, types.StepStatusCompleted, 120*time.Second)
	var prompt string
	for _, invocation := range h.AgentInvocations() {
		if strings.Contains(invocation.Prompt, "Draft a pull request title and summary for the full branch delta.") {
			prompt = invocation.Prompt
			break
		}
	}
	if prompt == "" || len(prompt) > 32*1024 || !strings.Contains(prompt, "M\tREADME.md") {
		t.Fatalf("PR prompt lost the authored anchor or exceeded the wire budget: bytes=%d", len(prompt))
	}
	var recovery string
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, "git -C ") && strings.Contains(line, " diff --name-status ") {
			recovery = line
			break
		}
	}
	if recovery == "" {
		t.Fatal("bounded prompt supplied no full-inventory recovery command")
	}
	// Execute the command delivered to the agent. The independent oracle is
	// the set of paths created above, not another call to the prompt builder.
	output, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", recovery).CombinedOutput()
	if err != nil {
		t.Fatalf("delivered recovery command: %v\n%s", err, output)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		_, name, ok := strings.Cut(line, "\t")
		if !ok || !expected[name] {
			t.Fatalf("recovery returned an unexpected path: %q", line)
		}
		delete(expected, name)
	}
	if len(expected) != 0 {
		t.Fatalf("recovery omitted %d known changed paths", len(expected))
	}
	if pr := readStatefulGH(t, state).PRs[branch]; pr == nil || pr.Body == "" {
		t.Fatal("real PR step did not publish a body")
	}
	if dir := os.Getenv("NM_INVENTORY_EVIDENCE_DIR"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "live-pr-inventory-prompt.txt"), []byte(prompt), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("real run=%s published a PR; generated native prompt=%d bytes; authored README anchor retained; delivered shell command recovered all 1201 independently known paths", run.ID, len(prompt))
}
