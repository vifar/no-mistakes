package main

import (
	"os"
	"slices"
	"strings"
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
	// The same-repository guard, verbatim. A job may wrap it in a status
	// function (see the e2e job below) but must never weaken it.
	const forkGuard = "github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository"
	for name, job := range wf.Jobs {
		if !slices.Equal(wfList(job.RunsOn), kingServerRunner) {
			t.Errorf("%s runner routing = %v", name, job.RunsOn)
		}
		if name == "e2e" {
			// The race suite and the e2e journeys saturate the same VM, so the
			// e2e job runs after the race job and still runs when it fails.
			// Both halves of that contract are checked here: without the
			// dependency the two suites overlap and the journeys fail their
			// waits, and without !cancelled() a red race job would hide every
			// e2e result.
			if !slices.Equal(wfList(job.Needs), []string{"test"}) {
				t.Errorf("e2e must depend on the race job so the two saturating suites never share the VM: needs = %v", job.Needs)
			}
			if !strings.Contains(job.If, forkGuard) {
				t.Errorf("e2e must keep the same-repository guard: %s", job.If)
			}
			if !strings.Contains(job.If, "!cancelled()") {
				t.Errorf("e2e must still run after a failed race job: %s", job.If)
			}
		} else {
			if len(wfList(job.Needs)) != 0 {
				t.Errorf("%s should not depend on another job: needs = %v", name, job.Needs)
			}
			if job.If != forkGuard {
				t.Errorf("%s must refuse external fork PR code before runner admission: %s", name, job.If)
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
