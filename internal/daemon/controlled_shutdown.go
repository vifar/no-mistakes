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

type controlledShutdownPreparation struct {
	cause *pipeline.ControlledShutdownCause
	ids   []string
	done  chan struct{}
	err   error
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
	m.mu.Lock()
	preparation := m.shutdownPreparation
	if preparation == nil {
		if m.shuttingDown.Load() {
			m.mu.Unlock()
			return nil, fmt.Errorf("daemon shutdown already started")
		}
		lifecycle := newShutdownToken()
		if lifecycle == "" {
			m.mu.Unlock()
			return nil, fmt.Errorf("shutdown lifecycle identity is unavailable")
		}
		m.shuttingDown.Store(true)
		cancels := make(map[string]context.CancelCauseFunc, len(m.cancels))
		ids := make([]string, 0, len(m.cancels))
		for id, cancel := range m.cancels {
			cancels[id] = cancel
			ids = append(ids, id)
		}
		preparation = &controlledShutdownPreparation{
			cause: &pipeline.ControlledShutdownCause{Instance: m.runtimeInstance, Lifecycle: lifecycle},
			ids:   ids,
			done:  make(chan struct{}),
		}
		m.shutdownPreparation = preparation
		m.mu.Unlock()
		if err := m.db.PrepareControlledShutdown(ids, preparation.cause.Instance, preparation.cause.Lifecycle, preparation.cause.Error()); err != nil {
			preparation.err = fmt.Errorf("record planned shutdown: %w", err)
			m.mu.Lock()
			m.shutdownPreparation = nil
			m.shuttingDown.Store(false)
			m.mu.Unlock()
			close(preparation.done)
			return nil, preparation.err
		}
		for _, cancel := range cancels {
			cancel(preparation.cause)
		}
		go func() { m.wg.Wait(); close(preparation.done) }()
	} else {
		m.mu.Unlock()
	}
	select {
	case <-preparation.done:
		if preparation.err != nil {
			return nil, preparation.err
		}
		for _, id := range preparation.ids {
			run, err := m.db.GetRun(id)
			if err != nil || run == nil || (run.Status != "completed" && run.Status != "failed" && run.Status != "cancelled") {
				return nil, fmt.Errorf("planned shutdown terminal outcome unavailable for run %s: %v", id, err)
			}
		}
		return &ipc.ShutdownResult{PID: os.Getpid(), StartedAt: m.runtimeStartedAt.UTC().Format(time.RFC3339Nano), OK: true, Instance: preparation.cause.Instance, Lifecycle: preparation.cause.Lifecycle}, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("planned shutdown did not drain terminal decisions: %w", ctx.Err())
	}
}
