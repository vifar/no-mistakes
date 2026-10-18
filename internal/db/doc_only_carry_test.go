package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCarryValidationOverDocOnlyCommits(t *testing.T) {
	const from, to = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cases := []struct {
		name         string
		approved     string
		tested       string
		skipTest     bool
		wantCarry    bool
		unattributed bool
		strict       bool
		movedHead    bool
	}{
		{name: "carries approval and tested head", approved: from, tested: from, wantCarry: true},
		{name: "revoked approval is not resurrected", approved: "", tested: from},
		{name: "replaced approval is not overwritten", approved: to[:39] + "c", tested: from},
		{name: "tested elsewhere does not carry", approved: from, tested: to[:39] + "c"},
		{name: "skipped test carries approval only", approved: from, skipTest: true, wantCarry: true},
		{name: "unattributed range never carries", approved: from, tested: from, unattributed: true},
		{name: "durable strict mode never carries", approved: from, tested: from, strict: true},
		{name: "replaced current head never carries", approved: from, tested: from, movedHead: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := openTestDB(t)
			repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRun(repo.ID, "feature", to, "base")
			if err != nil {
				t.Fatal(err)
			}
			if tc.approved != "" {
				if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, tc.approved); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.unattributed {
				id, err := d.BeginDocumentPass(run.ID, from)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.CompleteDocumentPass(id, to, 1, `{"findings":[],"summary":"updated docs"}`); err != nil {
					t.Fatal(err)
				}
			}
			if tc.strict {
				if _, err := d.sql.Exec(`UPDATE runs SET shipping_options=? WHERE id=?`, `{"mode":"full"}`, run.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.movedHead {
				if err := d.UpdateRunHeadSHA(run.ID, from); err != nil {
					t.Fatal(err)
				}
			}
			step, err := d.InsertStepResult(run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			if tc.skipTest {
				if err := d.CompleteStepWithStatus(step.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
					t.Fatal(err)
				}
			} else {
				raw, err := types.MarshalFindingsJSON(types.Findings{Summary: "ok", TestedHeadSHA: tc.tested})
				if err != nil {
					t.Fatal(err)
				}
				if err := d.SetStepFindings(step.ID, raw); err != nil {
					t.Fatal(err)
				}
				if err := d.CompleteStepWithStatus(step.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
					t.Fatal(err)
				}
			}

			carried, err := d.CarryValidationOverDocOnlyCommits(run.ID, from, to)
			if err != nil {
				t.Fatal(err)
			}
			if carried != tc.wantCarry {
				t.Fatalf("carried = %v, want %v", carried, tc.wantCarry)
			}
			passes, err := d.GetDocumentPasses(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.unattributed && ((passes[0].CarriedReviewFrom != nil) != tc.wantCarry) {
				t.Fatalf("carry audit = %+v, want carried=%v", passes, tc.wantCarry)
			}
			got, err := d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantApproved := tc.approved
			if tc.wantCarry {
				wantApproved = to
			}
			if (got.ReviewApprovedHeadSHA == nil) != (wantApproved == "") || (got.ReviewApprovedHeadSHA != nil && *got.ReviewApprovedHeadSHA != wantApproved) {
				t.Fatalf("approved head = %v, want %q", got.ReviewApprovedHeadSHA, wantApproved)
			}
			if tc.skipTest {
				return
			}
			steps, err := d.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			findings, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
			if err != nil {
				t.Fatal(err)
			}
			wantTested := tc.tested
			if tc.wantCarry {
				wantTested = to
			}
			if findings.TestedHeadSHA != wantTested {
				t.Fatalf("tested head = %s, want %s (a half-applied carry must roll back)", findings.TestedHeadSHA, wantTested)
			}
		})
	}
}
