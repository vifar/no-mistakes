package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// wfConcurrencyDoc is the part of a workflow this test reads.
type wfConcurrencyDoc struct {
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress any    `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
}

// A pull request's newer commit SUPERSEDES its older one, so the older run's
// result is worthless and cancelling it is right.
//
// The TRUNK is different. Every push to it is a DISTINCT commit that has to be
// verified, and cancelling the in-flight run means the trunk is never verified
// at all. A push to main lands every few minutes and the e2e job needs up to
// 90 minutes, so with cancel-in-progress on the trunk every main run was
// cancelled before its e2e job could report: the trunk's e2e never had a result
// to be red or green.
//
// The guard EVALUATES the expression, so a workflow that cancels the trunk
// fails here whether it says so with a literal or with a condition that
// happens to be true for a push.
func TestTrunkWorkflowsDoNotCancelTheTrunk(t *testing.T) {
	for _, name := range []string{"ci", "nix"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(".github/workflows/" + name + ".yml")
			if err != nil {
				t.Fatal(err)
			}
			var wf wfConcurrencyDoc
			if err := yaml.Unmarshal(raw, &wf); err != nil {
				t.Fatal(err)
			}
			expression, ok := wf.Concurrency.CancelInProgress.(string)
			if !ok {
				t.Fatalf("%s workflow's cancel-in-progress = %v (%T), want an expression that is false for a push to the trunk",
					name, wf.Concurrency.CancelInProgress, wf.Concurrency.CancelInProgress)
			}
			contexts := []workflowConditionContext{
				{EventName: "push", Ref: "refs/heads/main", RunID: "101"},
				{EventName: "push", Ref: "refs/heads/main", RunID: "102"},
				{EventName: "push", Ref: "refs/heads/main", RunID: "103"},
				{EventName: "pull_request", Ref: "refs/pull/101/merge", PRNumber: "101", RunID: "104"},
				{EventName: "pull_request", Ref: "refs/pull/101/merge", PRNumber: "101", RunID: "105"},
				{EventName: "pull_request", Ref: "refs/pull/102/merge", PRNumber: "102", RunID: "106"},
				{EventName: "workflow_dispatch", Ref: "refs/heads/main", RunID: "107"},
				{EventName: "workflow_dispatch", Ref: "refs/heads/main", RunID: "108"},
			}
			groups := make([]string, len(contexts))
			for i, context := range contexts {
				groups[i], err = evaluateWorkflowGroup(wf.Concurrency.Group, context)
				if err != nil {
					t.Fatal(err)
				}
				cancel, err := evaluateWorkflowCondition(expression, context)
				if err != nil {
					t.Fatal(err)
				}
				if cancel != (context.EventName == "pull_request") {
					t.Errorf("event %d (%s): cancel = %t", i, context.EventName, cancel)
				}
			}
			for i := range groups {
				for j := 0; j < i; j++ {
					wantShared := i == 4 && j == 3
					if shared := strings.EqualFold(groups[i], groups[j]); shared != wantShared {
						t.Errorf("events %d and %d: groups %q and %q share = %t, want %t", i, j, groups[i], groups[j], shared, wantShared)
					}
				}
			}
		})
	}
}

func evaluateWorkflowGroup(template string, context workflowConditionContext) (string, error) {
	var result strings.Builder
	for {
		prefix, expression, found := strings.Cut(template, "${{")
		result.WriteString(prefix)
		if !found {
			return result.String(), nil
		}
		expression, template, found = strings.Cut(expression, "}}")
		if !found {
			return "", fmt.Errorf("unterminated workflow group expression")
		}
		parts, _, err := splitWorkflowCondition(expression, "||")
		if err != nil {
			return "", err
		}
		if len(parts) == 0 {
			parts = []string{expression}
		}
		for _, part := range parts {
			value, err := workflowConditionValue(part, context)
			if err != nil {
				return "", err
			}
			if value != "" {
				result.WriteString(value)
				break
			}
		}
	}
}
