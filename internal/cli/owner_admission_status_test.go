package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRecordedOwnerAdmissionSurvivesPolicyChange(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	chdir(t, repoDir)
	writeGlobalConfig(t, "log_level: error\nintent:\n  require_owner: false\n")
	warning := "invalid launch ownership: duplicate owner push option; admitted ownerless after receive (follow-up #82)"
	run, err := database.InsertRunWithIntent(repo.ID, "main", "head", "base", &db.RunIntent{OwnerAdmission: &types.OwnerAdmissionEvent{Event: "launch_owner_policy_bypassed", CallerPath: "post-receive (TUI/direct push)", OwnerlessAdmitted: true, OwnerValidationError: "duplicate owner push option", Warning: warning}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"axi"}, {"axi", "status"}, {"axi", "status", "--json"}, {"status"}} {
		out, err := executeCmd(args...)
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		if !strings.Contains(out, warning) {
			t.Errorf("%v lost recorded warning after policy change: %s", args, out)
		}
		if args[len(args)-1] == "--json" {
			var doc struct {
				Run struct {
					Event types.OwnerAdmissionEvent `json:"owner_admission_event"`
				} `json:"run"`
			}
			if err := json.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Run.Event.RunID != run.ID || doc.Run.Event.OwnerValidationError != "duplicate owner push option" || !doc.Run.Event.OwnerlessAdmitted {
				t.Fatalf("lost durable event: %+v", doc.Run.Event)
			}
		}
	}
}
