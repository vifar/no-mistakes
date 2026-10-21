package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

const terminalStepQuietAge = 5 * time.Minute
const terminalStepSweepInterval = time.Minute

// repairTerminalSteps never competes with an executor. Terminal rows from older
// versions can have outlived the run; explicit abort repairs them immediately,
// whereas the background sweep requires five minutes of inactivity.
func (m *RunManager) repairTerminalSteps(runID string, at time.Time, quietAge time.Duration) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	steps, err := m.db.TerminalExecutingSteps()
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, step := range steps {
		if (runID != "" && step.RunID != runID) || m.runs[step.RunID] != nil {
			continue
		}
		if step.AgentPID != nil {
			alive, err := processRunning(*step.AgentPID)
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
		changed, err := m.db.CloseTerminalStep(step, at.Unix())
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

func (m *RunManager) sweepTerminalSteps(at time.Time, quietAge time.Duration) (int, error) {
	return m.repairTerminalSteps("", at, quietAge)
}
