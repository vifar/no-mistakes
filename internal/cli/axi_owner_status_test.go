package cli

import (
	"bytes"
	"encoding/json"
	toon "github.com/toon-format/toon-go"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiOwnerAndAwaitingPerson(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	chdir(t, repoDir)
	r, err := database.InsertRunWithIntent(repo.ID, "main", "head", "base", &db.RunIntent{Summary: "goal", Source: db.RunIntentSourceAgent, Owner: "firstmate-thread", SessionID: "firstmate-thread"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = database.UpdateRunStatus(r.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(r.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"summary":"Which behavior should we keep?","findings":[{"id":"f1","severity":"warning","description":"Choose the public behavior","action":"ask-user"}]}`
	if _, err = database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err = database.ParkStepForApproval(r.ID, step.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
		t.Fatal(err)
	}
	parked, err := database.GetRun(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldNow := nowUnix
	nowUnix = func() int64 { return *parked.AwaitingAgentSince + 150 }
	defer func() { nowUnix = oldNow }()
	status := func(args ...string) map[string]any {
		t.Helper()
		cmd := newAxiStatusCmd()
		cmd.SetArgs(append([]string{"--json"}, args...))
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("status: %v\n%s", err, out.String())
		}
		var doc map[string]any
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("json: %v\n%s", err, out.String())
		}
		return doc
	}
	doc := status()
	run := doc["run"].(map[string]any)
	if run["owner"] != "firstmate-thread" || run["ownerless"] != false {
		t.Errorf("owner: %+v", run)
	}
	if run["awaiting_person"] != true || run["awaiting_seconds"] != float64(150) || run["gate_question"] != "Which behavior should we keep?" {
		t.Errorf("parked signal: %+v", run)
	}
	filtered := status("--awaiting")
	if len(filtered["awaiting_runs"].([]any)) != 1 {
		t.Fatalf("filter: %+v", filtered)
	}
	if err = database.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	run = status()["run"].(map[string]any)
	if run["awaiting_person"] != false {
		t.Errorf("completed step with stale findings marked awaiting: %+v", run)
	}
	filtered = status("--awaiting")
	if len(filtered["awaiting_runs"].([]any)) != 0 {
		t.Fatalf("completed step in filter: %+v", filtered)
	}
	// Legacy/ownerless rows remain visibly ownerless.
	ownerless, err := database.InsertRun(repo.ID, "other", "other-head", "base")
	if err != nil {
		t.Fatal(err)
	}
	run = status("--run", ownerless.ID)["other_branch_run"].(map[string]any)
	if run["owner"] != "" || run["ownerless"] != true {
		t.Errorf("ownerless marker: %+v", run)
	}
}

func TestAxiGateQuestionFallbackBoundPreservesDescriptions(t *testing.T) {
	for _, gateStatus := range []types.StepStatus{types.StepStatusAwaitingApproval, types.StepStatusFixReview} {
		t.Run(string(gateStatus), func(t *testing.T) {
			repoDir, _, database, repo := setupAxiQueryRepo(t)
			chdir(t, repoDir)
			run, err := database.InsertRun(repo.ID, "main", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			step, err := database.InsertStepResult(run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			description := strings.Repeat("界🙂", 700)
			encoded, err := json.Marshal(map[string]any{"summary": "", "findings": []map[string]string{{"id": "f1", "severity": "warning", "description": description, "action": "ask-user"}}})
			if err != nil {
				t.Fatal(err)
			}
			findings := string(encoded)
			if _, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1); err != nil {
				t.Fatal(err)
			}
			if err := database.ParkStepForApproval(run.ID, step.ID, gateStatus, 0, 1, &findings); err != nil {
				t.Fatal(err)
			}
			for _, output := range []struct {
				name           string
				args           []string
				json, awaiting bool
			}{
				{name: "home"},
				{name: "status", args: []string{"status"}},
				{name: "status JSON", args: []string{"status", "--json"}, json: true},
				{name: "awaiting", args: []string{"status", "--awaiting"}, awaiting: true},
				{name: "awaiting JSON", args: []string{"status", "--awaiting", "--json"}, awaiting: true, json: true},
			} {
				t.Run(output.name, func(t *testing.T) {
					cmd := newAxiCmd()
					cmd.SetArgs(output.args)
					var out bytes.Buffer
					cmd.SetOut(&out)
					cmd.SetErr(&out)
					if err := cmd.Execute(); err != nil {
						t.Fatalf("render: %v", err)
					}
					type gateOutput struct {
						Question  string   `json:"gate_question" toon:"gate_question"`
						Questions []string `json:"gate_questions" toon:"gate_questions"`
					}
					var doc struct {
						Run       gateOutput   `json:"run" toon:"run"`
						ActiveRun gateOutput   `json:"active_run" toon:"active_run"`
						Awaiting  []gateOutput `json:"awaiting_runs" toon:"awaiting_runs"`
					}
					if output.json {
						err = json.Unmarshal(out.Bytes(), &doc)
					} else {
						err = toon.UnmarshalString(out.String(), &doc)
					}
					if err != nil {
						t.Fatalf("decode rendered contract: %v", err)
					}
					gate := doc.Run
					if output.name == "home" {
						gate = doc.ActiveRun
					}
					if output.awaiting {
						if len(doc.Awaiting) != 1 {
							t.Fatalf("awaiting rows = %d, want 1", len(doc.Awaiting))
						}
						gate = doc.Awaiting[0]
					}
					want := strings.Repeat("界🙂", 600) + "… (truncated, 1400 chars total)"
					if gate.Question != want {
						t.Errorf("gate_question differs from the 1200-rune prefix plus disclosure; got %d runes", utf8.RuneCountInString(gate.Question))
					}
					if len(gate.Questions) != 1 || gate.Questions[0] != description {
						t.Error("gate_questions must retain the exact complete original description")
					}
				})
			}
		})
	}
}
