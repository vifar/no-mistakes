//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The owner's R1 contract: a new acknowledgement cannot hide a known P1.
// The oracle is the persisted, human-routed CI finding, not the forge input.
func TestCIScopeCodexPendingKeepsActionableThreadJourney(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setTrusted("ci:\n  review_bot_comments: always\n")
	j.setForge(forgeState{
		Checks:          []forgeCheck{greenCheck("build")},
		Protection:      protectionJSON("build"),
		ReviewThreads:   []forgeThread{{Author: "chatgpt-codex-connector[bot]", Path: "store.go", Line: 73, Body: "![P1 Badge](https://img.shields.io/badge/P1-orange) Lost update"}},
		ReviewSummaries: []json.RawMessage{json.RawMessage(`{"id":7,"body":"@codex review","created_at":"2026-10-21T12:00:00Z"}`)},
		ReviewReactions: map[string][]json.RawMessage{"7": {json.RawMessage(`{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"eyes"}`)}},
	})
	j.init()
	branch := "feature/codex-pending-actionable-thread"
	j.push(branch)
	run, _ := j.expectGate(branch)
	ci, _ := findStep(run.Steps, types.StepCI)
	findings, err := types.ParseFindingsJSON(deref(ci.FindingsJSON))
	if err != nil || len(findings.Items) != 1 {
		t.Fatalf("pending lost the known finding: %+v err=%v", findings, err)
	}
	f := findings.Items[0]
	if f.File != "store.go" || f.Line != 73 || f.Severity != "error" || f.Action != "ask-user" || f.Category != "ci-review-bot" || !strings.Contains(f.Description, "P1 Badge") || len(j.ciRepairPrompts()) != 0 {
		t.Fatalf("pending changed the finding or dispatched a repair: %+v repairs=%v", f, j.ciRepairPrompts())
	}
	j.finish(branch)
}

// The owner's R2 contract: the H1 request stays settled after an unrelated H2
// push, but that settlement cannot cover a different H2 request. Both provider
// completion shapes and invalid provider attribution go through the real CLI.
func TestCIScopeCodexRequestIdentityAndHistoryJourney(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setTrusted("ci:\n  review_bot_comments: always\n")
	base := forgeState{Checks: []forgeCheck{greenCheck("build")}, Protection: protectionJSON("build")}
	j.setForge(base)
	j.init()
	branch := "feature/codex-request-history"
	wt, run, _ := j.strictRun(branch)
	const old = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const bot = "chatgpt-codex-connector[bot]"
	request := json.RawMessage(`{"id":7,"body":"@codex review","created_at":"2026-10-21T12:00:00Z"}`)
	newRequest := json.RawMessage(`{"id":8,"body":"@codex review","created_at":"2026-10-21T12:03:00Z"}`)
	eyes := json.RawMessage(`{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"eyes"}`)
	push := func(before, after, timestamp string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"ref":%q,"before":%q,"after":%q,"pushed_at":%q}`, "refs/heads/"+branch, before, after, timestamp))
	}
	history := []json.RawMessage{
		push(strings.Repeat("0", 40), old, "2026-10-21T11:00:00Z"),
		push(old, run.HeadSHA, "2026-10-21T12:02:00Z"),
	}
	for _, summary := range []bool{false, true} {
		for _, tc := range []struct {
			name, wantError string
			newerRequest    bool
			history         []json.RawMessage
		}{
			{name: "historical-request-settled", history: history},
			{name: "newer-request-not-settled", history: history, newerRequest: true, wantError: "review bot review is pending"},
			{name: "missing-history-waits", wantError: "review bot review is pending"},
			{name: "malformed-history-fails-closed", history: []json.RawMessage{json.RawMessage(`{"ref":"refs/heads/invalid"}`)}, wantError: "invalid review request head history"},
			{name: "disconnected-history-fails-closed", history: []json.RawMessage{history[0], push(strings.Repeat("c", 40), run.HeadSHA, "2026-10-21T12:02:00Z")}, wantError: "ambiguous review request head history"},
		} {
			t.Run(fmt.Sprintf("summary-%v/%s", summary, tc.name), func(t *testing.T) {
				forge := base
				forge.ReviewPushes = tc.history
				forge.ReviewSummaries = []json.RawMessage{request}
				forge.ReviewReactions = map[string][]json.RawMessage{"7": {eyes}, "8": {eyes}}
				if summary {
					forge.ReviewSummaries = append(forge.ReviewSummaries, json.RawMessage(fmt.Sprintf("{\"user\":{\"login\":%q},\"body\":\"**Reviewed commit:** `bbbbbbbbbb`\",\"created_at\":\"2026-10-21T12:04:00Z\"}", bot)))
				} else {
					forge.Reviews = []json.RawMessage{codexActivity(bot, old, "COMMENTED", "2026-10-21T12:04:00Z")}
				}
				if tc.newerRequest {
					forge.ReviewSummaries = append(forge.ReviewSummaries, newRequest)
				}
				j.setForge(forge)
				out, err := j.receipt(wt, run.ID)
				if tc.wantError == "" {
					if err != nil || !strings.Contains(out, `"phase":"ci-ready"`) {
						t.Fatalf("completed H1 request resurrected: %v\n%s", err, out)
					}
				} else if err == nil || !strings.Contains(out, tc.wantError) {
					t.Fatalf("expected refusal %q, got %v\n%s", tc.wantError, err, out)
				}
			})
		}
	}
	j.setForge(base)
	j.finish(branch)
}
