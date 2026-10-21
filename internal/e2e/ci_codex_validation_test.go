//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Oracles: the documented comment policy and issue #42's severity/routing
// contract. These journeys reject green readiness over unexcluded threads,
// accidental cross-bot exclusions, and automatically fixable review findings.
func TestCIScopeCodexBadgeAndExclusionJourney(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		codexRed     bool
		wantFiles    []string
	}{
		{"native-badges", "ci:\n  review_bot_comments: always\n", false, []string{"P0.go", "P1.go", "P2.go", "P3.go", "greptile.go"}},
		{"informational-does-not-exclude", "ci:\n  review_bot_comments: always\n  required_checks: auto\n", true, []string{"P0.go", "P1.go", "P2.go", "P3.go", "greptile.go"}},
		{"exclude-codex-only", "ci:\n  review_bot_comments: always\n  external_checks: [Codex Review]\n", false, []string{"greptile.go"}},
		{"exclude-greptile-only", "ci:\n  review_bot_comments: always\n  external_checks: [Greptile Review]\n", false, []string{"P0.go", "P1.go", "P2.go", "P3.go"}},
		{"on-failure-reads-failed-bot", "ci:\n  review_bot_comments: on_failure\n  required_checks: all\n", true, []string{"P0.go", "P1.go", "P2.go", "P3.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := newScopeJourney(t, SetupOpts{Agent: "claude"})
			j.setTrusted(tc.config)
			checks := []forgeCheck{greenCheck("build")}
			if tc.codexRed {
				c := redCheck("Codex Review")
				c.AppSlug, c.AppID = "chatgpt-codex-connector", 99
				checks = append(checks, c)
			}
			threads := []forgeThread{{Author: "greptile-apps[bot]", Path: "greptile.go", Line: 8, Body: "![P1 Badge](https://img.shields.io/badge/P1-orange) Greptile stays warning"}, {Author: "unknown-app[bot]", Path: "unknown.go", Line: 9, Body: "![P1 Badge](https://img.shields.io/badge/P1-orange) Unregistered app"}}
			for n := 0; n < 4; n++ {
				threads = append(threads, forgeThread{Author: "chatgpt-codex-connector[bot]", Path: fmt.Sprintf("P%d.go", n), Line: 70+n, Body: fmt.Sprintf("![P%d Badge](https://img.shields.io/badge/P%d-orange) Native priority", n, n)})
			}
			j.setForge(forgeState{Checks: checks, Protection: protectionJSON("build"), ReviewThreads: threads})
			j.init()
			branch := "feature/" + tc.name
			j.push(branch)
			run, _ := j.expectGate(branch)
			ci, _ := findStep(run.Steps, types.StepCI)
			findings, err := types.ParseFindingsJSON(deref(ci.FindingsJSON))
			if err != nil {
				t.Fatal(err)
			}
			var files []string
			severities := map[string]string{"P0.go": "error", "P1.go": "error", "P2.go": "warning", "P3.go": "info", "greptile.go": "warning"}
			for _, f := range findings.Items {
				files = append(files, f.File)
				if f.Action != "ask-user" || f.Category != "ci-review-bot" || f.Severity != severities[f.File] || f.Line == 0 || !strings.Contains(f.Description, "Badge") {
					t.Fatalf("lost native severity, badge, anchor or human routing: %+v", f)
				}
			}
			if !sameSet(files, tc.wantFiles...) || len(j.ciRepairPrompts()) != 0 {
				t.Fatalf("files=%v want=%v repair prompts=%v", files, tc.wantFiles, j.ciRepairPrompts())
			}
			j.finish(branch)
		})
	}
}

func codexActivity(author, head, state, timestamp string) json.RawMessage {
	activity := map[string]any{"user": map[string]string{"login": author}, "commit_id": head, "state": state}
	if timestamp != "" {
		activity["submitted_at"] = timestamp
	}
	data, _ := json.Marshal(activity)
	return data
}

func codexSummary(head string) json.RawMessage {
	data, _ := json.Marshal(map[string]any{"user": map[string]string{"login": "chatgpt-codex-connector[bot]"}, "body": "**Reviewed commit:** `" + head + "`", "created_at": "2026-10-21T12:01:00Z"})
	return data
}

// A receipt is a public, fresh provider read. It must reject a stale ready bit
// for both review activity and threads, without turning absence into a gate.
func TestCIScopeCodexFreshReceiptJourney(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setTrusted("ci:\n  review_bot_comments: always\n")
	base := forgeState{Checks: []forgeCheck{greenCheck("build")}, Protection: protectionJSON("build")}
	j.setForge(base)
	j.init()
	branch := "feature/codex-live-receipt"
	wt, run, out := j.strictRun(branch)
	if !strings.Contains(out, "outcome: checks-passed") {
		t.Fatalf("baseline not ready: %s", out)
	}
	head := run.HeadSHA
	const bot = "chatgpt-codex-connector[bot]"
	request := json.RawMessage(`{"id":7,"user":{"login":"owner"},"body":"@codex review","created_at":"2026-10-21T12:00:00Z"}`)
	eyes := json.RawMessage(`{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"eyes"}`)
	clean := json.RawMessage(`{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"+1"}`)
	for _, tc := range []struct {
		name                         string
		reviews, summaries, reactions []json.RawMessage
		threads                      []forgeThread
		ready, readError              bool
		wantError                    string
	}{
		{name: "no-participation", ready: true},
		{name: "current-pending", reviews: []json.RawMessage{codexActivity(bot, head, "PENDING", "")}, wantError: "review bot review is pending"},
		{name: "old-head-pending", reviews: []json.RawMessage{codexActivity(bot, strings.Repeat("b", 40), "PENDING", "")}, ready: true},
		{name: "non-bot-pending", reviews: []json.RawMessage{codexActivity("other-app[bot]", head, "PENDING", "")}, ready: true},
		{name: "submitted", reviews: []json.RawMessage{codexActivity(bot, head, "COMMENTED", "2026-10-21T12:01:00Z")}, ready: true},
		{name: "eyes-acknowledged", summaries: []json.RawMessage{request}, reactions: []json.RawMessage{eyes}, wantError: "review bot review is pending"},
		{name: "late-old-review-does-not-settle", summaries: []json.RawMessage{request}, reactions: []json.RawMessage{eyes}, reviews: []json.RawMessage{codexActivity(bot, strings.Repeat("b", 40), "COMMENTED", "2026-10-21T12:01:00Z")}, wantError: "review bot review is pending"},
		{name: "late-old-summary-does-not-settle", summaries: []json.RawMessage{request, codexSummary(strings.Repeat("b", 40))}, reactions: []json.RawMessage{eyes}, wantError: "review bot review is pending"},
		{name: "current-summary-settles", summaries: []json.RawMessage{request, codexSummary(head[:10])}, reactions: []json.RawMessage{eyes}, ready: true},
		{name: "clean-thumbs-up-settles", summaries: []json.RawMessage{request}, reactions: []json.RawMessage{eyes, clean}, ready: true},
		{name: "other-app-eyes-ignored", summaries: []json.RawMessage{request}, reactions: []json.RawMessage{json.RawMessage(`{"user":{"login":"other-app[bot]"},"content":"eyes"}`)}, ready: true},
		{name: "unknown-state-fails-closed", reviews: []json.RawMessage{codexActivity(bot, head, "NOT_A_REVIEW_STATE", "")}, wantError: "unknown review completion state"},
		{name: "provider-read-error-fails-closed", readError: true, wantError: "HTTP 403"},
		{name: "unresolved-thread", threads: []forgeThread{{Author: bot, Path: "store.go", Line: 73, Body: "![P1 Badge](https://img.shields.io/badge/P1-orange) Lost update"}}, wantError: "no unresolved review bot comments"},
		{name: "resolved-thread", threads: []forgeThread{{Author: bot, Path: "store.go", Line: 73, Body: "Resolved", Resolved: true}}, ready: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forge := base
			forge.Reviews, forge.ReviewSummaries, forge.ReviewThreads = tc.reviews, tc.summaries, tc.threads
			forge.ReviewReactions = map[string][]json.RawMessage{"7": tc.reactions}
			if tc.readError {
				forge.ReviewError = &forgeEndpoint{Status: 403, Message: "Forbidden"}
			}
			j.setForge(forge)
			receipt, err := j.receipt(wt, run.ID)
			if (err == nil) != tc.ready || tc.ready && !strings.Contains(receipt, `"phase":"ci-ready"`) {
				t.Fatalf("%s: ready=%v err=%v output=%s", tc.name, tc.ready, err, receipt)
			}
			if !tc.ready && !strings.Contains(receipt, tc.wantError) {
				t.Fatalf("receipt refused for unrelated reason: %s", receipt)
			}
		})
	}
	j.setForge(base)
	j.finish(branch)
}

// Exercise more polls than the consecutive read-error threshold. A legitimate
// start signal waits, without a repair turn or provider-error approval gate.
func TestCIScopeCodexAcknowledgedRequestWaitJourney(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setTrusted("ci:\n  review_bot_comments: always\n")
	forge := forgeState{Checks: []forgeCheck{greenCheck("build")}, Protection: protectionJSON("build"), ReviewSummaries: []json.RawMessage{json.RawMessage(`{"id":7,"body":"@codex review","created_at":"2026-10-21T12:00:00Z"}`)}, ReviewReactions: map[string][]json.RawMessage{"7": {json.RawMessage(`{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"eyes"}`)}}}
	j.setForge(forge)
	j.init()
	branch := "feature/codex-acknowledged-wait"
	j.push(branch)
	run := j.expectWaiting(branch, "review bot review is pending")
	deadline := time.Now().Add(4 * time.Minute)
	for j.ghCalls("/pulls/", "/reviews") < 7 {
		if time.Now().After(deadline) {
			t.Fatal("did not observe seven review polls")
		}
		time.Sleep(time.Second)
	}
	info := j.h.RunInfo(run.ID)
	ci, _ := findStep(info.Steps, types.StepCI)
	if info.CIReady || ci.Status != types.StepStatusRunning || len(j.ciRepairPrompts()) != 0 {
		t.Fatalf("pending review incorrectly escalated: ready=%v ci=%s repairs=%v", info.CIReady, ci.Status, j.ciRepairPrompts())
	}
	t.Logf("Seven live provider polls remained monitoring without readiness or repair:\n%s", j.ciLog(run.ID))
	forge.ReviewReactions["7"] = append(forge.ReviewReactions["7"], json.RawMessage(`{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"+1"}`))
	j.setForge(forge)
	j.expectReady(branch, scopeCheckWaitTime)
	j.finish(branch)
}

// The same live read used by the monitor must distinguish an operator's
// approval over unresolved comments from approval after they were resolved.
func TestCIScopeCodexApprovalVerificationJourney(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setTrusted("ci:\n  review_bot_comments: always\n")
	j.init()
	for _, resolved := range []bool{false, true} {
		t.Run(fmt.Sprintf("resolved-%v", resolved), func(t *testing.T) {
			forge := forgeState{Checks: []forgeCheck{greenCheck("build")}, Protection: protectionJSON("build"), ReviewThreads: []forgeThread{{Author: "chatgpt-codex-connector[bot]", Path: "store.go", Line: 73, Body: "![P1 Badge](https://img.shields.io/badge/P1-orange) Lost update"}}}
			j.setForge(forge)
			branch := fmt.Sprintf("feature/codex-approve-%v", resolved)
			j.push(branch)
			run, _ := j.expectGate(branch)
			forge.ReviewThreads[0].Resolved = resolved
			j.setForge(forge)
			j.h.Respond(run.ID, types.StepCI, types.ActionApprove)
			done := j.h.WaitForRun(branch, scopeCheckWaitTime)
			if done.Status != types.RunCompleted || (done.CIOverrideReason == "") != resolved {
				t.Fatalf("approval lost live thread truth: status=%s override=%q resolved=%v", done.Status, done.CIOverrideReason, resolved)
			}
			t.Logf("Live approval completed: resolved=%v override=%q", resolved, done.CIOverrideReason)
		})
	}
}

func TestCIScopeCodexRecordedPolicyReceiptJourney(t *testing.T) {
	for _, tc := range []struct {
		name, mode, external, author string
		ready                        bool
	}{
		{"codex-exclusion-preserves-greptile", "always", "Codex Review", "greptile-apps[bot]", false},
		{"greptile-exclusion-preserves-codex", "always", "Greptile Review", "chatgpt-codex-connector[bot]", false},
		{"codex-exclusion-permits-codex", "always", "Codex Review", "chatgpt-codex-connector[bot]", true},
		{"on-failure-without-a-bot-check", "on_failure", "", "chatgpt-codex-connector[bot]", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := newScopeJourney(t, SetupOpts{Agent: "claude"})
			cfg := "ci:\n  review_bot_comments: " + tc.mode + "\n"
			if tc.external != "" {
				cfg += "  external_checks: [" + tc.external + "]\n"
			}
			j.setTrusted(cfg)
			forge := forgeState{Checks: []forgeCheck{greenCheck("build")}, Protection: protectionJSON("build")}
			j.setForge(forge)
			j.init()
			branch := "feature/receipt-" + tc.name
			wt, run, initial := j.strictRun(branch)
			if !strings.Contains(initial, "outcome: checks-passed") {
				t.Fatalf("baseline not ready: %s", initial)
			}
			forge.ReviewThreads = []forgeThread{{Author: tc.author, Path: "finding.go", Line: 7, Body: "![P1 Badge](https://img.shields.io/badge/P1-orange) Unresolved"}}
			j.setForge(forge)
			out, err := j.receipt(wt, run.ID)
			if (err == nil) != tc.ready || !tc.ready && !strings.Contains(out, "no unresolved review bot comments") {
				t.Fatalf("recorded policy readiness=%v err=%v: %s", tc.ready, err, out)
			}
			if tc.name == "codex-exclusion-permits-codex" {
				forge.ReviewThreads = nil
				forge.Reviews = []json.RawMessage{codexActivity(tc.author, run.HeadSHA, "PENDING", "")}
				j.setForge(forge)
				if out, err := j.receipt(wt, run.ID); err != nil {
					t.Fatalf("excluded Codex activity held readiness: %v\n%s", err, out)
				}
			}
			j.finish(branch)
		})
	}
}

func TestCIScopeGreptileCheckCompletionJourney(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setTrusted("ci:\n  review_bot_comments: always\n  required_checks: all\n")
	greptile := runningCheck("Greptile Review")
	greptile.AppSlug, greptile.AppID = "greptile-apps", 100
	forge := forgeState{Checks: []forgeCheck{greenCheck("build"), greptile}, Protection: protectionJSON("build")}
	j.setForge(forge)
	j.init()
	branch := "feature/greptile-check-completion"
	j.push(branch)
	j.expectWaiting(branch, "CI checks running, waiting for results...")
	forge.Checks[1].Status, forge.Checks[1].Conclusion = "COMPLETED", "SUCCESS"
	j.setForge(forge)
	j.expectReady(branch, scopeCheckWaitTime)
	j.finish(branch)
}
