package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func newShutdownToken() string {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(token[:])
}

// prepareControlledShutdown drains the bound run decisions before acknowledging.
// A preparation record alone is never used by crash recovery.
func (m *RunManager) prepareControlledShutdown(ctx context.Context, expected ipc.ShutdownParams) (*ipc.ShutdownResult, error) {
	if expected.ExpectedPID <= 0 || expected.ExpectedStartedAt == "" || expected.ExpectedInstance == "" {
		return nil, fmt.Errorf("complete captured daemon identity is required")
	}
	if expected.ExpectedPID != os.Getpid() {
		return nil, fmt.Errorf("shutdown targets a different daemon instance")
	}
	if m.runtimeStartedAt.IsZero() || expected.ExpectedStartedAt != m.runtimeStartedAt.UTC().Format(time.RFC3339Nano) {
		return nil, fmt.Errorf("shutdown daemon start time changed")
	}
	if m.runtimeInstance == "" || expected.ExpectedInstance != m.runtimeInstance {
		return nil, fmt.Errorf("daemon runtime identity is unavailable")
	}
	lifecycle := newShutdownToken()
	if lifecycle == "" {
		return nil, fmt.Errorf("shutdown lifecycle identity is unavailable")
	}
	m.mu.Lock()
	if m.shuttingDown.Load() {
		m.mu.Unlock()
		return nil, fmt.Errorf("daemon shutdown already started")
	}
	m.shuttingDown.Store(true)
	cancels := make(map[string]context.CancelCauseFunc, len(m.cancels))
	ids := make([]string, 0, len(m.cancels))
	for id, cancel := range m.cancels {
		cancels[id] = cancel
		ids = append(ids, id)
	}
	m.mu.Unlock()
	cause := &pipeline.ControlledShutdownCause{Instance: m.runtimeInstance, Lifecycle: lifecycle}
	if err := m.db.PrepareControlledShutdown(ids, cause.Instance, cause.Lifecycle, cause.Error()); err != nil {
		m.shuttingDown.Store(false)
		return nil, fmt.Errorf("record planned shutdown: %w", err)
	}
	for _, cancel := range cancels {
		cancel(cause)
	}
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		for _, id := range ids {
			run, err := m.db.GetRun(id)
			if err != nil || run == nil || (run.Status != "completed" && run.Status != "failed" && run.Status != "cancelled") {
				return nil, fmt.Errorf("planned shutdown terminal outcome unavailable for run %s: %v", id, err)
			}
		}
		return &ipc.ShutdownResult{PID: os.Getpid(), StartedAt: m.runtimeStartedAt.UTC().Format(time.RFC3339Nano), OK: true, Instance: cause.Instance, Lifecycle: cause.Lifecycle}, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("planned shutdown did not drain terminal decisions: %w", ctx.Err())
	}
}
