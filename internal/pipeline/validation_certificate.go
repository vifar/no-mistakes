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
