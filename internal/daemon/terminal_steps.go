package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

const terminalStepQuietAge = 5 * time.Minute
const terminalStepSweepInterval = time.Minute
const terminalStepScanBudget = 10 * time.Second
const terminalStepProbeBudget = time.Second

// repairTerminalSteps never competes with an executor. Terminal rows from older
// versions can have outlived the run; explicit abort repairs them immediately,
// whereas the background sweep requires five minutes of inactivity.
func (m *RunManager) repairTerminalSteps(ctx context.Context, runID string, at time.Time, quietAge time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, terminalStepScanBudget)
	defer cancel()
	select {
	case m.maintenanceGate <- struct{}{}:
		defer func() { <-m.maintenanceGate }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	if m.beforeTerminalStepsRead != nil {
		m.beforeTerminalStepsRead(ctx)
	}
	steps, err := m.db.TerminalExecutingSteps(ctx)
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return closed, err
		}
		if runID != "" && step.RunID != runID {
			continue
		}
		m.mu.Lock()
		owned := m.runs[step.RunID] != nil
		m.mu.Unlock()
		if owned {
			continue
		}
		// Terminal run IDs are never resumed. A revived DB row or new step
		// activity still fails CloseTerminalStep's conditional write. Keep
		// database and OS reads outside mu so unrelated aborts never wait on
		// this maintenance scan.
		if step.AgentPID != nil {
			probeCtx, cancelProbe := context.WithTimeout(ctx, terminalStepProbeBudget)
			probe := m.terminalProcessRunning
			if probe == nil {
				probe = processRunningContext
			}
			alive, err := probe(probeCtx, *step.AgentPID)
			if probeCtx.Err() != nil {
				err = probeCtx.Err()
			}
			cancelProbe()
			if *step.AgentPID <= 0 || err != nil || alive {
				if runID != "" {
					return closed, fmt.Errorf("terminal run %s still has step %s (%s) with live or unverifiable agent_pid; no cleanup performed for that row", step.RunID, step.ID, step.StepName)
				}
				continue
			}
		}
		if quietAge > 0 && !terminalStepQuiet(step, m.paths.RunLogDir(step.RunID), at, quietAge) {
			continue
		}
		changed, err := m.db.CloseTerminalStep(ctx, step, at.Unix())
		if err != nil {
			return closed, err
		}
		if changed {
			closed++
		}
	}
	return closed, nil
}

func terminalStepQuiet(step *db.StepResult, logDir string, at time.Time, age time.Duration) bool {
	latest := int64(0)
	for _, stamp := range []*int64{step.StartedAt, step.RoundStartedAt, step.LastActivityAt} {
		if stamp != nil && *stamp > latest {
			latest = *stamp
		}
	}
	if latest == 0 || at.Sub(time.Unix(latest, 0)) < age {
		return false
	}
	logPath := filepath.Join(logDir, string(step.StepName)+".log")
	if step.LogPath != nil && *step.LogPath != "" {
		logPath = *step.LogPath
	}
	info, err := os.Stat(logPath)
	if os.IsNotExist(err) {
		return true
	}
	return err == nil && info.Mode().IsRegular() && at.Sub(info.ModTime()) >= age
}

func (m *RunManager) sweepTerminalSteps(ctx context.Context, at time.Time, quietAge time.Duration) (int, error) {
	return m.repairTerminalSteps(ctx, "", at, quietAge)
}
