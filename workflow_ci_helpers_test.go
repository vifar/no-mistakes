package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// wfDoc is a minimal typed view of a workflow. Flexible fields (runs-on) are
// decoded as any and normalized by wfScalar and wfList.
type wfDoc struct {
	On   any               `yaml:"on"`
	Jobs map[string]*wfJob `yaml:"jobs"`
}

type wfJob struct {
	name           string
	RunsOn         any      `yaml:"runs-on"`
	If             string   `yaml:"if"`
	Needs          any      `yaml:"needs"`
	TimeoutMinutes int      `yaml:"timeout-minutes"`
	Steps          []wfStep `yaml:"steps"`
}

type wfStep struct {
	Name  string            `yaml:"name"`
	Uses  string            `yaml:"uses"`
	If    string            `yaml:"if"`
	Shell string            `yaml:"shell"`
	Env   map[string]string `yaml:"env"`
	Run   string            `yaml:"run"`
	With  map[string]string `yaml:"with"`
}

func wfScalar(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func wfList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func loadCIWorkflowDoc(t *testing.T) *wfDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read CI workflow: %v", err)
	}
	var wf wfDoc
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse CI workflow: %v", err)
	}
	for name, job := range wf.Jobs {
		job.name = name
	}
	return &wf
}

type workflowCommand struct {
	step int
	line int
	name string
	args []string
}

func workflowCommandsMatching(steps []wfStep, include func(wfStep) bool) []workflowCommand {
	var commands []workflowCommand
	for stepIndex, step := range steps {
		if !include(step) {
			continue
		}
		for lineIndex, line := range strings.Split(step.Run, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) == 0 || strings.HasPrefix(fields[0], "$p") || strings.ContainsAny(fields[0], "{}()") {
				continue
			}
			commands = append(commands, workflowCommand{step: stepIndex, line: lineIndex, name: fields[0], args: fields[1:]})
		}
	}
	return commands
}
