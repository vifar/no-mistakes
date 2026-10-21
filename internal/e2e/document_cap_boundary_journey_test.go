//go:build e2e && !windows

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The documented cap bounds commits without waiving either a real gap or
// lint's separate duty. Exercise both boundaries through the running daemon.
func TestDocumentCapStrictGapRefusesApproval(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: documentJourneyScenarioWith(t, "", nil, documentJourneyCapGapAction())})
	commitTrustedRepoConfig(t, h, "document:\n  max_commits_per_run: 0\ncommands:\n  lint: 'true'\nno_ci: true\n")
	initFromOwnWorktree(t, h, "init-cap-strict-gap")
	const branch = "feature/cap-strict-gap"
	head := h.CommitChange(branch, "feature.txt", "feature\n", "feature")
	out, err := h.RunInDir(h.AddWorktree(branch), "axi", "run", "--shipping-mode", "full", "--intent", "keep real documentation gaps blocking at the commit cap", "--wait", "10s")
	if err != nil && !(strings.Contains(out, "launch_receipt:") && (strings.Contains(out, "elapsed") || strings.Contains(out, "context deadline exceeded"))) {
		t.Fatalf("strict launch: %v\n%s", err, out)
	}
	t.Logf("actual strict launch:\n%s", out)
	parked := waitForStepStatus(t, h, branch, types.StepDocument, types.StepStatusAwaitingApproval, 90*time.Second)
	if parked.Shipping == nil || parked.Shipping.Mode != types.ShippingFull {
		t.Fatal("run is not strict")
	}
	err = h.RespondError(parked.ID, types.StepDocument, types.ActionApprove)
	if err == nil || !strings.Contains(err.Error(), "unresolved actionable findings") {
		t.Fatalf("strict approval did not refuse the real gap: %v", err)
	}
	t.Logf("actual daemon approval refusal: %v", err)
	stillParked := h.RunInfo(parked.ID)
	step, ok := findStep(stillParked.Steps, types.StepDocument)
	if !ok || step.Status != types.StepStatusAwaitingApproval {
		t.Fatalf("refused approval released the gate: %+v", step)
	}
	capJourneyRestored(t, h, stillParked, head)
	documentJourneyStatus(t, h, parked.ID)
}

func TestDocumentCapCleanCheckLeavesLintToItsOwnPass(t *testing.T) {
	lint := map[string]any{
		"match": "Detect the linting and formatting tools for this project",
		"structured": map[string]any{
			"findings": []map[string]any{{"id": "remaining-lint", "severity": "warning", "file": "feature.txt", "action": "ask-user", "description": "formatting still needs attention"}},
			"summary": "unresolved lint duty",
		},
	}
	scenario := documentJourneyScenarioWith(t, "", nil, capJourneyCheck(documentJourneyAction("", "")), lint)
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	branch, head := documentJourneyStart(t, h, "document:\n  max_commits_per_run: 0\n")
	parked := waitForStepStatus(t, h, branch, types.StepLint, types.StepStatusAwaitingApproval, 90*time.Second)
	document, ok := findStep(parked.Steps, types.StepDocument)
	if !ok || document.Status != types.StepStatusCompleted {
		t.Fatalf("clean cap check did not complete Document: %+v", document)
	}
	step, _ := findStep(parked.Steps, types.StepLint)
	if step.FindingsJSON == nil || !strings.Contains(*step.FindingsJSON, "formatting still needs attention") {
		t.Fatalf("gap check swallowed lint duty: %+v", step)
	}
	lintCalls := 0
	for _, call := range h.AgentInvocations() {
		if strings.Contains(call.Prompt, "Detect the linting and formatting tools for this project") {
			lintCalls++
		}
	}
	if capJourneyCalls(h) != 1 || lintCalls != 1 {
		t.Fatalf("want one gap check and one independent lint pass: gap=%d lint=%d", capJourneyCalls(h), lintCalls)
	}
	capJourneyRestored(t, h, parked, head)
	documentJourneyStatus(t, h, parked.ID)
}
