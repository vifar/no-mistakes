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
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	shutdownIdle int32 = iota
	shutdownPlanned
	shutdownRaw
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
	if m.shutdownState.Load() == shutdownRaw {
		m.mu.Unlock()
		return nil, fmt.Errorf("raw daemon shutdown already started")
	}
	preparation := m.shutdownPreparation
	if preparation == nil {
		lifecycle := newShutdownToken()
		if lifecycle == "" {
			m.mu.Unlock()
			return nil, fmt.Errorf("shutdown lifecycle identity is unavailable")
		}
		if !m.shutdownState.CompareAndSwap(shutdownIdle, shutdownPlanned) {
			m.mu.Unlock()
			return nil, fmt.Errorf("daemon shutdown already started")
		}
		cancels := m.runCancelsLocked()
		ids := make([]string, 0, len(cancels))
		dones := make([]chan struct{}, 0, len(cancels))
		for id := range cancels {
			ids = append(ids, id)
			// A consumed handle belongs to a run goroutine counted in wg; an
			// unconsumed one is still in setup, which wg does not cover.
			if handle := m.runs[id].handle; !handle.consumed {
				dones = append(dones, handle.done)
			}
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
			m.shutdownState.CompareAndSwap(shutdownPlanned, shutdownIdle)
			m.mu.Unlock()
			close(preparation.done)
			return nil, preparation.err
		}
		m.stopLimitWatcher()
		m.mu.Lock()
		if m.shutdownState.Load() != shutdownRaw {
			for _, cancel := range cancels {
				cancel(preparation.cause)
			}
		}
		m.mu.Unlock()
		go func() {
			// Run setup is not counted in wg until its executor starts, so the
			// bound runs' own handles are awaited as well.
			for _, done := range dones {
				<-done
			}
			m.wg.Wait()
			close(preparation.done)
		}()
	} else {
		m.mu.Unlock()
	}
	select {
	case <-preparation.done:
		if preparation.err != nil {
			return nil, preparation.err
		}
		// A run parked at an approval gate or monitoring CI is deliberately
		// left non-terminal for the next start's recovery, and queued runs stay
		// queued; every other bound run must have a settled terminal outcome.
		m.mu.Lock()
		left := make(map[string]bool, len(m.leftForRecovery))
		for id := range m.leftForRecovery {
			left[id] = true
		}
		m.mu.Unlock()
		for _, id := range preparation.ids {
			run, err := m.db.GetRun(id)
			if err != nil || run == nil || (!run.Status.Terminal() && !left[id]) {
				return nil, fmt.Errorf("planned shutdown terminal outcome unavailable for run %s: %v", id, err)
			}
		}
		active, err := m.db.GetActiveRuns()
		if err != nil {
			return nil, fmt.Errorf("planned shutdown has unsettled runs: %v", err)
		}
		for _, run := range active {
			if run.Status != types.RunQueued && !left[run.ID] {
				return nil, fmt.Errorf("planned shutdown has unsettled run %s", run.ID)
			}
		}
		if m.shutdownState.Load() == shutdownRaw {
			return nil, fmt.Errorf("raw daemon shutdown already started")
		}
		return &ipc.ShutdownResult{PID: os.Getpid(), StartedAt: m.runtimeStartedAt.UTC().Format(time.RFC3339Nano), OK: true, Instance: preparation.cause.Instance, Lifecycle: preparation.cause.Lifecycle}, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("planned shutdown did not drain terminal decisions: %w", ctx.Err())
	}
}

// stopLimitWatcher stops the live-limit watcher so no queued run is admitted
// once a shutdown has begun, and so a wait on wg can finish.
func (m *RunManager) stopLimitWatcher() {
	m.watchOnce.Do(func() {})
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.watchStop != nil {
		select {
		case <-m.watchStop:
		default:
			close(m.watchStop)
		}
	}
}
