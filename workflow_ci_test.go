package main

import (
	"os"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// The repository-wide Actions actor policy protects the persistent local runner
// from workflows modified by external contributors. These checks also protect
// routing, token scope, and same-repository PR admission.
func TestCIWorkflowUsesDedicatedLocalRunner(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
	}
	if err := yaml.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"pull_request", "push", "workflow_dispatch"} {
		if _, ok := settings.On[event]; !ok {
			t.Errorf("missing CI trigger %s", event)
		}
	}
	if settings.Permissions["contents"] != "read" || len(settings.Permissions) != 1 {
		t.Fatalf("CI permissions = %v, want only contents: read", settings.Permissions)
	}
	wf := loadCIWorkflowDoc(t)
	for _, name := range []string{"check", "test", "e2e"} {
		if wf.Jobs[name] == nil {
			t.Fatalf("missing %s job", name)
		}
	}
	for name, job := range wf.Jobs {
		if !slices.Equal(wfList(job.RunsOn), kingServerRunner) {
			t.Errorf("%s runner routing = %v", name, job.RunsOn)
		}
		if name == "e2e" {
			if !slices.Equal(wfList(job.Needs), []string{"test"}) {
				t.Errorf("e2e must depend on the race job so the two saturating suites never share the VM: needs = %v", job.Needs)
			}
		} else {
			if len(wfList(job.Needs)) != 0 {
				t.Errorf("%s should not depend on another job: needs = %v", name, job.Needs)
			}
		}
		maxTimeout := 45
		if name == "test" || name == "e2e" {
			maxTimeout = 90
		}
		if job.TimeoutMinutes < 1 || job.TimeoutMinutes > maxTimeout {
			t.Errorf("%s timeout = %d minutes, want 1..%d", name, job.TimeoutMinutes, maxTimeout)
		}
		checkout := 0
		for _, step := range job.Steps {
			if step.Uses != "actions/checkout@v6" {
				continue
			}
			checkout++
			if step.With["persist-credentials"] != "false" || step.With["ref"] != "${{ github.event.pull_request.head.sha || github.sha }}" {
				t.Errorf("%s must test the exact head without persisted credentials: %v", name, step.With)
			}
		}
		if checkout != 1 {
			t.Errorf("%s checkout count = %d", name, checkout)
		}
	}
}

func TestCIWorkflowJobAdmission(t *testing.T) {
	wf := loadCIWorkflowDoc(t)
	for name, job := range wf.Jobs {
		for _, event := range []struct {
			name  string
			event string
			head  string
			want  bool
		}{
			{"same-repository PR", "pull_request", "owner/repo", true},
			{"external PR", "pull_request", "stranger/repo", false},
			{"push", "push", "", true},
			{"dispatch", "workflow_dispatch", "", true},
		} {
			for _, raceResult := range []string{"success", "failure", "skipped"} {
				for _, cancelled := range []bool{false, true} {
					scenario := name + "/" + event.name + "/race-" + raceResult + "/active"
					if cancelled {
						scenario = name + "/" + event.name + "/race-" + raceResult + "/cancelled"
					}
					t.Run(scenario, func(t *testing.T) {
						got, err := evaluateWorkflowJobCondition(job.If, workflowConditionContext{
							EventName: event.event, Repository: "owner/repo", HeadRepo: event.head,
							Cancelled: cancelled,
							Failed:    len(wfList(job.Needs)) > 0 && raceResult == "failure",
							Skipped:   len(wfList(job.Needs)) > 0 && raceResult == "skipped",
						})
						if err != nil {
							t.Fatal(err)
						}
						want := event.want && !cancelled
						if got != want {
							t.Fatalf("job admission = %t, want %t", got, want)
						}
					})
				}
			}
		}
	}
}

func TestCIWorkflowRunsEndToEndJourneys(t *testing.T) {
	job := loadCIWorkflowDoc(t).Jobs["e2e"]
	if job == nil {
		t.Fatal("CI workflow has no e2e job")
	}
	for _, command := range workflowCommandsMatching(job.Steps, func(wfStep) bool { return true }) {
		if command.name == "make" && slices.Equal(command.args, []string{"e2e"}) {
			return
		}
	}
	t.Fatal("local CI must exercise make e2e")
}
