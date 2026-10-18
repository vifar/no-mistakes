package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestOwnerPolicyStatusWarning(t *testing.T) {
	for _, tc := range []struct {
		name, owner string
		required    bool
	}{{name: "ownerless", required: true}, {name: "owned", owner: "Firstmate", required: true}, {name: "policy_off"}} {
		t.Run(tc.name, func(t *testing.T) {
			repoDir, _, database, repo := setupAxiQueryRepo(t)
			chdir(t, repoDir)
			writeGlobalConfig(t, fmt.Sprintf("intent:\n  require_owner: %v\n", tc.required))
			run, err := database.InsertRunWithIntent(repo.ID, "main", "head", "base", &db.RunIntent{Owner: tc.owner}, "")
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
			findings := `{"summary":"choose","findings":[{"id":"f1","severity":"warning","description":"choose","action":"ask-user"}]}`
			if _, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1); err != nil {
				t.Fatal(err)
			}
			if err := database.ParkStepForApproval(run.ID, step.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"axi"}, {"axi", "status"}, {"axi", "status", "--json"}, {"axi", "status", "--awaiting"}, {"axi", "status", "--awaiting", "--json"}, {"status"}} {
				out, err := executeCmd(args...)
				if err != nil {
					t.Fatalf("%v: %v %s", args, err, out)
				}
				want := tc.required && tc.owner == ""
				if got := strings.Contains(out, "until #82"); got != want {
					t.Errorf("%v policy warning=%v want=%v: %s", args, got, want, out)
				}
				if args[len(args)-1] == "--json" {
					var doc map[string]any
					if err := json.Unmarshal([]byte(out), &doc); err != nil {
						t.Fatal(err)
					}
					row, ok := doc["run"].(map[string]any)
					if !ok {
						row = doc["awaiting_runs"].([]any)[0].(map[string]any)
					}
					_, present := row["owner_policy_warning"]
					if present != want {
						t.Errorf("typed status warning=%v want=%v: %+v", present, want, row)
					}
				}

			}
		})
	}
}
