//go:build e2e && !windows

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A real daemon and product CLI must not certify a green head over a Codex
// P1 thread. The fake forge supplies only the external GitHub boundary.
func TestCIScopeCodexThreads(t *testing.T) {
	for _, mode := range []string{"on_failure", "always"} {
		t.Run(mode, func(t *testing.T) {
			j := newScopeJourney(t, SetupOpts{Agent: "claude"})
			j.setTrusted("ci:\n  review_bot_comments: " + mode + "\n")
			j.init()
			for _, resolved := range []bool{false, true} {
				name := "unresolved"
				if resolved {
					name = "resolved"
				}
				t.Run(name, func(t *testing.T) {
					forge := forgeState{Checks: []forgeCheck{greenCheck("build")}, Protection: protectionJSON("build"), ReviewThreads: []forgeThread{{Author: "chatgpt-codex-connector[bot]", Path: "store.go", Line: 73, Body: "![P1 Badge](https://img.shields.io/badge/P1-orange) Fix lost updates", Resolved: resolved}}}
					j.setForge(forge)
					branch := "feature/codex-" + mode + "-" + name
					j.push(branch)
					if mode == "always" && !resolved {
						run, _ := j.expectGate(branch)
						ci, _ := findStep(run.Steps, types.StepCI)
						f, err := types.ParseFindingsJSON(deref(ci.FindingsJSON))
						if err != nil {
							t.Fatal(err)
						}
						if len(f.Items) != 1 || f.Items[0].File != "store.go" || f.Items[0].Line != 73 || f.Items[0].Severity != "error" || f.Items[0].Action != "ask-user" {
							t.Fatalf("findings=%+v", f)
						}
					} else {
						j.expectReady(branch, scopeCheckWaitTime)
					}
					j.finish(branch)
				})
			}
		})
	}
}

func TestCIScopeCodexPendingReview(t *testing.T) {
	j := newScopeJourney(t, SetupOpts{Agent: "claude"})
	j.setTrusted("ci:\n  review_bot_comments: always\n")
	j.init()
	branch := "feature/codex-pending"
	j.h.CommitChange(branch, "codex-pending.txt", "pending review\n", "add pending review scenario")
	headBytes, err := j.h.runGit(t.Context(), j.h.WorkDir, "rev-parse", branch)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(headBytes))
	review := func(state string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":%q,"state":%q}`, head, state))
	}
	forge := forgeState{Checks: []forgeCheck{greenCheck("build")}, Protection: protectionJSON("build"), Reviews: []json.RawMessage{review("PENDING")}}
	j.setForge(forge)
	j.h.PushToGate(branch)
	j.expectWaiting(branch, "review bot review is pending")
	forge.Reviews = []json.RawMessage{review("COMMENTED")}
	j.setForge(forge)
	j.expectReady(branch, scopeCheckWaitTime)
	j.finish(branch)
}
