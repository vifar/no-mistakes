package main

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"testing"
)

func TestPhaseHeavyJobBudgets(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Env  map[string]string `yaml:"env"`
		Jobs map[string]struct {
			Timeout int `yaml:"timeout-minutes"`
		} `yaml:"jobs"`
	}
	if err = yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	if wf.Env["GOMAXPROCS"] != "8" {
		t.Fatalf("GOMAXPROCS=%s", wf.Env["GOMAXPROCS"])
	}
	for _, name := range []string{"test", "e2e"} {
		if wf.Jobs[name].Timeout != 90 {
			t.Fatalf("%s timeout=%d", name, wf.Jobs[name].Timeout)
		}
	}
	report := map[string]any{"env": wf.Env, "jobs": loadCIWorkflowDoc(t).Jobs}
	data, e := json.MarshalIndent(report, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(os.Getenv("PHASE_EVIDENCE"), "workflow-semantics.json"), data, 0600); e != nil {
		t.Fatal(e)
	}
	t.Logf("Actions semantic configuration: race job=90 minutes; e2e job=90 minutes; GOMAXPROCS=8")
}
