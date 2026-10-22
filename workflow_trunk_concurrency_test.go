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
			// The group has to keep a pull request's run apart from the
			// trunk's, or the two cancel each other.
			group := wf.Concurrency.Group
			for _, part := range []string{"github.event.pull_request.number", "github.ref"} {
				if !strings.Contains(group, part) {
					t.Errorf("%s workflow's concurrency group = %q, want it to name %q so a pull request and a trunk push do not share a group", name, group, part)
				}
			}
		})
	}
}
