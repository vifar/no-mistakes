package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCustomGateStep_ZeroExitRefusalsCannotCertifyOrPreserve(t *testing.T) {
	for _, tc := range []struct {
		name, report, problem string
		eligible              bool
	}{
		{"clean", `{"findings":[]}`, "", true},
		{"error", `{"findings":[{"id":"budget","severity":"error","description":"below budget"}]}`, "below budget", false},
		{"malformed", "{", "findings file invalid", false},
		{"transport", `{"findings":[{"id":"budget","severity":"error","description":"` + strings.Repeat("<", 200000) + `"}]}`, "too large to transport", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc, _, publication := strictStatusContext(t)
			certificate, err := sc.DB.BuildStatusCertificate(sc.Run.ID, sc.Run.HeadSHA, pipeline.StatusConfigDigest(sc.Config))
			if err != nil || certificate == nil {
				t.Fatalf("core certificate missing: %v", err)
			}
			publication.StrictCertificate = certificate
			publication.LastState = "success"
			if err := sc.DB.PutCommitStatusPublication(sc.Run.ID, publication); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sc.WorkDir, "gate-report.json"), []byte(tc.report), 0600); err != nil {
				t.Fatal(err)
			}
			step := &CustomGateStep{Gate: config.Gate{Name: "budget", After: types.StepLint, Command: gateReportCommand(t, sc.WorkDir, 0)}}
			outcome, err := step.Execute(sc)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.ExitCode != 0 || outcome.NeedsApproval == tc.eligible {
				t.Fatalf("gate command outcome: %+v", outcome)
			}
			if tc.problem != "" && !strings.Contains(outcome.Findings, tc.problem) {
				t.Fatalf("missing observed refusal %q: %s", tc.problem, outcome.Findings)
			}
			sr, err := sc.DB.InsertStepResult(sc.Run.ID, step.Name())
			if err != nil {
				t.Fatal(err)
			}
			if err := sc.DB.SetStepFindings(sr.ID, outcome.Findings); err != nil {
				t.Fatal(err)
			}
			if err := sc.DB.CompleteStep(sr.ID, outcome.ExitCode, 1, ""); err != nil {
				t.Fatal(err)
			}
			if err := sc.DB.RecordStatusValidationStep(sr.ID, sc.Run.ID, sc.Run.HeadSHA, pipeline.StatusConfigDigest(sc.Config), 0); err != nil {
				t.Fatal(err)
			}
			certificate, err = sc.DB.BuildStatusCertificate(sc.Run.ID, sc.Run.HeadSHA, pipeline.StatusConfigDigest(sc.Config))
			if err != nil {
				t.Fatal(err)
			}
			if (certificate != nil) != tc.eligible {
				t.Errorf("gate certificate=%v, eligible=%v", certificate != nil, tc.eligible)
			}
			if certificate != nil {
				publication.StrictCertificate = certificate
				if err := sc.DB.PutCommitStatusPublication(sc.Run.ID, publication); err != nil {
					t.Fatal(err)
				}
			}
			if err := sc.DB.TerminalizeCertifiedWatcher(sc.Run.ID, types.RunCancelReasonAbortedByUser, sc.Run.HeadSHA, pipeline.StatusConfigDigest(sc.Config), types.RunCancelled); err != nil {
				t.Fatal(err)
			}
			stored, err := sc.DB.GetCommitStatusPublication(context.Background(), sc.Run.ID, publication)
			if err != nil {
				t.Fatal(err)
			}
			preserved := stored.PreserveSuccess != nil && *stored.PreserveSuccess
			if preserved != tc.eligible {
				t.Fatalf("zero-exit gate preserved success=%v, want %v", preserved, tc.eligible)
			}
		})
	}
}
