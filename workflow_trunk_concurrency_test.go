package main

import (
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

// workflowGroupValue resolves a concurrency group for one event. A group mixes
// literal text with `${{ ... }}` segments, and the `||` inside a segment picks
// the first NON-EMPTY operand, so it is not the boolean `||` the condition
// evaluator implements.
func workflowGroupValue(group string, context workflowConditionContext) string {
	var out strings.Builder
	rest := group
	for {
		start := strings.Index(rest, "${{")
		if start < 0 {
			out.WriteString(rest)
			return out.String()
		}
		out.WriteString(rest[:start])
		rest = rest[start+3:]
		end := strings.Index(rest, "}}")
		if end < 0 {
			out.WriteString(rest)
			return out.String()
		}
		out.WriteString(workflowGroupOperand(strings.TrimSpace(rest[:end]), context))
		rest = rest[end+2:]
	}
}

// workflowGroupOperand resolves one segment of a group: the first non-empty of
// its `||` alternatives.
func workflowGroupOperand(expression string, context workflowConditionContext) string {
	for _, part := range strings.Split(expression, "||") {
		value, err := workflowConditionValue(strings.TrimSpace(part), context)
		if err == nil && value != "" {
			return value
		}
	}
	return ""
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
// The group has to keep three things apart: a pull request's OWN runs share one
// group, so a superseded run IS cancelled; two pull requests do not share one,
// so one pull request never cancels another's; and two pushes never share one,
// so a push to the trunk never cancels another push.
//
// The guard EVALUATES the expression, so a workflow that cancels the trunk
// fails here whether it says so with a literal or with a condition that
// happens to be true for a push.
func TestTrunkWorkflowsDoNotCancelTheTrunk(t *testing.T) {
	for _, name := range []string{"ci", "nix"} {
		group := readWorkflowConcurrencyGroup(t, name)

		t.Run(name+"/cancel-in-progress", func(t *testing.T) {
			expression, ok := group.Concurrency.CancelInProgress.(string)
			if !ok {
				t.Fatalf("%s workflow's cancel-in-progress = %v (%T), want an expression that is false for a push to the trunk",
					name, group.Concurrency.CancelInProgress, group.Concurrency.CancelInProgress)
			}
			for _, tc := range []struct {
				event string
				want  bool
			}{
				{"push", false},
				{"pull_request", true},
			} {
				got, err := evaluateWorkflowCondition(expression, workflowConditionContext{EventName: tc.event})
				if err != nil {
					t.Fatalf("%s workflow's cancel-in-progress on %s: %v", name, tc.event, err)
				}
				if got != tc.want {
					t.Errorf("%s workflow's cancel-in-progress on %s = %v, want %v", name, tc.event, got, tc.want)
				}
			}
		})

		t.Run(name+"/group", func(t *testing.T) {
			pushA := workflowConditionContext{EventName: "push", RunID: "1", Ref: "refs/heads/main"}
			pushB := workflowConditionContext{EventName: "push", RunID: "2", Ref: "refs/heads/main"}
			prA := workflowConditionContext{EventName: "pull_request", PRNumber: "138", RunID: "3"}
			prA2 := workflowConditionContext{EventName: "pull_request", PRNumber: "138", RunID: "4"}
			prB := workflowConditionContext{EventName: "pull_request", PRNumber: "139", RunID: "5"}
			if got, other := workflowGroupValue(group.Concurrency.Group, pushA), workflowGroupValue(group.Concurrency.Group, pushB); got == other {
				t.Errorf("%s workflow: two pushes to the trunk share the group %q, so one can cancel the other", name, got)
			}
			if got, other := workflowGroupValue(group.Concurrency.Group, prA), workflowGroupValue(group.Concurrency.Group, prB); got == other {
				t.Errorf("%s workflow: two pull requests share the group %q, so one can cancel the other", name, got)
			}
			if got, other := workflowGroupValue(group.Concurrency.Group, prA), workflowGroupValue(group.Concurrency.Group, prA2); got != other {
				t.Errorf("%s workflow: a pull request's own runs do not share one group (%q vs %q), so a superseded run is never cancelled", name, got, other)
			}
		})
	}
}

// readWorkflowConcurrencyGroup reads a workflow's concurrency block.
func readWorkflowConcurrencyGroup(t *testing.T, name string) wfConcurrencyDoc {
	t.Helper()
	raw, err := os.ReadFile(".github/workflows/" + name + ".yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf wfConcurrencyDoc
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	return wf
}
