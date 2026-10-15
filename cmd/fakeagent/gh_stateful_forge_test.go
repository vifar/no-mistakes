package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGhForgeRollup(t *testing.T) {
	if got := (&ghForge{}).graphqlChecks(); !strings.Contains(got, `"statusCheckRollup":null`) {
		t.Fatalf("a forge with no checks must answer a null rollup, got %s", got)
	}
	forge := &ghForge{
		Checks: []ghForgeCheck{
			{DatabaseID: 5, Name: "build", Conclusion: "SUCCESS", AppSlug: "github-actions", AppID: 15368, Workflow: "CI", DetailsURL: "https://example.test/run/1/job/5"},
			{Name: "lint", Status: "IN_PROGRESS"},
		},
		Statuses: []ghForgeStatus{{Context: "legacy", State: "PENDING"}},
	}
	var parsed struct {
		Data struct {
			Repository struct {
				Object struct {
					Rollup struct {
						Contexts struct {
							Nodes []map[string]any `json:"nodes"`
						} `json:"contexts"`
					} `json:"statusCheckRollup"`
				} `json:"object"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(forge.graphqlChecks()), &parsed); err != nil {
		t.Fatal(err)
	}
	nodes := parsed.Data.Repository.Object.Rollup.Contexts.Nodes
	if len(nodes) != 3 {
		t.Fatalf("nodes = %v, want 2 check runs and 1 status context", nodes)
	}
	suite := nodes[0]["checkSuite"].(map[string]any)
	app := suite["app"].(map[string]any)
	workflow := suite["workflowRun"].(map[string]any)["workflow"].(map[string]any)
	if nodes[0]["__typename"] != "CheckRun" || nodes[0]["databaseId"].(float64) != 5 || app["slug"] != "github-actions" || app["databaseId"].(float64) != 15368 || workflow["name"] != "CI" {
		t.Fatalf("check run node lost its identity: %v", nodes[0])
	}
	if nodes[1]["status"] != "IN_PROGRESS" || nodes[2]["__typename"] != "StatusContext" || nodes[2]["context"] != "legacy" {
		t.Fatalf("unexpected nodes: %v", nodes[1:])
	}
}

func TestGhForgeREST(t *testing.T) {
	forge := &ghForge{
		Rules:       &ghForgeRules{Pages: []json.RawMessage{json.RawMessage(`[]`), json.RawMessage(`[{"type":"required_status_checks"}]`)}},
		Runs:        []json.RawMessage{json.RawMessage(`{"id":7,"run_attempt":2}`)},
		AttemptJobs: map[string][]int64{"7/2": {6, 8}},
	}
	const repo = "o/r"
	protection := "repos/o/r/branches/main/protection/required_status_checks"

	// An unprotected branch answers the 404 the product reads as "no protection".
	if _, errText, handled := forge.rest(repo, protection, nil); !handled || !strings.Contains(errText, "Branch not protected") || !strings.Contains(errText, "HTTP 404") {
		t.Fatalf("unprotected branch: handled=%v err=%q", handled, errText)
	}
	forge.Protection = &ghForgeEndpoint{Status: 403, Message: "Resource not accessible by integration"}
	if _, errText, _ := forge.rest(repo, protection, nil); !strings.Contains(errText, "HTTP 403") {
		t.Fatalf("forbidden protection err = %q", errText)
	}
	forge.Protection = &ghForgeEndpoint{Body: json.RawMessage(`{"contexts":["build"]}`)}
	if out, errText, _ := forge.rest(repo, protection, nil); errText != "" || out != `{"contexts":["build"]}` {
		t.Fatalf("protection = %q err %q", out, errText)
	}

	out, _, _ := forge.rest(repo, "repos/o/r/rules/branches/main?per_page=100", nil)
	var pages []json.RawMessage
	if err := json.Unmarshal([]byte(out), &pages); err != nil || len(pages) != 2 {
		t.Fatalf("slurped rules = %q, %v; want one outer element per page", out, err)
	}
	forge.Rules = &ghForgeRules{Status: 500, Message: "Server Error"}
	if _, errText, _ := forge.rest(repo, "repos/o/r/rules/branches/main", nil); !strings.Contains(errText, "HTTP 500") {
		t.Fatalf("failing rules err = %q", errText)
	}

	out, _, _ = forge.rest(repo, "repos/o/r/actions/runs", nil)
	if !strings.Contains(out, `"total_count":1`) || !strings.Contains(out, `"run_attempt":2`) {
		t.Fatalf("runs = %s", out)
	}
	out, _, handled := forge.rest(repo, "repos/o/r/actions/runs/7/attempts/2/jobs", nil)
	if !handled || !strings.Contains(out, `"total_count":2`) || !strings.Contains(out, `"id":8`) {
		t.Fatalf("attempt jobs = %s", out)
	}
	if _, _, handled := forge.rest(repo, "repos/other/r/actions/runs", nil); handled {
		t.Fatal("an endpoint of another repository must not be answered")
	}
}

func TestLoadGhForgeFollowsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.json")
	t.Setenv("FAKEAGENT_GH_FORGE", path)
	if forge, err := loadGhForge(); forge != nil || err != nil {
		t.Fatalf("a missing forge file must keep the legacy stub answers: %v, %v", forge, err)
	}
	if err := os.WriteFile(path, []byte(`{"merge_state_status":"BEHIND","review_threads":[{"author":"bot","body":"x"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	forge, err := loadGhForge()
	if err != nil || forge == nil || forge.MergeStateStatus != "BEHIND" || len(forge.ReviewThreads) != 1 {
		t.Fatalf("forge = %+v, %v", forge, err)
	}
	if !strings.Contains(forge.graphqlReviewThreads(), `"login":"bot"`) {
		t.Fatalf("review threads = %s", forge.graphqlReviewThreads())
	}
	if err := os.WriteFile(path, []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGhForge(); err == nil {
		t.Fatal("a corrupt forge file must fail loudly")
	}
}
