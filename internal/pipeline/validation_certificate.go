package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func StatusConfigDigest(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (e *Executor) recordRecoveredAcceptedGapProof(ctx context.Context, stepID, runID, workDir string) error {
	epoch, err := e.db.StatusValidationEpoch(runID)
	if err != nil {
		return err
	}
	digest := StatusConfigDigest(e.config)
	row, err := e.db.GetStepResult(stepID)
	if err != nil {
		return err
	}
	if row == nil || row.ValidAcceptedGaps() == nil || row.OverrideReason != nil || row.ApprovalReason != nil || row.Status != types.StepStatusCompleted || row.ExitCode == nil || *row.ExitCode != 0 || digest == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(*row.FindingsJSON)
	if err != nil || findings.TestedHeadSHA == "" {
		return nil
	}
	run, err := e.db.GetRun(runID)
	if err != nil {
		return err
	}
	head, err := git.HeadSHA(ctx, workDir)
	if err != nil || run == nil || findings.TestedHeadSHA != head || run.HeadSHA != head || digest != StatusConfigDigest(e.config) {
		return nil
	}
	return e.db.RecordStatusValidationStep(stepID, runID, head, digest, epoch)
}

func (e *Executor) terminalizeCertifiedWatcher(run *db.Run, message string, status types.RunStatus, head string, controlled *ControlledShutdownCause) error {
	ctx, cancel := context.WithTimeout(context.Background(), commitStatusTimeout)
	defer cancel()
	clean, err := git.Run(ctx, e.workDir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(clean) != "" {
		return e.db.RecordIneligibleTerminal(run.ID, message, status, &head)
	}
	observed, err := git.HeadSHA(ctx, e.workDir)
	if err != nil {
		return err
	}
	if observed != head {
		return fmt.Errorf("terminal watcher head changed")
	}
	var binding *db.ControlledShutdownBinding
	if controlled != nil {
		binding = &db.ControlledShutdownBinding{Instance: controlled.Instance, Lifecycle: controlled.Lifecycle, Reason: controlled.Error()}
	}
	return e.db.TerminalizeCertifiedWatcher(run.ID, message, head, StatusConfigDigest(e.config), status, binding)
}
