package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/eval"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/procreap"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/shipping"
	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/verificationplan"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// StepFactory creates pipeline steps for a run. Defaults to steps.AllSteps.
type StepFactory func() []pipeline.Step

var recoveredConfigFetchTimeout = 10 * time.Second

var fetchRecoveredDefaultBranchSHA = fetchTrustedDefaultBranchSHA

// RunManager tracks active pipeline executors and manages run lifecycle.
type RunManager struct {
	// beforeAnswerRespond is a test scheduling barrier, nil in production.
	beforeAnswerRespond func()

	runtimeInstance  string
	runtimeStartedAt time.Time
	mu               sync.Mutex
	runs             map[string]*managedRun
	wg               sync.WaitGroup // tracks background run goroutines
	shutdownState    atomic.Int32   // prevents new runs during shutdown
	db               *db.DB
	paths            *paths.Paths
	steps            StepFactory

	// shutdownPreparation is the in-flight instance-bound planned shutdown.
	shutdownPreparation *controlledShutdownPreparation
	// leftForRecovery names runs a shutdown deliberately left non-terminal
	// (parked at a gate or monitoring CI) for the next start's recovery.
	leftForRecovery map[string]bool

	notificationMu           sync.Mutex
	afterPushRetained        func()
	beforeNotificationRename func() error
	beforeRunInsert          func()
	afterReceiptLookup       func(*db.Run)
	afterPushCommitted       func()
	branchLocks              sync.Map // repoID+"/"+branch → *sync.Mutex

	// sched enforces daemon.max_concurrent_runs and owns the start order of
	// queued runs; limitsMu/limitsCache memoise the live global config it reads.
	sched *slotScheduler
	// claimQueued moves a dispatched queued run to pending; beforeQueuedRegister
	// runs between the shutdown pre-check and handle registration. Both exist
	// so tests can inject faults and pauses.
	claimQueued          func(runID string) (bool, error)
	markQueued           func(runID, spec string) (int64, error)
	beforeQueuedRegister func()
	// afterSupersedeSnapshot runs between supersession's snapshot of a branch's
	// runs and the cancellation of each; a test seam for that race.
	afterSupersedeSnapshot func()
	// afterFullDecision runs inside admission, between "the cap is full" and the
	// queue insertion. A test seam for the admission race.
	afterFullDecision func()
	// afterExecute and whileFinishing run when a run's Execute returns: before
	// and after it is marked finishing.
	afterExecute   func(runID string)
	whileFinishing func(runID string)
	watchOnce      sync.Once
	watchStop      chan struct{}
	limitsMu       sync.Mutex
	limitsCache    runLimits

	// evalCaptureMu serializes automatic eval collection. Concurrent runs
	// finishing together would otherwise write the same per-repository object
	// pool and the same registry file at once.
	evalCaptureMu sync.Mutex

	// subMu guards the subscriber set and the per-run state revisions. It is
	// a plain Mutex, not an RWMutex, because revision assignment and fan-out
	// must be one atomic step: if two concurrent state events could be
	// enqueued out of revision order, a consumer's monotonic guard would
	// permanently discard the older one's payload. The critical section
	// contains no blocking operation and no I/O, so hold time is
	// O(subscribers) memory writes.
	subMu          sync.Mutex
	subscribers    map[string][]*eventMailbox // runID → subscriber mailboxes
	stateRevs      map[string]int64           // runID → monotonic state revision
	completedRuns  map[string]bool            // runIDs whose goroutines have finished
	completedOrder []string                   // insertion order for FIFO eviction
}

type runOwnership uint8

const (
	// ownershipQueued: durable row, no executor; position lives in the scheduler.
	ownershipQueued runOwnership = iota
	// ownershipClaimed: a cancel handle is registered and the run is launching
	// or executing.
	ownershipClaimed
	// ownershipRecoveringGate: a recovered parked gate that is registered with
	// its executor and cancel handle before it holds (or waits for) a slot.
	ownershipRecoveringGate
	// ownershipFinishing: Execute/Resume has returned and the run's outcome is
	// decided; eval collection and cleanup are still running. Abort and
	// supersede leave it alone so they can never rewrite its terminal status.
	ownershipFinishing
)

// managedRun is the one in-memory record of a run's ownership. RunManager.mu
// guards the table and every state transition; abort, supersede, respond and
// shutdown consult it before the database.
type managedRun struct {
	run      *db.Run
	state    runOwnership
	handle   *runHandle
	executor *pipeline.Executor
}

// maxSubscribersPerRun bounds the global mailbox footprint: queued bytes can
// never exceed activeRuns × maxSubscribersPerRun × mailboxMaxBytes. Refusing
// past the cap is an ordinary error, never unbounded growth.
const maxSubscribersPerRun = 32

// An e2e build can install the same scheduling barrier used by daemon tests.
var defaultBeforeAnswerRespond func()

// NewRunManager creates a RunManager. Pass nil for stepFactory to use default steps.
func NewRunManager(database *db.DB, p *paths.Paths, stepFactory StepFactory) *RunManager {
	if stepFactory == nil {
		stepFactory = func() []pipeline.Step { return steps.AllSteps() }
	}
	m := &RunManager{
		runtimeInstance: newShutdownToken(),
		runs:            make(map[string]*managedRun),
		leftForRecovery: make(map[string]bool),
		db:              database,
		paths:           p,
		steps:           stepFactory,
		subscribers:     make(map[string][]*eventMailbox),
		stateRevs:       make(map[string]int64),
		completedRuns:   make(map[string]bool),
	}
	m.beforeAnswerRespond = defaultBeforeAnswerRespond
	m.limitsCache = runLimits{max: config.DefaultMaxConcurrentRuns}
	m.claimQueued = database.ClaimQueuedRun
	m.markQueued = database.MarkRunQueued
	m.sched = newSlotScheduler(m.liveLimits, &m.mu)
	m.sched.start = m.startQueuedRun
	return m
}

// runLimits is the scheduling slice of the global config.
type runLimits struct {
	loaded  bool
	modTime time.Time
	size    int64
	max     int
	hold    bool
}

// SeedLimits installs the limits from the validated startup configuration
// before any recovery or admission. A config file that is later missing or
// unreadable then keeps these accepted limits instead of falling back to the
// built-in defaults.
func (m *RunManager) SeedLimits(cfg *config.GlobalConfig) {
	if cfg == nil {
		return
	}
	m.limitsMu.Lock()
	m.limitsCache = runLimits{max: cfg.Daemon.MaxConcurrentRuns, hold: cfg.Daemon.HoldSlotWhileParked}
	m.limitsMu.Unlock()
	pipeline.SetMaxConcurrentAgents(cfg.Daemon.MaxConcurrentAgents)
	m.sched.refresh()
}

// liveLimits returns the current daemon.max_concurrent_runs and
// daemon.hold_slot_while_parked. The global file is re-parsed only when its
// mtime or size changes (parsing logs deprecation warnings, and this runs on
// every slot yield), so an edit applies to the next scheduling decision. An
// unreadable file keeps the last good limits.
func (m *RunManager) liveLimits() (int, bool) {
	m.limitsMu.Lock()
	defer m.limitsMu.Unlock()
	path := m.paths.ConfigFile()
	info, err := os.Stat(path)
	if err != nil {
		// A file that vanished (or cannot be inspected) keeps the limits last
		// accepted: parsing a missing file would yield the built-in defaults and
		// silently replace what the operator configured.
		return m.limitsCache.max, m.limitsCache.hold
	}
	if m.limitsCache.loaded && info.ModTime().Equal(m.limitsCache.modTime) && info.Size() == m.limitsCache.size {
		return m.limitsCache.max, m.limitsCache.hold
	}
	cfg, loadErr := config.LoadGlobal(path)
	if loadErr != nil || cfg == nil {
		if loadErr != nil {
			slog.Warn("failed to load global config for run limits; keeping previous limits", "error", loadErr)
		}
		return m.limitsCache.max, m.limitsCache.hold
	}
	m.limitsCache = runLimits{loaded: err == nil, max: cfg.Daemon.MaxConcurrentRuns, hold: cfg.Daemon.HoldSlotWhileParked}
	if err == nil {
		m.limitsCache.modTime, m.limitsCache.size = info.ModTime(), info.Size()
	}
	pipeline.SetMaxConcurrentAgents(cfg.Daemon.MaxConcurrentAgents)
	return m.limitsCache.max, m.limitsCache.hold
}

// limitWatchInterval is how often the daemon notices an edited global config
// and applies a changed run limit without waiting for the next launch or run
// exit. A variable so tests can shorten it.
var limitWatchInterval = 2 * time.Second

// StartLimitWatcher applies edits to daemon.max_concurrent_runs and
// daemon.hold_slot_while_parked without a restart: it re-checks the global
// config file's mtime on a short interval and drains the queue, so a raised
// cap starts queued runs promptly. Lowering the cap never touches a running
// run; it only holds back new admissions until usage drops below it. The
// watcher stops with Shutdown.
func (m *RunManager) StartLimitWatcher() {
	m.watchOnce.Do(func() {
		m.watchStop = make(chan struct{})
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			ticker := time.NewTicker(limitWatchInterval)
			defer ticker.Stop()
			for {
				select {
				case <-m.watchStop:
					return
				case <-ticker.C:
					m.sched.dispatch()
				}
			}
		}()
	})
}

// ReloadLimits re-reads the scheduling limits from the global config now,
// starts whatever queued runs the new cap admits, and returns the resulting
// slot usage. It is what `daemon set` calls over IPC.
func (m *RunManager) ReloadLimits() ipc.SlotsResult {
	m.limitsMu.Lock()
	m.limitsCache.loaded = false
	m.limitsMu.Unlock()
	m.sched.dispatch()
	return m.SlotUsage()
}

// SlotUsage reports the current cap and slot usage.
func (m *RunManager) SlotUsage() ipc.SlotsResult {
	m.sched.refresh()
	st := m.sched.stats()
	return ipc.SlotsResult{
		MaxConcurrentRuns:   st.Max,
		HoldSlotWhileParked: st.Hold,
		InUse:               st.Active,
		Queued:              st.Queued,
		ParkedReleased:      st.Yielded,
	}
}

func (m *RunManager) startQueuedRun(runID string, slot *runSlot) (func(), bool) {
	if m.shutdownState.Load() != shutdownIdle {
		return slot.Release, true
	}
	if m.beforeQueuedRegister != nil {
		m.beforeQueuedRegister()
	}
	if m.shutdownState.Load() != shutdownIdle {
		return slot.Release, true
	}
	claimed, err := m.claimQueued(runID)
	if err != nil {
		slog.Error("failed to claim queued run; leaving it in place", "run_id", runID, "error", err)
		m.sched.retrying = true
		m.wg.Add(1)
		return func() {
			go func() {
				defer m.wg.Done()
				<-time.After(claimRetryDelay)
				m.mu.Lock()
				m.sched.retrying = false
				m.mu.Unlock()
				m.sched.dispatch()
			}()
		}, false
	}
	if !claimed {
		delete(m.runs, runID)
		return slot.Release, true
	}
	runCtx, cancel := context.WithCancelCause(context.Background())
	handle := &runHandle{cancel: cancel, runCtx: runCtx, done: make(chan struct{})}
	m.runs[runID].state = ownershipClaimed
	m.runs[runID].handle = handle
	m.wg.Add(1)
	return func() {
		go func() {
			defer m.wg.Done()
			m.launchQueuedRun(runID, slot, handle)
		}()
	}, true
}

// launchQueuedRun finishes the launch of a run taken off the queue, rebuilding
// everything from its row. Setup failures use failRunSetup to preserve a prior
// cancellation cause; there is no caller left to return them to.
func (m *RunManager) launchQueuedRun(runID string, slot *runSlot, handle *runHandle) {
	defer func() {
		// launchRun transfers the handle to the run goroutine once the
		// executor exists; any earlier exit must unregister it here.
		if r := recover(); r != nil {
			slog.Error("panic launching queued run", "run_id", runID, "panic", r)
			m.failRunSetup(handle.runCtx, runID, false, fmt.Errorf("internal panic: %v", r))
		}
		if !handle.consumed {
			if !m.terminalRunSettled(runID) {
				return
			}
			m.mu.Lock()
			if owned := m.runs[runID]; owned != nil && owned.handle == handle {
				delete(m.runs, runID)
			}
			m.mu.Unlock()
			handle.cancel(nil)
			close(handle.done)
			// Once consumed, the run goroutine owns the slot and frees it.
			slot.Release()
		}
	}()
	if cause := context.Cause(handle.runCtx); cause != nil {
		m.failRunSetup(handle.runCtx, runID, false, cause)
		return
	}
	run, err := m.db.GetRun(runID)
	if err != nil || run == nil {
		slog.Error("queued run is unreadable", "run_id", runID, "error", err)
		if err == nil {
			err = fmt.Errorf("run is missing")
		}
		m.failRunSetup(handle.runCtx, runID, false, fmt.Errorf("queued run is unreadable: %w", err))
		return
	}
	fail := func(err error) {
		slog.Error("queued run cannot start", "run_id", runID, "error", err)
		m.failRunSetup(handle.runCtx, runID, false, err)
	}
	repo, err := m.db.GetRepo(run.RepoID)
	if err != nil || repo == nil {
		if err == nil {
			err = fmt.Errorf("repository is missing")
		}
		fail(fmt.Errorf("queued run repository is missing: %w", err))
		return
	}
	var spec queueSpec
	raw, err := m.db.GetRunQueueSpec(runID)
	if err != nil {
		fail(fmt.Errorf("read queued run spec: %w", err))
		return
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &spec); err != nil {
			fail(fmt.Errorf("decode queued run spec: %w", err))
			return
		}
	}
	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		fail(fmt.Errorf("load config: %w", err))
		return
	}
	_, _ = m.launchRun(handle.runCtx, repo, run, globalCfg, spec.Trigger, spec.SkipSteps, slot, handle)
}

// Its conditional write leaves terminal outcomes alone, and subscribers see
// the saved verdict rather than a competing local failure reason.
// started must become true once worktree creation succeeds: only failures before
// that boundary can release custody without inspecting a managed worktree head.
func (m *RunManager) failRunSetup(ctx context.Context, runID string, started bool, setupErr error) error {
	err := pipeline.SettleTerminalWrite(m.db, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		status, message := types.RunFailed, setupErr.Error()
		cause := context.Cause(ctx)
		if owned := m.runs[runID]; owned != nil && owned.handle != nil {
			if original := context.Cause(owned.handle.runCtx); original != nil {
				cause = original
			}
		}
		cancelled := pipeline.CancellationOnly(setupErr, cause)
		if cancelled {
			status, message = types.RunCancelled, cause.Error()
			var planned *pipeline.ControlledShutdownCause
			if errors.Is(cause, pipeline.ErrDaemonShutdown) && !errors.As(cause, &planned) {
				status = types.RunFailed
			}
		}
		revoked, err := m.db.RawShutdownSuperseded(runID)
		if err != nil {
			return err
		}
		if revoked && cancelled {
			status, message = types.RunFailed, db.RawShutdownReason
		}
		terminate := m.db.TerminateActiveRun
		if !started {
			terminate = m.db.TerminateUnstartedRun
		}
		if _, err := terminate(runID, message, status); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		slog.Error("setup terminal outcome unsettled; retaining run custody", "run_id", runID, "error", err)
		return err
	}
	run, err := pipeline.ReadSettledRun(m.db, runID)
	if err != nil {
		return fmt.Errorf("read setup terminal outcome for run %s: %w", runID, err)
	}
	if run == nil || !run.Status.Terminal() {
		return fmt.Errorf("setup terminal outcome unavailable for run %s", runID)
	}
	reason := ""
	if run.Error != nil {
		reason = *run.Error
	}
	m.broadcastTerminal(run, run.Status, reason)
	return nil
}

func (m *RunManager) failUnadmittedRunSetup(ctx context.Context, run *db.Run, setupErr error) error {
	m.mu.Lock()
	m.runs[run.ID] = &managedRun{run: run, state: ownershipFinishing}
	m.mu.Unlock()
	if err := m.failRunSetup(ctx, run.ID, false, setupErr); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.runs, run.ID)
	m.mu.Unlock()
	return nil
}

// claimRetryDelay spaces retries of a queued run whose claim failed, so a
// persistent database fault cannot spin the queue. A variable for tests.
var claimRetryDelay = time.Second

// broadcastTerminal tells subscribers a run that never reached its executor
// ended, and closes its streams.
func (m *RunManager) broadcastTerminal(run *db.Run, status types.RunStatus, reason string) {
	s := string(status)
	m.broadcast(ipc.Event{
		Type:   ipc.EventRunCompleted,
		RunID:  run.ID,
		RepoID: run.RepoID,
		Status: &s,
		Branch: &run.Branch,
		Error:  &reason,
	})
	m.closeSubscribers(run.ID)
}

// markFinishing records that a run's Execute/Resume has returned. Its outcome
// is decided from here on, so cancellation no longer applies to it.
// afterExecute and whileFinishing are test seams around the transition.
func (m *RunManager) markFinishing(runID string) {
	if m.afterExecute != nil {
		m.afterExecute(runID)
	}
	m.mu.Lock()
	if owned := m.runs[runID]; owned != nil {
		owned.state = ownershipFinishing
	}
	m.mu.Unlock()
	if m.whileFinishing != nil {
		m.whileFinishing(runID)
	}
}

// annotateQueue stamps the run's place in the start order onto a snapshot.
func (m *RunManager) annotateQueue(info *ipc.RunInfo) *ipc.RunInfo {
	m.sched.annotate(info)
	return info
}

// loadQueuedRuns reads and validates the durable queue after a restart. It is
// read-only and all-or-nothing: either every queued row is returned in start
// order, or an error is, with no side effect on the database or the manager.
//
// Daemon startup runs it BEFORE any recovery work and fails on its error. A
// queued row that never reaches the ownership table can be neither started nor
// aborted (abort and supersede consult the table first), so serving without it
// would strand durable rows; and failing only after parked gates were resumed
// would cancel them and persist the shutdown as their terminal failure. Failing
// first leaves every gate parked and recoverable on the next start.
func (m *RunManager) loadQueuedRuns() ([]*db.Run, error) {
	ids, err := m.db.QueuedRunIDs()
	if err != nil {
		return nil, fmt.Errorf("list queued runs: %w", err)
	}
	runs := make([]*db.Run, 0, len(ids))
	for _, id := range ids {
		run, err := m.db.GetRun(id)
		if err != nil {
			return nil, fmt.Errorf("read queued run %s: %w", id, err)
		}
		if run == nil {
			return nil, fmt.Errorf("queued run %s disappeared", id)
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// holdQueue keeps queued runs from starting until releaseQueue. Daemon startup
// holds it before anything can dispatch.
func (m *RunManager) holdQueue() {
	m.mu.Lock()
	m.sched.held = true
	m.mu.Unlock()
}

// restoreQueuedRuns registers loaded queued rows in the ownership table and the
// scheduler, in start order. Queued runs were never executing, so they stay
// queued across a restart. It runs after recovered gates are resumed, so those
// gates are already waiting for (or holding) slots and keep priority over the
// restored new runs. Nothing is claimed or started here: startup holds the
// queue until IPC is serving (see releaseQueue).
func (m *RunManager) restoreQueuedRuns(runs []*db.Run) {
	if len(runs) == 0 {
		return
	}
	ids := make([]string, 0, len(runs))
	m.mu.Lock()
	for _, run := range runs {
		m.runs[run.ID] = &managedRun{run: run, state: ownershipQueued}
		ids = append(ids, run.ID)
	}
	m.mu.Unlock()
	slog.Info("restored queued runs", "count", len(ids))
	m.sched.restoreQueue(ids)
}

// releaseQueue ends startup's hold: queued runs start, and the config watcher
// begins. Daemon startup calls it only once IPC has bound and answered health,
// so a startup that fails earlier leaves every queued row queued for the next.
func (m *RunManager) releaseQueue() {
	m.mu.Lock()
	m.sched.held = false
	m.mu.Unlock()
	m.sched.dispatch()
	m.StartLimitWatcher()
}

// cancelManagedRun is the single cancellation path for abort and supersede. It
// consults only the ownership table first:
//
//   - queued: the cancellation is persisted first; only on success is the run
//     dequeued. On error it stays queued in place and abortable.
//   - claimed or recovering gate: the cancel handle is invoked at once, with no
//     database access before it, so a failing database cannot keep a run alive
//     (and a recovering gate needs no slot to be cancelled).
//   - finishing: the outcome is already decided, so nothing is cancelled or
//     rewritten.
//
// It returns the channel that closes when the cancelled run's workload has
// stopped, or nil when there is nothing to wait for (a never-started queued run,
// a finishing run, an unknown id). The state is read and acted on under one
// hold of the ownership lock, so the channel is always the handle that was
// actually signalled: a queued run that dispatch claimed a moment earlier is
// cancelled as claimed, with its new handle and completion channel returned.
func (m *RunManager) cancelManagedRun(runID, reason string) (<-chan struct{}, error) {
	m.mu.Lock()
	owned := m.runs[runID]
	if owned == nil {
		m.mu.Unlock()
		return nil, nil
	}
	switch owned.state {
	case ownershipQueued:
		cancelled, err := m.db.CancelQueuedRun(runID, reason)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if !cancelled {
			m.mu.Unlock()
			return nil, fmt.Errorf("queued run %s could not be cancelled", runID)
		}
		m.sched.removeQueuedLocked(runID)
		delete(m.runs, runID)
		m.mu.Unlock()
		m.broadcastTerminal(owned.run, types.RunCancelled, reason)
		return nil, nil
	case ownershipFinishing:
		m.mu.Unlock()
		return nil, nil
	}
	owned.handle.cancel(errors.New(reason))
	done := owned.handle.done
	m.mu.Unlock()
	return done, nil
}

type recoveredRunPlan struct {
	run     *db.Run
	repo    *db.Repo
	workDir string
	gateDir string
	cfg     *config.Config
	agent   agent.Agent
	steps   []pipeline.Step
	forge   *forgecontext.Context
}

func (m *RunManager) recoverableParkedRuns(ctx context.Context) []recoveredRunPlan {
	runs, err := m.db.GetActiveRuns()
	if err != nil {
		slog.Error("failed to list active runs for recovery", "error", err)
		return nil
	}
	plans := make([]recoveredRunPlan, 0, len(runs))
	branchCounts := make(map[string]int, len(runs))
	for _, run := range runs {
		branchCounts[run.RepoID+"\x00"+run.Branch]++
	}
	for _, run := range runs {
		if branchCounts[run.RepoID+"\x00"+run.Branch] != 1 {
			slog.Warn("active run cannot be safely resumed", "run_id", run.ID, "error", "conflicting active run for branch")
			continue
		}
		plan, err := m.prepareRecoveredRun(ctx, run)
		if err != nil {
			slog.Warn("active run cannot be safely resumed", "run_id", run.ID, "error", err)
			continue
		}
		plans = append(plans, *plan)
	}
	return plans
}

func (m *RunManager) prepareRecoveredRun(ctx context.Context, run *db.Run) (*recoveredRunPlan, error) {
	if run == nil || run.Status != types.RunRunning || run.AwaitingAgentSince == nil || run.Branch == "" {
		return nil, fmt.Errorf("run is not a parked running run")
	}
	repo, err := m.db.GetRepo(run.RepoID)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("run repository is missing")
	}
	workDir := worktrees.RecordedDir(m.paths, run.WorktreePath(), repo.ID, run.ID)
	if info, err := os.Stat(workDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("worktree is missing")
	}
	headSHA, err := git.HeadSHA(ctx, workDir)
	if err != nil || headSHA != run.HeadSHA {
		return nil, fmt.Errorf("worktree head does not match run head")
	}
	gateDir := m.paths.RepoDir(repo.ID)
	commonDir, err := git.Run(ctx, workDir, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("resolve worktree common git dir: %w", err)
	}
	if !samePath(resolveGitPath(workDir, commonDir), gateDir) {
		return nil, fmt.Errorf("worktree does not belong to its gate repository")
	}

	cfg, err := m.loadRecoveredConfig(ctx, run, repo, workDir)
	if err != nil {
		return nil, err
	}
	execSteps := steps.WithCustomGates(m.steps(), cfg.Gates)
	if err := pipeline.ValidateRecoveredRun(m.db, run, execSteps); err != nil {
		return nil, err
	}
	forgeCtx, err := forgecontext.Resolve(ctx, cfg.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
	if err == nil {
		err = forgecontext.RefuseProviderPluginOverlap(ctx, forgeCtx, cfg.ProviderPlugins, repo.UpstreamURL)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve forge profile: %w", err)
	}
	var obsolete bool
	if err := pipeline.WithCommitStatusLock(ctx, func() error {
		var err error
		obsolete, err = m.db.ReconcileRecoveredRun(run.ID, m.runtimeInstance)
		return err
	}); err != nil {
		return nil, err
	}
	if obsolete {
		return nil, fmt.Errorf("unsettled shutdown from previous daemon runtime")
	}
	ag, err := newPipelineAgent(ctx, cfg, m.paths.EvidenceRoot(cfg.Test.Evidence.LocalRoot), exec.LookPath, forgeEnvironment(forgeCtx))
	if err != nil {
		return nil, err
	}
	if cfg.SessionReuse {
		if err := validateRecoveredSessionProviders(m.db, run.ID, ag); err != nil {
			_ = ag.Close()
			return nil, err
		}
	}
	return &recoveredRunPlan{
		run:     run,
		repo:    repo,
		workDir: workDir,
		gateDir: gateDir,
		cfg:     cfg,
		agent:   ag,
		steps:   execSteps,
		forge:   forgeCtx,
	}, nil
}

func validateRecoveredSessionProviders(database *db.DB, runID string, ag agent.Agent) error {
	sessions, err := database.GetRunAgentSessions(runID)
	if err != nil {
		return fmt.Errorf("get run sessions: %w", err)
	}
	for _, session := range sessions {
		if session.Role != string(pipeline.SessionRoleReviewer) && session.Role != string(pipeline.SessionRoleFixer) {
			return fmt.Errorf("recovered run has unknown session role %q", session.Role)
		}
		if session.Agent == "" || session.SessionID == "" {
			return fmt.Errorf("recovered run has incomplete session metadata")
		}
		if session.Role == string(pipeline.SessionRoleFixer) && !agent.SupportsSessionProvider(ag, session.Agent) {
			return fmt.Errorf("session provider %q is no longer configured", session.Agent)
		}
	}
	return nil
}

func (m *RunManager) loadRecoveredConfig(ctx context.Context, run *db.Run, repo *db.Repo, workDir string) (*config.Config, error) {
	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	repoCfg, err := config.LoadRepo(workDir)
	if err != nil {
		return nil, fmt.Errorf("load repo config: %w", err)
	}
	var trustedSHA string
	if repo.DefaultBranch != "" {
		fetchCtx, cancel := context.WithTimeout(ctx, recoveredConfigFetchTimeout)
		defer cancel()
		if sha, err := fetchRecoveredDefaultBranchSHA(fetchCtx, workDir, repo); err != nil {
			slog.Warn("failed to fetch default branch while recovering run; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else {
			trustedSHA = sha
		}
	}
	// SECURITY: a trusted-config fetch failure must abort, not silently disable
	// the disable_project_settings opt-out (see assertGateTrustedConfigReadable).
	if err := assertGateTrustedConfigReadable(ctx, workDir, repo.DefaultBranch, trustedSHA); err != nil {
		return nil, err
	}
	trustedRepoCfg := loadTrustedRepoConfig(ctx, workDir, trustedSHA, run.ID)
	allowRepoCommands := trustedRepoCfg != nil && trustedRepoCfg.AllowRepoCommands
	effectiveRepoCfg := config.EffectiveRepoConfig(repoCfg, trustedRepoCfg, allowRepoCommands)
	cfg := config.MergeForRemote(globalCfg, effectiveRepoCfg, repo.UpstreamURL)
	if err := cfg.Review.ValidatePathInstructionsBudget(); err != nil {
		return nil, err
	}
	// Gates are read back from the run, never re-resolved. Everything else here
	// is deliberately re-read from the live default branch, but a gate decides
	// which steps the run HAS: the default branch may have gained or lost one
	// since this run parked, and rebuilding the sequence from the current list
	// would leave recovery matching the run's recorded steps against a sequence
	// it never executed - failing a healthy parked run as a crash.
	gates, err := m.pinnedRunGates(run.ID)
	if err != nil {
		return nil, err
	}
	cfg.Gates = gates
	if err := cfg.ApplyPiProfile(run.PiProfile); err != nil {
		return nil, err
	}
	if err := m.paths.ValidateEvidenceRoot(cfg.Test.Evidence.LocalRoot); err != nil {
		return nil, err
	}
	cfg.TrustedConfigSHA = trustedSHA
	if globalCfg.Eval.CaptureProvenance {
		if err := cfg.EnableEvalProvenance(globalCfg, effectiveRepoCfg); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// pinnedRunGates reads back the gate list resolved during admitted-run setup.
// An absent pin means the bare core pipeline - the only sequence a run created before
// gates were pinned can have had - while an unusable one fails its caller
// closed with a reason, because silently dropping it would resume the run
// against a shorter pipeline than the one it recorded.
func (m *RunManager) pinnedRunGates(runID string) ([]config.Gate, error) {
	payload, err := m.db.GetRunGates(runID)
	if err != nil {
		return nil, fmt.Errorf("read pinned gates: %w", err)
	}
	gates, err := config.ParseGates(payload)
	if err != nil {
		return nil, fmt.Errorf("pinned gates are unusable: %w", err)
	}
	return gates, nil
}

func newPipelineAgent(ctx context.Context, cfg *config.Config, evidenceRoot string, lookPath func(string) (string, error), environment runenv.Overlay) (agent.Agent, error) {
	if steps.IsDemoMode() {
		return agent.NewNoop(), nil
	}
	primary, err := newConfiguredAgent(ctx, cfg, evidenceRoot, lookPath, environment)
	if err != nil {
		return nil, err
	}
	roles := make(map[string]agent.Agent, len(cfg.ReviewAgents))
	for _, role := range config.ReviewAgentRoles {
		entry, ok := cfg.ReviewAgents[role]
		if !ok {
			continue
		}
		next, err := newConfiguredAgent(ctx, cfg.ForReviewAgent(entry), evidenceRoot, lookPath, environment)
		if err != nil {
			_ = primary.Close()
			for _, existing := range roles {
				_ = existing.Close()
			}
			return nil, fmt.Errorf("create review_agents.%s: %w", role, err)
		}
		roles[role] = next
	}
	return agent.WithReviewRoles(primary, agent.ReviewRoles{
		Reviewer: agent.RoundedRole{
			Agent:    roles[config.RoleReviewer],
			Late:     roles[config.RoleReviewerAfterRound],
			LateFrom: cfg.ReviewAgentTakeoverRound(config.RoleReviewerAfterRound),
		},
		Fixer: agent.RoundedRole{
			Agent:    roles[config.RoleFixer],
			Late:     roles[config.RoleFixerAfterRound],
			LateFrom: cfg.ReviewAgentTakeoverRound(config.RoleFixerAfterRound),
		},
	}), nil
}

func newConfiguredAgent(ctx context.Context, cfg *config.Config, evidenceRoot string, lookPath func(string) (string, error), environment runenv.Overlay) (agent.Agent, error) {
	if err := cfg.ResolveAgent(ctx, lookPath); err != nil {
		return nil, err
	}
	agents := cfg.Agents
	if len(agents) == 0 {
		agents = []types.AgentName{cfg.Agent}
	}
	created := make([]agent.Agent, 0, len(agents))
	for _, name := range agents {
		next, err := agent.NewWithOptions(name, cfg.AgentPathFor(name), cfg.AgentArgsFor(name), agent.Options{
			ACPRegistryOverrides:   cfg.ACPRegistryOverrides,
			DisableProjectSettings: cfg.DisableProjectSettings,
			Profile:                cfg.AgentProfileFor(name),
			Environment:            environment,
		})
		if err != nil {
			for _, existing := range created {
				_ = existing.Close()
			}
			return nil, fmt.Errorf("create agent %s: %w", name, err)
		}
		created = append(created, agent.WithSteering(next, evidenceRoot))
	}
	ag := agent.NewFallback(created)
	// Fail closed ONLY under the trusted opt-out (see startRun): refuse an
	// unverified harness when the repo disabled project settings; otherwise run
	// every adapter as before.
	if cfg.DisableProjectSettings {
		if err := agent.EnsureGateNeutralized(ag); err != nil {
			_ = ag.Close()
			return nil, err
		}
	}
	return ag, nil
}

func forgeEnvironment(ctx *forgecontext.Context) runenv.Overlay {
	if ctx == nil {
		return runenv.Overlay{}
	}
	return ctx.Environment
}

func resolveGitPath(workDir, value string) string {
	value = strings.TrimSpace(value)
	if !filepath.IsAbs(value) {
		value = filepath.Join(workDir, value)
	}
	return filepath.Clean(value)
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	return a == b
}

func (m *RunManager) resumeRecoveredRuns(plans []recoveredRunPlan) {
	for _, plan := range plans {
		m.resumeRecoveredRun(plan)
	}
}

func hasRebaseCoreStep(plan []pipeline.Step) bool {
	for _, step := range plan {
		if _, ok := step.(*steps.RebaseStep); ok {
			return true
		}
	}
	return false
}

func (m *RunManager) resumeRecoveredRun(plan recoveredRunPlan) {
	if m.shutdownState.Load() != shutdownIdle {
		_ = plan.agent.Close()
		return
	}
	runCtx, cancel := context.WithCancelCause(context.Background())
	slot := &runSlot{s: m.sched, runID: plan.run.ID}
	executor := pipeline.NewExecutor(m.db, m.paths, plan.cfg, plan.agent, plan.steps, m.broadcast)
	executor.SetRunSlot(slot)
	executor.SetRuntimeInstance(m.runtimeInstance)
	executor.SetOnPRMerged(func(_ context.Context, runID string) {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.relabelEvalRun(context.Background(), plan.cfg, runID)
		}()
	})
	executor.SetForgeContext(plan.forge)
	if hasRebaseCoreStep(plan.steps) {
		executor.SetPRContextGuard(steps.GuardPRContext)
	}
	// Registration inspects step implementations before the background panic
	// handler owns this run. Convert a panic here into the same durable failure
	// path as a registration error instead of crashing daemon recovery.
	registerErr := func() (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("internal panic: %v", recovered)
			}
		}()
		return executor.RegisterRecoveredGate(plan.run)
	}()
	if registerErr == nil {
		slot.Reserve()
	}
	done := make(chan struct{})
	m.mu.Lock()
	// Shutdown admission is fenced under m.mu: once shutdown began, a parked
	// run that has not been registered stays parked for the next start, and
	// one that has is counted in wg before shutdown can wait on it.
	if m.shutdownState.Load() != shutdownIdle {
		m.mu.Unlock()
		cancel(pipeline.ErrDaemonShutdown)
		_ = plan.agent.Close()
		return
	}
	m.runs[plan.run.ID] = &managedRun{run: plan.run, state: ownershipRecoveringGate, handle: &runHandle{runCtx: runCtx, cancel: cancel, done: done}, executor: executor}
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		startedAt := time.Now()
		defer m.wg.Done()
		parkedAtShutdown := false
		defer slot.Release()
		defer func() {
			if recovered := recover(); recovered != nil {
				errMsg := fmt.Sprintf("internal panic: %v", recovered)
				if err := pipeline.RecordRunFailure(context.Background(), m.db, plan.run.ID, errMsg, nil); err != nil {
					slog.Error("failed to update recovered run after panic", "run_id", plan.run.ID, "error", err)
				} else {
					plan.run.Status = types.RunFailed
					plan.run.Error = &errMsg
					if err := executor.ReportSettledRunFailure(context.Background(), plan.run, plan.repo); err != nil {
						slog.Error("recovered terminal notification failed", "run_id", plan.run.ID, "error", err)
					}
				}
			} else if plan.run.Status == types.RunFailed || plan.run.Status == types.RunCancelled {
				executor.ReportPublishedHeadFailure(context.Background(), plan.run, plan.repo)
			}
			// A run left for recovery keeps its non-terminal record by design;
			// any other run keeps custody until its terminal outcome settles.
			if parkedAtShutdown {
				if !m.releaseShutdownForRecovery(plan.run.ID) {
					return
				}
			} else if !m.terminalRunSettled(plan.run.ID) {
				return
			}
			cancel(nil)
			_ = plan.agent.Close()
			m.closeSubscribers(plan.run.ID)
			if !parkedAtShutdown {
				m.removeRunWorktree(plan.repo.ID, plan.run.ID, plan.gateDir, plan.workDir, "resumed_run_finished")
				// A recovered run is a finished run too. This is the second of the
				// two completion boundaries, and leaving it out is what let a run
				// resumed after a daemon restart keep its empty evidence directory
				// until some later run or restart happened to sweep it.
				m.cleanupRunEvidence(plan.cfg, plan.run.ID)
			}
			m.mu.Lock()
			delete(m.runs, plan.run.ID)
			m.mu.Unlock()
			close(done)
		}()

		if registerErr == nil {
			_ = slot.Acquire(runCtx)
		}
		m.mu.Lock()
		m.runs[plan.run.ID].state = ownershipClaimed
		m.mu.Unlock()
		resumeErr := registerErr
		if resumeErr == nil {
			resumeErr = executor.Resume(runCtx, plan.run, plan.repo, plan.workDir)
		}
		// Mark finishing before releasing: Release dispatches queued runs
		// synchronously, and a push landing in that window must not treat
		// this already-decided run as a claimed one to cancel and wait on.
		m.markFinishing(plan.run.ID)
		slot.Release()
		if pipeline.LeftForRecoveryAtShutdown(resumeErr) {
			parkedAtShutdown = true
			slog.Info("recovered run left for recovery after daemon restart", "run_id", plan.run.ID, "reason", resumeErr)
			return
		} else if err := resumeErr; err != nil {
			if plan.run.Status == types.RunRunning {
				if failureErr := executor.FailRun(plan.run, plan.repo, err, runCtx); pipeline.LeftForRecoveryAtShutdown(failureErr) {
					parkedAtShutdown = true
					return
				}
			}
			if sqlitewrite.IsContention(err) {
				slog.Error("recovered pipeline infrastructure failure", "run_id", plan.run.ID, "error", err)
			} else {
				slog.Error("recovered pipeline failed", "run_id", plan.run.ID, "error", err)
			}
		}
		fields := telemetry.Fields{
			"action":      "finished",
			"trigger":     "recovery",
			"agent":       string(plan.cfg.Agent),
			"branch_role": telemetryBranchRole(plan.run.Branch, plan.repo.DefaultBranch),
			"status":      string(plan.run.Status),
			"duration_ms": time.Since(startedAt).Milliseconds(),
			"step_count":  len(plan.steps),
			"pr_created":  plan.run.PRURL != nil && *plan.run.PRURL != "",
		}
		if failedStep := telemetryFailedStepName(m.db, plan.run.ID); failedStep != "" {
			fields["failed_step"] = failedStep
		}
		addRunPerformanceSummary(m.db, plan.run.ID, fields)
		telemetry.Track("run", fields)
		m.autoIngestCIFalseNegatives(runCtx, plan.cfg, plan.run.ID)
	}()
}

func agentListsEqual(a, b []types.AgentName) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Subscribe registers a subscriber mailbox for a run.
//
// The returned subscription always opens with a stream-gap frame, so a
// subscriber's first action is always one authoritative read. That makes
// attach and reconnect converge without each consumer needing its own
// subscribe-then-reconcile ordering rule. A run that has already completed
// yields that one gap and then finishes.
func (m *RunManager) Subscribe(runID string) (*Subscription, error) {
	m.subMu.Lock()
	defer m.subMu.Unlock()

	mb := newEventMailbox(runID, m.stateRevs[runID])
	if m.completedRuns[runID] {
		mb.close()
		return &Subscription{mb: mb, unsub: func() {}}, nil
	}
	if len(m.subscribers[runID]) >= maxSubscribersPerRun {
		return nil, fmt.Errorf("run %s already has the maximum of %d event subscribers", runID, maxSubscribersPerRun)
	}
	m.subscribers[runID] = append(m.subscribers[runID], mb)

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			m.subMu.Lock()
			subs := m.subscribers[runID]
			for i, s := range subs {
				if s == mb {
					m.subscribers[runID] = append(subs[:i], subs[i+1:]...)
					break
				}
			}
			if len(m.subscribers[runID]) == 0 {
				delete(m.subscribers, runID)
			}
			m.subMu.Unlock()
			mb.release()
		})
	}
	return &Subscription{mb: mb, unsub: unsub}, nil
}

// Subscription is one subscriber's view of a run's event stream. It owns no
// goroutine: the caller drives it with Next.
type Subscription struct {
	mb    *eventMailbox
	unsub func()
}

// Next blocks until the next frame is available and returns it. ok is false
// once the stream is finished or ctx is done.
func (s *Subscription) Next(ctx context.Context) (ipc.Event, bool) { return s.mb.next(ctx) }

// Close unsubscribes and releases every retained payload. It is idempotent.
func (s *Subscription) Close() { s.unsub() }

// StateRev returns the current monotonic state revision for a run.
//
// A caller serving an authoritative snapshot must sample this BEFORE reading
// the database. Every producer writes state and only then broadcasts, so a
// revision sampled first is never newer than the snapshot that follows it:
// every event at or below it is already reflected in that read, and every
// event above it still reaches the subscriber and still applies on top.
func (m *RunManager) StateRev(runID string) int64 {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	return m.stateRevs[runID]
}

// broadcast stamps a state revision and publishes an event to every subscriber
// of the event's run. It performs no blocking channel operation and no I/O, so
// the executor can never be stalled by a slow or dead subscriber.
func (m *RunManager) broadcast(event ipc.Event) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	if ipc.ClassOf(event.Type) == ipc.ClassState {
		m.stateRevs[event.RunID]++
		event.StateRev = m.stateRevs[event.RunID]
	}
	for _, mb := range m.subscribers[event.RunID] {
		mb.publish(event)
	}
}

// sweepRunWorktreeProcesses terminates whatever is still standing in a
// finished run's worktree. Cancelling the run context tears down each step's
// process group, but a descendant that called setsid(2) is no longer in any
// group the pipeline can name - only the worktree it is standing in still
// identifies it (see internal/procreap).
//
// The run's own worktree is what the sweep is pointed at, so a placement
// outside the default tree needs no configuration lookup and cannot be hidden
// by a worktree_roots edit made while the run was executing. The ordering rule
// and its rationale live on procreap.SweepRunWorktree, which every removal site
// in every package goes through.
func (m *RunManager) sweepRunWorktreeProcesses(repoID, runID, wtDir string) {
	procreap.SweepRunWorktree(m.paths.WorktreesDir(), repoID, runID, wtDir, "run_cleanup")
}

// cleanupRunEvidence tidies up after one finished run, then bounds the whole
// evidence directory, the run's own step-log directory (see reapRunLogs), and
// any leftover run worktree that immediate removal did not clear (see
// reapWorktrees).
//
// The per-run half is deliberately os.Remove and not os.RemoveAll: it succeeds
// only when the directory is empty, so a run that produced no artifact leaves
// nothing behind while a run that did keeps every file. The test step creates
// the directory before the agent decides whether it has evidence to write, so
// without this nearly every run left a permanent empty directory - that alone
// was the overwhelming majority of the accumulation this reaper exists to stop.
//
// The sweeps that follow keep a long-lived daemon converging on both retention
// budgets instead of waiting for a restart. All of this is best effort: losing
// a cleanup pass costs disk, while failing a finished run over it would cost
// the user their result.
func (m *RunManager) cleanupRunEvidence(cfg *config.Config, runID string) {
	configured := ""
	policy := evidenceReapPolicy{
		Retention: config.DefaultEvidenceRetention,
		MaxRuns:   config.DefaultEvidenceMaxRuns,
	}
	wtPolicy := worktreeReapPolicy{
		Retention: config.DefaultWorktreeRetention,
		MaxRuns:   config.DefaultWorktreeMaxRuns,
	}
	if cfg != nil {
		configured = cfg.Test.Evidence.LocalRoot
		policy = evidenceReapPolicy{
			Retention: cfg.Test.Evidence.Retention,
			MaxRuns:   cfg.Test.Evidence.MaxRuns,
		}
		wtPolicy = worktreeReapPolicy{
			Retention: cfg.Worktree.Retention,
			MaxRuns:   cfg.Worktree.MaxRuns,
		}
	}
	root := m.paths.EvidenceRoot(configured)
	pruneEmptyEvidenceGenerations(filepath.Join(root, runID))
	if err := os.Remove(filepath.Join(root, runID)); err != nil && !os.IsNotExist(err) {
		slog.Debug("run evidence kept", "run_id", runID, "reason", err)
	}
	now := time.Now()
	reapEvidence(m.db, root, policy, now)
	reapWorktrees(m.db, m.paths, wtPolicy, now)
	reapRunLogs(m.db, m.paths.LogsDir(), policy, now)
}

// removeRunWorktree sweeps processes before deciding whether to remove the
// directory, so refusal retention cannot keep escaped workers alive.
//
// Every removal of a run worktree this package performs goes through here, and
// none calls git.WorktreeRemove directly, because the ordering is easy to forget
// at one site and invisible when forgotten - a run whose setup failed, whose
// execution returned, or which was resumed after a crash all reach this point by
// different routes. reason distinguishes the routes in the log.
func (m *RunManager) removeRunWorktree(repoID, runID, gateDir, wtDir, reason string) {
	m.sweepRunWorktreeProcesses(repoID, runID, wtDir)
	run, err := m.db.GetRun(runID)
	if err != nil {
		slog.Warn("preserving run worktree: cannot read run", "run_id", runID, "error", err)
		return
	}
	if refusal := worktreeRefusalCleanupReason(m.db, run); refusal != "" {
		slog.Warn("preserving run worktree", "run_id", runID, "path", wtDir, "reason", refusal)
		return
	}
	if err := git.WorktreeRemove(context.Background(), gateDir, wtDir); err != nil {
		slog.Warn("failed to remove run worktree", "reason", reason, "run_id", runID, "path", wtDir, "error", err)
	}
}

// closeSubscribers soft-closes every subscriber for a run and marks the run
// completed so future Subscribe calls return a gapped, immediately-finished
// subscription. Soft close still drains queued frames and any pending gap, so
// a coalesced terminal transition cannot be discarded by completion.
func (m *RunManager) closeSubscribers(runID string) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	for _, mb := range m.subscribers[runID] {
		mb.close()
	}
	delete(m.subscribers, runID)
	m.completedRuns[runID] = true
	m.completedOrder = append(m.completedOrder, runID)
	if len(m.completedOrder) > 1000 {
		half := len(m.completedOrder) / 2
		for _, id := range m.completedOrder[:half] {
			delete(m.completedRuns, id)
			delete(m.stateRevs, id)
		}
		m.completedOrder = m.completedOrder[half:]
	}
}

// ownedGateRepoID extracts the repo id from a gate path and refuses a gate this
// root does not own. Defense in depth behind paths.ForGate, which resolves a
// hook call's root from the gate path itself: the gate path carries the root
// that owns it, but repoIDFromGatePath keeps only the basename, so a caller
// that handed this daemon a gate under a different root - a hand-run CLI or a
// direct IPC client - would otherwise re-resolve that id under this daemon's
// own root, admitting or validating a foreign repository's push against local
// state. The --gate value arrives absolute and symlink-resolved from git
// rev-parse while the owned path is built from NM_HOME exactly as spelled, so
// compare through sameGateDir - canonicalRoot's "same root" (relative against
// absolute, symlinked against real, /var -> /private/var on macOS, case on
// Windows) plus a filesystem identity check for spellings that differ only in
// letter case on a case-insensitive volume - rather than textually.
func ownedGateRepoID(p *paths.Paths, gate string) (string, error) {
	repoID, err := repoIDFromGatePath(gate)
	if err != nil {
		return "", err
	}
	if owned := p.RepoDir(repoID); !sameGateDir(gate, owned) {
		return "", fmt.Errorf("gate %q does not belong to this daemon's home (this root owns %q)", gate, owned)
	}
	return repoID, nil
}

// sameGateDir reports whether two paths name the same gate directory. The
// canonical spellings are compared first; when they differ, the two directories
// must be the same on-disk object (os.SameFile). That follows the filesystem's
// own case rule: a macOS home registered as /Users/Name but stored as
// /Users/name resolves to one directory, while on a case-sensitive volume the
// same two spellings are different directories and stay refused. A path that
// cannot be stat'ed is never the same, so the guard stays fail-closed.
func sameGateDir(a, b string) bool {
	if canonicalRoot(a) == canonicalRoot(b) {
		return true
	}
	infoA, err := os.Stat(a)
	if err != nil {
		return false
	}
	infoB, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(infoA, infoB)
}

// repoIDFromGatePath extracts the repo ID from a gate bare repo path.
// Gate paths look like: <root>/repos/<id>.git
func repoIDFromGatePath(gatePath string) (string, error) {
	base := filepath.Base(gatePath)
	if !strings.HasSuffix(base, ".git") {
		return "", fmt.Errorf("invalid gate path: %s", gatePath)
	}
	return strings.TrimSuffix(base, ".git"), nil
}

// branchFromRef extracts the branch name from a full git ref.
// "refs/heads/main" → "main", "main" → "main"
func branchFromRef(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// loadTrustedRepoConfig reads .no-mistakes.yaml from the trusted
// default-branch commit (trustedSHA - a freshly fetched and resolved SHA)
// in the worktree and parses it. Reading at a pinned SHA, rather
// than the origin/<defaultBranch> remote-tracking ref, closes the stale-ref
// hole: the gate worktree shares refs with the bare repo, so without a fresh
// fetch + resolve the ref could point at a commit a previous run left behind.
//
// trustedSHA is empty when the default branch is unknown, the fetch failed,
// or the ref did not resolve. The caller must first reject those cases with
// assertGateTrustedConfigReadable; returning nil here remains defensive and
// ensures EffectiveRepoConfig never uses pushed gate-control fields.
func loadTrustedRepoConfig(ctx context.Context, wtDir, trustedSHA, runID string) *config.RepoConfig {
	if trustedSHA == "" {
		// No trusted SHA means no freshly-fetched default-branch commit to
		// read from. Return nil so EffectiveRepoConfig forces empty
		// commands/agent - the secure default - instead of falling back to a
		// potentially stale origin/<defaultBranch> ref.
		return nil
	}
	content, err := git.ShowFile(ctx, wtDir, trustedSHA, ".no-mistakes.yaml")
	if err != nil {
		// Path absent on the default branch is the common "repo has no
		// trusted commands" case; log at debug so it isn't noisy. Other
		// errors are surfaced at warn so a genuinely broken read isn't
		// silent. Either way trusted is nil → fail closed.
		slog.Debug("trusted repo config: not present on default branch", "run_id", runID, "sha", trustedSHA, "error", err)
		return nil
	}
	trusted, err := config.LoadRepoFromBytes([]byte(content))
	if err != nil {
		slog.Warn("trusted repo config: parse failed; commands/agent from pushed branch will be disabled", "run_id", runID, "sha", trustedSHA, "error", err)
		return nil
	}
	return trusted
}

// assertGateTrustedConfigReadable fails a run LOUD when the trusted
// default-branch copy of .no-mistakes.yaml could not be READ at all. This is the
// security correction for disable_project_settings: that field is a boundary
// honored only from the trusted copy, so an unreadable trusted config must NOT
// be silently treated as "not opted out" - no-mistakes cannot know whether the
// repo relies on the boundary, so it refuses to run rather than risk launching a
// gate agent with the project instructions loaded.
//
// It distinguishes "could not read the trusted config at all" (abort) from
// "read the trusted tree fine, there is simply no .no-mistakes.yaml on the
// default branch" (the common ordinary-repo case, which is NOT opted out and
// must proceed). Abort cases:
//   - no known default branch to read a trusted copy from,
//   - the default branch could not be fetched/resolved to a pinned SHA,
//   - the pinned commit or tree is not readable (missing object / partial fetch),
//   - the trusted .no-mistakes.yaml is present but unreadable or unparseable.
func assertGateTrustedConfigReadable(ctx context.Context, wtDir, defaultBranch, trustedSHA string) error {
	return assertTrustedConfigReadable(ctx, wtDir, defaultBranch, trustedSHA, "disable_project_settings")
}

// assertTrustedConfigReadable owns the fail-closed read of the freshly fetched
// default-branch config. Callers name the policy they are evaluating so the
// same trust proof retains an actionable diagnostic at each consumer seam.
func assertTrustedConfigReadable(ctx context.Context, wtDir, defaultBranch, trustedSHA, policy string) error {
	if defaultBranch == "" {
		return fmt.Errorf("cannot evaluate %s: repository has no known default branch to read trusted config from", policy)
	}
	if trustedSHA == "" {
		return fmt.Errorf("cannot evaluate %s: failed to fetch or resolve trusted default branch %q (refusing to run without reading the trusted config)", policy, defaultBranch)
	}
	if _, err := git.Run(ctx, wtDir, "rev-parse", "-q", "--verify", trustedSHA+"^{commit}"); err != nil {
		return fmt.Errorf("cannot evaluate %s: trusted default-branch commit %s is not readable: %w", policy, trustedSHA, err)
	}
	entry, err := git.Run(ctx, wtDir, "ls-tree", trustedSHA, "--", ".no-mistakes.yaml")
	if err != nil {
		return fmt.Errorf("cannot evaluate %s: trusted default-branch tree at %s is not readable: %w", policy, trustedSHA, err)
	}
	if entry == "" {
		return nil
	}
	content, err := git.ShowFile(ctx, wtDir, trustedSHA, ".no-mistakes.yaml")
	if err != nil {
		return fmt.Errorf("cannot evaluate %s: trusted .no-mistakes.yaml at %s is present but not readable: %w", policy, trustedSHA, err)
	}
	if _, err := config.LoadRepoFromBytes([]byte(content)); err != nil {
		return fmt.Errorf("cannot evaluate %s: trusted .no-mistakes.yaml at %s is present but unparseable: %w", policy, trustedSHA, err)
	}
	return nil
}

type ownerAdmissionIssueKey struct{}

func (m *RunManager) handleRetainedPushLocked(ctx context.Context, params *ipc.PushReceivedParams) (string, error) {
	owner, issue := params.Owner, params.OwnerValidationError
	if err := types.ValidateRunOwner(owner); err != nil {
		issue = err.Error()
	}
	if issue != "" {
		owner = ""
	}
	ctx = context.WithValue(ctx, ownerAdmissionIssueKey{}, issue)
	ctx = types.WithShippingOptions(ctx, params.Shipping)
	ctx = types.WithRunOwner(ctx, owner)
	// Ref deletion (git push remote :branch) sends new SHA as all-zeros.
	// Nothing to validate - skip pipeline.
	if git.IsZeroSHA(params.New) {
		return "", fmt.Errorf("ref deletion push, no pipeline to run")
	}

	repoID, err := ownedGateRepoID(m.paths, params.Gate)
	if err != nil {
		return "", err
	}
	repo, err := m.db.GetRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return "", fmt.Errorf("unknown repo for gate %s", params.Gate)
	}

	branch := branchFromRef(params.Ref)
	baseSHA := params.Old
	// A push that re-creates a branch the pusher reconciled reports no previous
	// head, which would record a zero base and make a deliberate history
	// rewrite look like an ordinary push to the rebase step. Restore the head
	// the branch actually carried, but only when the gate's own archive tag
	// records it: the claim itself arrives over the push and is not evidence.
	if git.IsZeroSHA(baseSHA) && gate.ArchivedHeadRecorded(ctx, m.paths.RepoDir(repo.ID), branch, params.ReconciledPreviousHead) {
		baseSHA = strings.TrimSpace(params.ReconciledPreviousHead)
	}
	if params.LaunchNonce != "" {
		receipt, err := m.startFreshLaunchLocked(ctx, repo, branch, params.New, baseSHA, params.Gate, params.SkipSteps, params.Intent, params.LaunchNonce, params.ValidationGeneration, params.PRBaseBranch, params.OmitIntent, params.ExternalCIOwner, "push", params.VerificationPlanID, params.ClosingIssueRefs, params.PiProfile)
		if err != nil {
			return "", err
		}
		return receipt.RunID, nil
	}
	return m.startRunWithIntentSourceLocked(ctx, repo, branch, params.New, baseSHA, "push", params.SkipSteps, params.Intent, db.RunIntentSourceAgent, "", "", "", params.PRBaseBranch, strings.TrimSpace(params.PRBaseBranch) != "", params.OmitIntent, params.ExternalCIOwner, "", params.VerificationPlanID, params.ClosingIssueRefs, params.PiProfile)
}

// HandleStartFreshRun creates or replays a proof-mode launch only after
// checking the creation context under the repository/branch lock.
func (m *RunManager) HandleStartFreshRun(ctx context.Context, params *ipc.StartFreshRunParams) (ipc.LaunchReceipt, error) {
	if err := types.ValidateRunOwner(params.Owner); err != nil {
		return ipc.LaunchReceipt{}, err
	}
	ctx = types.WithShippingOptions(ctx, params.Shipping)
	ctx = types.WithRunOwner(ctx, params.Owner)
	repo, err := m.db.GetRepo(params.RepoID)
	if err != nil {
		return ipc.LaunchReceipt{}, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return ipc.LaunchReceipt{}, fmt.Errorf("unknown repo %s", params.RepoID)
	}
	return m.startFreshLaunch(ctx, repo, params.Branch, params.HeadSHA, "", m.paths.RepoDir(repo.ID), params.SkipSteps, params.Intent, params.LaunchNonce, params.ValidationGeneration, params.PRBaseBranch, params.OmitIntent, params.ExternalCIOwner, "fresh", params.VerificationPlanID, params.ClosingIssueRefs, params.PiProfile)
}

// startFreshLaunch owns proof identity under the branch lock. A nonce may
// replay only its immutable submitted-head, generation, and persisted-intent
// digest. It must never fall back to ordinary same-head reattachment.
func (m *RunManager) startFreshLaunch(ctx context.Context, repo *db.Repo, branch, headSHA, baseSHA, gateDir string, skipSteps []types.StepName, intent, launchNonce, validationGeneration, prBaseBranch string, omitIntent bool, externalCIOwner, trigger, planID string, closingIssues []string, profiles ...*agentcfg.PiProfile) (ipc.LaunchReceipt, error) {
	var receipt ipc.LaunchReceipt
	_, err := m.withBranchLock(repo.ID, branch, func() (string, error) {
		var err error
		receipt, err = m.startFreshLaunchLocked(ctx, repo, branch, headSHA, baseSHA, gateDir, skipSteps, intent, launchNonce, validationGeneration, prBaseBranch, omitIntent, externalCIOwner, trigger, planID, closingIssues, profiles...)
		return receipt.RunID, err
	})
	return receipt, err
}

func (m *RunManager) startFreshLaunchLocked(ctx context.Context, repo *db.Repo, branch, headSHA, baseSHA, gateDir string, skipSteps []types.StepName, intent, launchNonce, validationGeneration, prBaseBranch string, omitIntent bool, externalCIOwner, trigger, planID string, closingIssues []string, profiles ...*agentcfg.PiProfile) (ipc.LaunchReceipt, error) {
	if err := types.ValidateExternalCIOwnerRequest(externalCIOwner, skipSteps); err != nil {
		return ipc.LaunchReceipt{}, err
	}
	if types.ShippingFromContext(ctx) != nil && externalCIOwner != "" {
		return ipc.LaunchReceipt{}, fmt.Errorf("strict shipping cannot be combined with external CI ownership")
	}
	request := agentcfg.OptionalPiProfile(profiles)
	if err := request.ValidateRequest(); err != nil {
		return ipc.LaunchReceipt{}, err
	}
	if err := validateLaunchNonce(launchNonce); err != nil {
		return ipc.LaunchReceipt{}, err
	}
	if err := validateValidationGeneration(validationGeneration); err != nil {
		return ipc.LaunchReceipt{}, err
	}
	storedPRBaseBranch, err := normalizeRunPRBaseBranch(prBaseBranch)
	if err != nil {
		return ipc.LaunchReceipt{}, err
	}

	if strings.TrimSpace(intent) == "" {
		return ipc.LaunchReceipt{}, fmt.Errorf("intent is required with launch_nonce")
	}
	// Proof launches bind exactly the bytes persisted. Ordinary AXI intent
	// retains its historical normalization.
	persistedIntent := intent
	requestDigest := digestIntent(persistedIntent)
	var receipt ipc.LaunchReceipt
	_, err = func() (string, error) {
		existing, err := m.db.GetRunByLaunchNonce(repo.ID, branch, launchNonce)
		if err != nil {
			return "", err
		}
		if existing != nil {
			if existing.Owner() != types.RunOwnerFromContext(ctx) {
				return "", fmt.Errorf("conflicting launch_nonce: owner differs from run")
			}
			if planID != "" && (existing.VerificationPlan == nil || existing.VerificationPlan.ID != planID) {
				return "", fmt.Errorf("verification plan cannot replace an existing run attachment")
			}
			if !existing.Shipping.Matches(types.ShippingFromContext(ctx)) {
				return "", fmt.Errorf("conflicting launch_nonce: shipping pin differs")
			}
			if !existing.PiProfile.Matches(request) {
				return "", fmt.Errorf("conflicting launch_nonce: Pi profile differs from run pin")
			}
			if !launchPRBaseBranchMatches(existing, storedPRBaseBranch) {
				return "", conflictingLaunchPRBaseBranch(launchNonce)
			}
			// The stored value folds the operator's global intent.publish_intent
			// default in, so only a claim REQUESTING omission against a run
			// without it is a genuine conflict; the reverse can be the fold.
			if omitIntent && !existing.OmitIntent {
				return "", conflictingLaunchOmitIntent(launchNonce)
			}
			if existing.ExternalCIOwner != externalCIOwner {
				return "", fmt.Errorf("conflicting launch_nonce: external CI owner differs from run")
			}

			replayed, err := m.receiptForRun(existing, false)
			if err != nil {
				return "", err
			}
			// Submitted head is immutable proof provenance. Pipeline fixes and
			// later gate pushes do not invalidate a same-nonce replay.
			if replayed.ValidationGeneration != validationGeneration || replayed.SubmittedHeadSHA != headSHA || replayed.IntentDigest != requestDigest {
				return "", fmt.Errorf("conflicting launch_nonce %q is already bound to a different validation generation, submitted head, or intent", launchNonce)
			}
			// Duplicate hook delivery is not observation: preserve an
			// unclaimed push row for the first matching receipt observer.
			if trigger == "push" {
				if retained, _ := ctx.Value(pushNotificationKey{}).(*retainedPush); retained != nil {
					if err := m.validateAndMarkNotification(ctx, retained); err != nil {
						return "", err
					}
					if err := m.db.BindPushNotification(retained.ID, existing.ID); err != nil {
						return "", err
					}
				}
				receipt = replayed
				return existing.ID, nil
			}
			m.mu.Lock()
			claimedRun, claimed, err := m.db.ClaimLaunchReceiptWithAllOptions(repo.ID, branch, launchNonce, headSHA, validationGeneration, requestDigest, storedPRBaseBranch, omitIntent, types.ShippingFromContext(ctx), externalCIOwner, types.RunOwnerFromContext(ctx), request, m.receiptEligibilityLocked)
			m.mu.Unlock()
			if err != nil {
				return "", err
			}
			if claimedRun == nil {
				return "", fmt.Errorf("claimed launch receipt %q disappeared", launchNonce)
			}
			if !launchPRBaseBranchMatches(claimedRun, storedPRBaseBranch) {
				return "", conflictingLaunchPRBaseBranch(launchNonce)
			}

			receipt = launchReceiptForEligibleRun(claimedRun, claimed)
			return existing.ID, nil
		}

		gateHead, err := git.Run(ctx, gateDir, "rev-parse", "refs/heads/"+branch+"^{commit}")
		if err != nil {
			if pin := types.ShippingFromContext(ctx); pin == nil || pin.Mode != types.ShippingImmutableSource {
				return "", fmt.Errorf("resolve gate head: %w", err)
			}
			gateHead = ""
		}
		if gateHead != headSHA {
			if pin := types.ShippingFromContext(ctx); pin == nil || pin.Mode != types.ShippingImmutableSource {
				return "", fmt.Errorf("launch context drift: gate branch %q is at %s, not requested %s", branch, gateHead, headSHA)
			}
			sourceURL := git.ResolvePushURL(ctx, gateDir, repo.UpstreamURL, repo.ForkURL, repo.URLsVerified)
			remote, err := shipping.RemoteHead(ctx, gateDir, sourceURL, branch)
			if err != nil {
				return "", fmt.Errorf("observe immutable published source: %w", err)
			}
			if remote != headSHA {
				return "", fmt.Errorf("immutable published source differs from submitted head")
			}
			if _, err := git.Run(ctx, gateDir, "fetch", "--no-tags", "--no-write-fetch-head", sourceURL, headSHA); err != nil {
				return "", fmt.Errorf("fetch immutable source objects: %w", err)
			}
		}
		inheritedPRURL := ""
		if baseSHA == "" {
			runs, err := m.db.GetRunsByRepoHead(repo.ID, branch, headSHA)
			if err != nil {
				return "", err
			}
			baseSHA = headSHA
			if len(runs) > 0 {
				baseSHA = runs[0].BaseSHA
				inheritedPRURL = inheritablePRURL(runs[0])
			}
		}
		runID, err := m.startRunWithIntentSourceLocked(ctx, repo, branch, headSHA, baseSHA, trigger, skipSteps, persistedIntent, db.RunIntentSourceAgent, launchNonce, validationGeneration, requestDigest, storedPRBaseBranch, storedPRBaseBranch != "", omitIntent, externalCIOwner, inheritedPRURL, planID, closingIssues, request)
		if err != nil {
			return "", err
		}
		run, err := m.db.GetRun(runID)
		if err != nil {
			return "", fmt.Errorf("read created run: %w", err)
		}
		if trigger == "push" {
			receipt, err = m.receiptForRun(run, true)
			if err != nil {
				return "", err
			}
		} else {
			m.mu.Lock()
			claimedRun, claimed, err := m.db.ClaimLaunchReceiptWithAllOptions(repo.ID, branch, launchNonce, headSHA, validationGeneration, requestDigest, storedPRBaseBranch, omitIntent, types.ShippingFromContext(ctx), externalCIOwner, types.RunOwnerFromContext(ctx), request, m.receiptEligibilityLocked)
			m.mu.Unlock()
			if err != nil {
				return "", err
			}
			if claimedRun == nil {
				return "", fmt.Errorf("claim newly created launch receipt")
			}
			if !launchPRBaseBranchMatches(claimedRun, storedPRBaseBranch) {
				return "", conflictingLaunchPRBaseBranch(launchNonce)
			}

			receipt = launchReceiptForEligibleRun(claimedRun, claimed)
		}
		return runID, nil
	}()
	if err != nil {
		return ipc.LaunchReceipt{}, err
	}
	return receipt, nil
}

func normalizeRunPRBaseBranch(prBaseBranch string) (string, error) {
	normalized, err := steps.ValidateRunPRBaseBranchName(prBaseBranch)
	if err != nil {
		return "", fmt.Errorf("pr base branch: %w", err)
	}
	return normalized, nil
}

func launchPRBaseBranchMatches(run *db.Run, requested string) bool {
	if requested == "" {
		return true
	}
	return run != nil && run.PRBaseBranch != nil && strings.TrimSpace(*run.PRBaseBranch) == requested
}

func conflictingLaunchPRBaseBranch(launchNonce string) error {
	return fmt.Errorf("conflicting launch_nonce %q is already bound to a different pr base branch", launchNonce)
}

func conflictingLaunchOmitIntent(launchNonce string) error {
	return fmt.Errorf("conflicting launch_nonce %q is already bound to a run that publishes the Intent section", launchNonce)
}

func validateLaunchNonce(nonce string) error {
	return validateLaunchValue("launch_nonce", nonce)
}

func validateValidationGeneration(generation string) error {
	return validateLaunchValue("validation_generation", generation)
}

func validateLaunchValue(field, value string) error {
	if len(value) == 0 || len(value) > 128 {
		return fmt.Errorf("%s must be 1 to 128 ASCII URL-safe characters", field)
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._~-", char) {
			continue
		}
		return fmt.Errorf("%s contains unsupported character %q", field, char)
	}
	return nil
}

func digestIntent(intent string) string {
	sum := sha256.Sum256([]byte(intent))
	return fmt.Sprintf("%x", sum)
}

func (m *RunManager) claimLaunchReceiptLocked(p *ipc.ClaimLaunchReceiptParams, prBaseBranch string) (*ipc.ClaimLaunchReceiptResult, error) {
	existing, err := m.db.GetRunByLaunchNonce(p.RepoID, p.Branch, p.LaunchNonce)
	if err != nil {
		return nil, err
	}
	if m.afterReceiptLookup != nil {
		m.afterReceiptLookup(existing)
	}
	if existing != nil {
		if err := m.runAdmissionError(existing); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	run, claimed, err := m.db.ClaimLaunchReceiptWithAllOptions(p.RepoID, p.Branch, p.LaunchNonce, p.SubmittedHeadSHA, p.ValidationGeneration, p.IntentDigest, prBaseBranch, p.OmitIntent, p.Shipping, p.ExternalCIOwner, p.Owner, p.PiProfile, m.receiptEligibilityLocked)
	m.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("claim launch receipt: %w", err)
	}
	if run == nil {
		return &ipc.ClaimLaunchReceiptResult{}, nil
	}
	if !run.Shipping.Matches(p.Shipping) {
		return nil, fmt.Errorf("conflicting launch_nonce: shipping pin differs")
	}
	if !run.PiProfile.Matches(p.PiProfile) {
		return nil, fmt.Errorf("conflicting launch_nonce: Pi profile differs from run pin")
	}
	if !launchPRBaseBranchMatches(run, prBaseBranch) {
		return nil, conflictingLaunchPRBaseBranch(p.LaunchNonce)
	}
	if p.OmitIntent && !run.OmitIntent {
		return nil, conflictingLaunchOmitIntent(p.LaunchNonce)
	}
	if run.ExternalCIOwner != p.ExternalCIOwner {
		return nil, fmt.Errorf("conflicting launch_nonce: external CI owner differs from run")
	}

	receipt := launchReceiptForEligibleRun(run, claimed)
	if receipt.SubmittedHeadSHA != p.SubmittedHeadSHA || receipt.ValidationGeneration != p.ValidationGeneration || receipt.IntentDigest != p.IntentDigest {
		return nil, fmt.Errorf("conflicting launch_nonce is already bound to a different validation generation, submitted head, or intent")
	}
	return &ipc.ClaimLaunchReceiptResult{Receipt: &receipt}, nil
}

func (m *RunManager) receiptEligibilityLocked(run *db.Run) error {
	if err := m.runAdmissionErrorLocked(run); err != nil {
		return err
	}
	if run.SubmittedHeadSHA == nil || *run.SubmittedHeadSHA == "" {
		return fmt.Errorf("run %s has no submitted head binding", run.ID)
	}
	if run.LaunchNonce == nil || run.LaunchValidationGeneration == nil || run.LaunchIntentDigest == nil {
		return fmt.Errorf("run %s has no launch binding", run.ID)
	}
	return nil
}

func (m *RunManager) receiptForRun(run *db.Run, created bool) (ipc.LaunchReceipt, error) {
	m.mu.Lock()
	err := m.receiptEligibilityLocked(run)
	m.mu.Unlock()
	if err != nil {
		return ipc.LaunchReceipt{}, err
	}
	return launchReceiptForEligibleRun(run, created), nil
}

func launchReceiptForEligibleRun(run *db.Run, created bool) ipc.LaunchReceipt {
	disposition := "reused"
	if created {
		disposition = "created"
	}
	return ipc.LaunchReceipt{
		Owner:                run.Owner(),
		PiProfile:            run.PiProfile,
		RunID:                run.ID,
		Disposition:          disposition,
		LaunchNonce:          *run.LaunchNonce,
		ValidationGeneration: *run.LaunchValidationGeneration,
		Branch:               run.Branch,
		HeadSHA:              *run.SubmittedHeadSHA,
		SubmittedHeadSHA:     *run.SubmittedHeadSHA,
		IntentDigest:         *run.LaunchIntentDigest,
	}
}

// HandleRerun creates a new run for the latest recoverable head on a branch:
// normally the gate branch, or the latest terminal run's verified unpublished
// head while custody remains outstanding. An unverified head counts when its
// own recovery ref anchors it above the gate head, exactly as Recover accepts. An explicit intent overrides the
// selected run. Otherwise an authoritative intent is inherited byte-for-byte;
// runs without one infer intent afresh. The selected run's PR URL is inherited
// when that PR is not already merged or closed, so a later --base-branch
// retarget can prove it is moving the same still-open review object.
// A supplied clean caller head must match the selected head before any run
// starts or is superseded. It never changes head selection.
func (m *RunManager) HandleRerun(ctx context.Context, repoID, branch, previousRunID string, skipSteps []types.StepName, intent, prBaseBranch string, omitIntent bool, externalCIOwner, callerHeadSHA, planID string, closingIssues []string, profiles ...*agentcfg.PiProfile) (string, error) {
	repo, err := m.db.GetRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return "", fmt.Errorf("unknown repo %s", repoID)
	}

	gateDir := m.paths.RepoDir(repo.ID)
	runs, err := m.db.GetRunsByRepo(repoID)
	if err != nil {
		return "", fmt.Errorf("get runs: %w", err)
	}

	var latestForBranch *db.Run
	for _, run := range runs {
		if run.Branch != branch {
			continue
		}
		if latestForBranch == nil {
			latestForBranch = run
		}
	}
	if latestForBranch == nil {
		return "", fmt.Errorf("no previous run for branch %s", branch)
	}
	selectedRun := latestForBranch
	if previousRunID != "" {
		selectedRun, err = m.db.GetRun(previousRunID)
		if err != nil {
			return "", fmt.Errorf("get selected run: %w", err)
		}
		if selectedRun == nil || selectedRun.RepoID != repoID || selectedRun.Branch != branch {
			return "", fmt.Errorf("selected run %s does not belong to repo %s branch %s", previousRunID, repoID, branch)
		}
	}

	var headSHA, gateHead string
	var matchingHead *db.Run
	if selectedRun.Shipping != nil && selectedRun.Shipping.Mode == types.ShippingImmutableSource {
		headSHA, err = resolveImmutableSourceRerunHead(ctx, gateDir, repo, selectedRun)
		if err != nil {
			return "", err
		}
	} else {
		gateHead, err = git.Run(ctx, gateDir, "rev-parse", "refs/heads/"+branch+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("resolve gate head: %w", err)
		}
		for _, run := range runs {
			if run.Branch == branch && run.HeadSHA == gateHead {
				matchingHead = run
				break
			}
		}
		headSHA, err = resolveRerunHead(ctx, gateDir, branch, latestForBranch)
		if err != nil {
			return "", err
		}
	}
	if callerHeadSHA != "" && callerHeadSHA != headSHA {
		return "", fmt.Errorf("refusing rerun: selected head %s differs from clean local head %s; inspect `no-mistakes axi status` and reconcile custody before using `no-mistakes axi run` to submit the local head", headSHA, callerHeadSHA)
	}

	baseSHA := latestForBranch.BaseSHA
	if matchingHead != nil && headSHA == gateHead {
		baseSHA = matchingHead.BaseSHA
	}
	if selectedRun.Shipping != nil && selectedRun.Shipping.Mode == types.ShippingImmutableSource {
		baseSHA = selectedRun.BaseSHA
	}

	intentSource := db.RunIntentSourceAgent
	if strings.TrimSpace(intent) == "" {
		intentSource = ""
		if selectedRun.Intent != nil && selectedRun.IntentSource != nil &&
			db.IsAuthoritativeRunIntentSource(*selectedRun.IntentSource) {
			// Do not normalize or regenerate this value. The selected run's
			// persisted bytes are the canonical acceptance criteria for the
			// replacement run.
			intent = *selectedRun.Intent
			intentSource = db.RunIntentSourceRerun
		}
	}

	storedPRBaseBranch := strings.TrimSpace(prBaseBranch)
	if selectedRun.Shipping != nil {
		pinnedTarget := strings.TrimSpace(selectedRun.Shipping.TargetBranch)
		if pinnedTarget == "" {
			return "", fmt.Errorf("strict shipping run %s has no pinned PR target", selectedRun.ID)
		}
		if storedPRBaseBranch != "" && storedPRBaseBranch != pinnedTarget {
			return "", fmt.Errorf("refusing rerun: requested PR target %q conflicts with pinned strict shipping target %q", storedPRBaseBranch, pinnedTarget)
		}
		storedPRBaseBranch = pinnedTarget
	} else if storedPRBaseBranch == "" && selectedRun.PRBaseBranch != nil {
		storedPRBaseBranch = strings.TrimSpace(*selectedRun.PRBaseBranch)
	}
	// Publication omission is tighten-only on rerun as everywhere else: the
	// selected run's decision is inherited and this rerun can only add to it.
	// The locked start then folds in the operator's live global default, which
	// likewise can only add omission, never remove it.
	ctx = types.WithShippingOptions(ctx, selectedRun.Shipping)
	// Closing references are structured run metadata and survive reruns: an
	// explicit request is added to, never replaces, what the selected run carried.
	closingIssues = append(append([]string(nil), selectedRun.ClosingIssueRefs...), closingIssues...)
	return m.startRunWithIntentSource(ctx, repo, branch, headSHA, baseSHA, "rerun", skipSteps, intent, intentSource, storedPRBaseBranch, strings.TrimSpace(prBaseBranch) != "", selectedRun.OmitIntent || omitIntent, externalCIOwner, inheritablePRURL(selectedRun), planID, closingIssues, profiles...)
}

func resolveImmutableSourceRerunHead(ctx context.Context, gateDir string, repo *db.Repo, selected *db.Run) (string, error) {
	if selected == nil || selected.Shipping == nil || selected.Shipping.Mode != types.ShippingImmutableSource {
		return "", fmt.Errorf("selected run is not an immutable source run")
	}
	if selected.SubmittedHeadSHA == nil || strings.TrimSpace(*selected.SubmittedHeadSHA) == "" || *selected.SubmittedHeadSHA != selected.HeadSHA {
		return "", fmt.Errorf("refusing immutable rerun: selected run has no exact submitted source head")
	}
	branch := strings.TrimPrefix(selected.Branch, "refs/heads/")
	if branch == "" {
		return "", fmt.Errorf("refusing immutable rerun: selected source branch is empty")
	}
	if repo == nil {
		return "", fmt.Errorf("refusing immutable rerun: repository is missing")
	}
	sourceURL := git.ResolvePushURL(ctx, gateDir, repo.UpstreamURL, repo.ForkURL, repo.URLsVerified)
	remote, err := shipping.RemoteHead(ctx, gateDir, sourceURL, branch)
	if err != nil {
		return "", fmt.Errorf("observe immutable rerun source: %w", err)
	}
	if remote != *selected.SubmittedHeadSHA {
		return "", fmt.Errorf("refusing immutable rerun: published source branch %q is at %s, not selected submitted head %s", branch, remote, *selected.SubmittedHeadSHA)
	}
	if err := shipping.CheckImmutableTarget(ctx, selected, repo, gateDir, shipping.RemoteHead); err != nil {
		return "", fmt.Errorf("observe immutable rerun target: %w", err)
	}
	return *selected.SubmittedHeadSHA, nil
}

func inheritablePRURL(run *db.Run) string {
	if run.PRURL == nil {
		return ""
	}
	state := ""
	if run.PRState != nil {
		state = strings.ToLower(strings.TrimSpace(*run.PRState))
	}
	if state == "merged" || state == "closed" {
		return ""
	}
	return strings.TrimSpace(*run.PRURL)
}

func resolveRerunHead(ctx context.Context, gateDir, branch string, latest *db.Run) (string, error) {
	gateHead, exists, err := git.DirectRefTarget(ctx, gateDir, "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("resolve direct gate head: %w", err)
	}
	if !exists {
		return "", fmt.Errorf("resolve gate head: branch %s is missing", branch)
	}
	if resolved, err := git.ResolveRef(ctx, gateDir, gateHead); err != nil || resolved != gateHead {
		return "", fmt.Errorf("resolve gate head: branch %s does not directly name a commit", branch)
	}
	if latest == nil || !latest.Status.Terminal() || latest.CustodyReturnedAt != nil || latest.HeadSHA == gateHead {
		return gateHead, nil
	}
	published := ""
	if latest.LastPushedSHA != nil {
		published = *latest.LastPushedSHA
	} else if latest.SubmittedHeadSHA != nil {
		published = *latest.SubmittedHeadSHA
	}
	if published == latest.HeadSHA || (latest.TerminalHeadVerifiedAt == nil && !custody.AnchoredDescendant(ctx, gateDir, latest.ID, gateHead, latest.HeadSHA)) {
		return gateHead, nil
	}
	recoveryRef := custody.RecoveryRef(latest.ID)
	refTarget, refExists, refErr := git.ExactRefTarget(ctx, gateDir, recoveryRef)
	if refErr != nil {
		return "", fmt.Errorf("inspect terminal recovery ref for run %s: %w", latest.ID, refErr)
	}
	if refExists {
		preserved, preserveErr := git.Run(ctx, gateDir, "rev-parse", recoveryRef+"^{commit}")
		if preserveErr != nil {
			return "", fmt.Errorf("refusing rerun: terminal recovery ref for run %s points at non-commit object %s; inspect with `no-mistakes axi status` and reconcile custody first", latest.ID, refTarget)
		}
		if preserved != latest.HeadSHA {
			return "", fmt.Errorf("refusing rerun: terminal recovery ref for run %s points at %s, not recorded unpublished head %s; inspect with `no-mistakes axi status` and reconcile custody first", latest.ID, preserved, latest.HeadSHA)
		}
		return preserved, nil
	}
	if preserved, objectErr := git.Run(ctx, gateDir, "rev-parse", latest.HeadSHA+"^{commit}"); objectErr == nil && preserved == latest.HeadSHA {
		if anchorErr := custody.PreserveRecoveryHead(ctx, gateDir, latest.ID, preserved); anchorErr != nil {
			return "", fmt.Errorf("preserve terminal head %s before rerun: %w", preserved, anchorErr)
		}
		return preserved, nil
	}
	return "", fmt.Errorf("refusing rerun from stale gate head %s: terminal run %s recorded unpublished head %s, but that head is unavailable; inspect with `no-mistakes axi status` and reconcile custody first", gateHead, latest.ID, latest.HeadSHA)
}

// fetchTrustedDefaultBranchSHA imports the live default branch into a
// caller-owned private ref and pins its SHA before deleting the ref. New runs,
// recovery, and Pi-profile validation share this boundary so concurrent reads
// never contend on origin tracking refs or FETCH_HEAD. Refreshed registration
// URLs apply when verified; otherwise origin retains its authentication.
func fetchTrustedDefaultBranchSHA(ctx context.Context, gateDir string, repo *db.Repo) (string, error) {
	if strings.TrimSpace(repo.DefaultBranch) == "" {
		return "", fmt.Errorf("cannot read trusted config: repository has no known default branch to read trusted config from")
	}
	privateRef := fmt.Sprintf("refs/no-mistakes/trusted-config/%d-%d", os.Getpid(), time.Now().UnixNano())
	defer func() {
		_, _ = git.Run(context.WithoutCancel(ctx), gateDir, "update-ref", "--no-deref", "-d", privateRef)
	}()
	originURL, err := git.GetRemoteURL(ctx, gateDir, "origin")
	var fetchErr error
	if !repo.URLsVerified || (err == nil && safeurl.Redact(originURL) == repo.UpstreamURL) {
		fetchErr = git.FetchRemoteBranchToPrivateRef(ctx, gateDir, "origin", repo.DefaultBranch, privateRef)
	} else {
		fetchErr = git.FetchRemoteBranchToPrivateRef(ctx, gateDir, repo.UpstreamURL, repo.DefaultBranch, privateRef)
	}
	if fetchErr != nil {
		return "", fmt.Errorf("cannot read trusted config: failed to fetch trusted default branch %q: %w", repo.DefaultBranch, fetchErr)
	}
	sha, err := git.ResolveRef(ctx, gateDir, privateRef)
	if err != nil {
		return "", fmt.Errorf("cannot read trusted config: failed to resolve trusted default branch %q: %w", repo.DefaultBranch, err)
	}
	return sha, nil
}

// validatePiProfileAgentsBeforeCancel loads the effective trusted repo agent
// selection (and, when allow_repo_commands is set, the pushed copy) from the
// gate and runs the same check ValidatePiProfileAgents will run after merge.
// A trusted default-branch Claude or mixed fallback list must fail here, not
// after cancelActiveRuns has already stopped a healthy validation. This is also
// the first point where a trusted repository Pi override can replace a global
// auto or non-Pi selection, so profile resolution must not reject it earlier.
func (m *RunManager) validatePiProfileAgentsBeforeCancel(ctx context.Context, repo *db.Repo, headSHA string, globalCfg *config.GlobalConfig) error {
	gateDir := m.paths.RepoDir(repo.ID)
	trustedSHA, err := fetchTrustedDefaultBranchSHA(ctx, gateDir, repo)
	if err != nil {
		return err
	}
	if err := assertTrustedConfigReadable(ctx, gateDir, repo.DefaultBranch, trustedSHA, "Pi run profile"); err != nil {
		return err
	}
	trustedRepoCfg := loadTrustedRepoConfig(ctx, gateDir, trustedSHA, "")
	pushedRepoCfg, err := loadRepoConfigAtSHA(ctx, gateDir, headSHA)
	if err != nil {
		return err
	}
	allowRepoCommands := trustedRepoCfg != nil && trustedRepoCfg.AllowRepoCommands
	effective := config.EffectiveRepoConfig(pushedRepoCfg, trustedRepoCfg, allowRepoCommands)
	return config.MergeForRemote(globalCfg, effective, repo.UpstreamURL).ValidatePiProfileAgents()
}

func loadRepoConfigAtSHA(ctx context.Context, dir, sha string) (*config.RepoConfig, error) {
	if sha == "" {
		return nil, fmt.Errorf("cannot evaluate Pi run profile: submitted commit is missing")
	}
	if _, err := git.Run(ctx, dir, "rev-parse", "-q", "--verify", sha+"^{commit}"); err != nil {
		return nil, fmt.Errorf("cannot evaluate Pi run profile: submitted commit %s is not readable: %w", sha, err)
	}
	entry, err := git.Run(ctx, dir, "ls-tree", sha, "--", ".no-mistakes.yaml")
	if err != nil {
		return nil, fmt.Errorf("cannot evaluate Pi run profile: submitted tree at %s is not readable: %w", sha, err)
	}
	if entry == "" {
		return &config.RepoConfig{}, nil
	}
	content, err := git.ShowFile(ctx, dir, sha, ".no-mistakes.yaml")
	if err != nil {
		return nil, fmt.Errorf("cannot evaluate Pi run profile: submitted .no-mistakes.yaml at %s is present but not readable: %w", sha, err)
	}
	cfg, err := config.LoadRepoFromBytes([]byte(content))
	if err != nil {
		return nil, fmt.Errorf("cannot evaluate Pi run profile: submitted .no-mistakes.yaml at %s is present but unparseable: %w", sha, err)
	}
	return cfg, nil
}

// startRun records a run for admission; worktree setup and pipeline execution
// begin only after a slot is granted. A queued launch returns its run ID immediately.
// A non-empty intent is stamped onto the run as agent-supplied, so the intent
// step uses it instead of inferring from transcripts.
func (m *RunManager) startRun(ctx context.Context, repo *db.Repo, branch, headSHA, baseSHA, trigger string, skipSteps []types.StepName, intent, prBaseBranch string, omitIntent bool, externalCIOwner, planID string, closingIssues []string, profiles ...*agentcfg.PiProfile) (string, error) {
	return m.startRunWithIntentSource(ctx, repo, branch, headSHA, baseSHA, trigger, skipSteps, intent, db.RunIntentSourceAgent, prBaseBranch, strings.TrimSpace(prBaseBranch) != "", omitIntent, externalCIOwner, "", planID, closingIssues, profiles...)
}

// startRunWithIntentSource is the common run-creation path. source is empty
// when no intent is supplied, RunIntentSourceAgent for a new explicit
// override, and RunIntentSourceRerun for inherited explicit intent.
func (m *RunManager) startRunWithIntentSource(ctx context.Context, repo *db.Repo, branch, headSHA, baseSHA, trigger string, skipSteps []types.StepName, intent, source, prBaseBranch string, prBaseBranchRequested, omitIntent bool, externalCIOwner, inheritedPRURL, planID string, closingIssues []string, profiles ...*agentcfg.PiProfile) (string, error) {
	return m.withBranchLock(repo.ID, branch, func() (string, error) {
		return m.startRunWithIntentSourceLocked(ctx, repo, branch, headSHA, baseSHA, trigger, skipSteps, intent, source, "", "", "", prBaseBranch, prBaseBranchRequested, omitIntent, externalCIOwner, inheritedPRURL, planID, closingIssues, profiles...)
	})
}

func (m *RunManager) withBranchLock(repoID, branch string, action func() (string, error)) (string, error) {
	lockKey := repoID + "/" + branch
	lockVal, _ := m.branchLocks.LoadOrStore(lockKey, &sync.Mutex{})
	branchMu := lockVal.(*sync.Mutex)
	branchMu.Lock()
	defer branchMu.Unlock()
	return action()
}

// startRunWithIntentSourceLocked performs run creation while the caller owns
// the repository/branch lock. Proof fields are empty for ordinary launches.
func (m *RunManager) startRunWithIntentSourceLocked(ctx context.Context, repo *db.Repo, branch, headSHA, baseSHA, trigger string, skipSteps []types.StepName, intent, source, launchNonce, validationGeneration, intentDigest, prBaseBranch string, prBaseBranchRequested, omitIntent bool, externalCIOwner, inheritedPRURL, planID string, closingIssues []string, profiles ...*agentcfg.PiProfile) (string, error) {
	if err := types.ValidateExternalCIOwnerRequest(externalCIOwner, skipSteps); err != nil {
		return "", err
	}
	if types.ShippingFromContext(ctx) != nil && externalCIOwner != "" {
		return "", fmt.Errorf("strict shipping cannot be combined with external CI ownership")
	}
	branchRole := telemetryBranchRole(branch, repo.DefaultBranch)
	trackStartFailure := func(stage string) {
		telemetry.Track("run", telemetry.Fields{
			"action":      "start_failed",
			"trigger":     trigger,
			"branch_role": branchRole,
			"stage":       stage,
		})
	}

	if m.shutdownState.Load() != shutdownIdle {
		trackStartFailure("daemon_shutdown")
		return "", fmt.Errorf("daemon is shutting down")
	}
	retained, _ := ctx.Value(pushNotificationKey{}).(*retainedPush)
	if retained != nil {
		if err := m.validateAndMarkNotification(ctx, retained); err != nil {
			return "", err
		}
		previous, _, err := m.db.PushNotificationRuns(retained.ID)
		if err != nil {
			return "", err
		}
		if previous != nil {
			return previous.ID, m.runAdmissionError(previous)
		}
		if id, handled, err := m.reattachReceivedRun(retained); handled || err != nil {
			return id, err
		}
	}
	globalCfg, globalCfgErr := config.LoadGlobal(m.paths.ConfigFile())
	if globalCfgErr != nil {
		active, err := m.db.GetActiveRun(repo.ID, branch)
		if err != nil {
			return "", err
		}
		if active == nil && launchNonce == "" && agentcfg.OptionalPiProfile(profiles) == nil && types.ShippingFromContext(ctx) == nil && planID == "" {
			creation := db.RunCreation{DiagnosticError: fmt.Sprintf("load config: %s", globalCfgErr)}
			var diagnostic *db.Run
			if retained != nil {
				creation.NotificationID = retained.ID
				_, diagnostic, err = m.db.PushNotificationRuns(retained.ID)
				if err != nil {
					return "", err
				}
			}
			if diagnostic == nil {
				owner := types.RunOwnerFromContext(ctx)
				_, err = m.db.InsertRunWithAllOptions(repo.ID, branch, headSHA, baseSHA, &db.RunIntent{Summary: intent, Source: source, Owner: owner, SessionID: owner}, "", "", "", "", false, omitIntent, externalCIOwner, nil, nil, nil, creation)
				if err != nil {
					return "", fmt.Errorf("load global config: %w; record diagnostic: %v", globalCfgErr, err)
				}
			}
		}
		trackStartFailure("load_global_config")
		return "", fmt.Errorf("load global config: %w", globalCfgErr)
	}
	// Best-effort only: a clone's remotes may change after init. Refresh the
	// registered URLs before constructing any run-owned Git operation, but keep
	// the exact prior repo value and continue when discovery, validation, or the
	// atomic database replacement fails. The reason is deliberately bounded and
	// URL-free so neither credentials nor sensitive remote material reach logs.
	if refreshed, _, refreshErr := gate.RefreshRepoURLs(ctx, m.db, repo); refreshErr != nil {
		slog.Warn("repository URL refresh skipped; continuing with existing registration", "repo_id", repo.ID, "reason", gate.ReasonForRefreshFailure(refreshErr))
	} else {
		repo = refreshed
	}

	// Resolve before cancellation, row creation or any pipeline work. A bad
	// dispatch request must not supersede a healthy active validation.
	// ResolvePiProfile pins model/effort from global-only defaults. Agent
	// eligibility is checked next against the effective freshly fetched trusted
	// repository selection, which may replace the global agent or fallback list.
	//
	// The global config also decides the caller-side omit-intent default,
	// which is stamped on the row at creation so recovery and reruns read the
	// stamp back instead of a since-changed config file.
	owner := types.RunOwnerFromContext(ctx)
	if err := types.ValidateRunOwner(owner); err != nil {
		return "", err
	}
	if owner == "" {
		if globalCfg != nil && globalCfg.Intent.RequireOwner && trigger != "push" {
			return "", fmt.Errorf("launch requires an owner: pass --owner or NO_MISTAKES_OWNER")
		}
		slog.Warn("launch has no owner; nobody is recorded to drive approval gates", "repo_id", repo.ID, "branch", branch)
	}
	var pin *agentcfg.PiProfile
	if request := agentcfg.OptionalPiProfile(profiles); request != nil {
		var err error
		pin, err = globalCfg.ResolvePiProfile(request)
		if err != nil {
			trackStartFailure("invalid_pi_profile")
			return "", err
		}
		if err := m.validatePiProfileAgentsBeforeCancel(ctx, repo, headSHA, globalCfg); err != nil {
			trackStartFailure("invalid_pi_profile")
			return "", err
		}
	}

	plan, err := verificationplan.Resolve(m.paths.RunInputsDir(), planID, repo.ID, branch, headSHA)
	if err != nil {
		return "", err
	}
	if plan != nil {
		active, err := m.db.GetActiveRun(repo.ID, branch)
		if err != nil {
			return "", err
		}
		if active != nil {
			return "", fmt.Errorf("verification plan is accepted only for a new run; reattach without --verification-plan")
		}
		if existing, err := m.db.GetRun(plan.ID); err != nil {
			return "", err
		} else if existing != nil {
			return "", fmt.Errorf("verification plan capture already belongs to a run")
		}
	}

	shippingPin := types.ShippingFromContext(ctx)
	if err := shippingPin.Validate(); err != nil {
		return "", err
	}
	if shippingPin != nil {
		if len(skipSteps) > 0 {
			return "", fmt.Errorf("strict shipping forbids skipped steps")
		}
		target := strings.TrimSpace(prBaseBranch)
		if target == "" {
			target = repo.DefaultBranch
		}
		if _, err := steps.ValidateRunPRBaseBranchName(target); err != nil {
			return "", err
		}
		copyPin := *shippingPin
		shippingPin = &copyPin
		shippingPin.TargetBranch = target
		retainTargetPin := trigger == "rerun" && shippingPin.Mode == types.ShippingImmutableSource && strings.TrimSpace(shippingPin.TargetSHA) != ""
		if !retainTargetPin {
			shippingPin.TargetSHA = ""
		}
		if shippingPin.Mode == types.ShippingImmutableSource {
			repoDir := m.paths.RepoDir(repo.ID)
			sourceURL := git.ResolvePushURL(ctx, repoDir, repo.UpstreamURL, repo.ForkURL, repo.URLsVerified)
			remote, err := shipping.RemoteHead(ctx, repoDir, sourceURL, branch)
			if err != nil {
				return "", fmt.Errorf("observe immutable published source: %w", err)
			}
			if remote != headSHA {
				return "", fmt.Errorf("immutable published source differs from submitted head")
			}
			// WorktreeAdd resolves the immutable SHA from the gate's object DB.
			// Import it before cancellation so a missing object cannot supersede
			// a healthy run. This fetch writes no branch or FETCH_HEAD refs.
			if _, err := git.Run(ctx, repoDir, "cat-file", "-e", headSHA+"^{commit}"); err != nil {
				if _, err := git.Run(ctx, repoDir, "fetch", "--no-tags", "--no-write-fetch-head", sourceURL, headSHA); err != nil {
					return "", fmt.Errorf("fetch immutable source objects: %w", err)
				}
			}
			if !retainTargetPin {
				upstreamURL := git.ResolveUpstreamURL(ctx, repoDir, repo.UpstreamURL, repo.URLsVerified)
				tip, err := shipping.RemoteHead(ctx, repoDir, upstreamURL, target)
				if err != nil {
					return "", err
				}
				shippingPin.TargetSHA = tip
			}
		}
	}
	predecessors := m.activeRunIDs(repo.ID, branch)

	storedIntent := intent
	if source != db.RunIntentSourceRerun && launchNonce == "" && plan == nil {
		storedIntent = strings.TrimSpace(storedIntent)
	}
	var runIntent *db.RunIntent
	if strings.TrimSpace(storedIntent) != "" || owner != "" {
		if source == "" {
			source = db.RunIntentSourceAgent
		}
		runIntent = &db.RunIntent{Summary: storedIntent, Source: source, Owner: owner, SessionID: owner, Score: 1}
	}

	issue, _ := ctx.Value(ownerAdmissionIssueKey{}).(string)
	if owner == "" && trigger == "push" && (globalCfg.Intent.RequireOwner || issue != "") {
		if runIntent == nil {
			runIntent = &db.RunIntent{}
		}
		warning := "require_owner not enforced for post-receive (TUI/direct push): no pre-receive check (follow-up #82)"
		if issue != "" {
			warning = "invalid launch ownership: " + issue + "; admitted ownerless after receive (follow-up #82)"
		}
		runIntent.OwnerAdmission = &types.OwnerAdmissionEvent{Event: "launch_owner_policy_bypassed", CallerPath: "post-receive (TUI/direct push)", OwnerlessAdmitted: true, OwnerValidationError: issue, Warning: warning}
	}
	storedPRBaseBranch, err := normalizeRunPRBaseBranch(prBaseBranch)
	if err != nil {
		trackStartFailure("invalid_pr_base_branch")
		return "", err
	}
	// The caller-side omit decision is the OR of the explicit per-run request
	// and the operator's global tighten-only default. It is stamped here, at
	// creation, and can only reduce publication: the repository's trusted
	// pr.publish_intent is enforced independently by the PR step.
	storedOmitIntent := omitIntent || (globalCfg != nil && !globalCfg.Intent.PublishesIntentByDefault())

	creation := db.RunCreation{PRURL: strings.TrimSpace(inheritedPRURL), ClosingIssueRefs: closingIssues}
	if retained != nil {
		creation.NotificationID = retained.ID
	}
	if m.beforeRunInsert != nil {
		m.beforeRunInsert()
	}
	run, err := m.db.InsertRunWithAllOptions(repo.ID, branch, headSHA, baseSHA, runIntent, launchNonce, validationGeneration, intentDigest, storedPRBaseBranch, prBaseBranchRequested, storedOmitIntent, externalCIOwner, plan, pin, shippingPin, creation)
	if err != nil {
		trackStartFailure("create_run")
		return "", fmt.Errorf("create run: %w", err)
	}
	if err := m.cancelRunIDs(predecessors); err != nil {
		settlementErr := m.failUnadmittedRunSetup(ctx, run, fmt.Errorf("supersede predecessor: %w", err))
		trackStartFailure("supersede_predecessor")
		return "", fmt.Errorf("supersede predecessor for replacement run %s: %w; inspect no-mistakes axi status --run %s and correct the fault before no-mistakes rerun", run.ID, errors.Join(err, settlementErr), run.ID)
	}

	// Admission. The run row is durable from here on. A run that fits under
	// daemon.max_concurrent_runs starts now, exactly as before. Otherwise it is
	// recorded as queued and this call returns at once: no worktree, no agent,
	// no executor. The queue drain (slotScheduler.dispatch) finishes the launch
	// from the row alone, so a queued run also survives a daemon restart.
	slot, err := m.admitRun(run, queueSpec{Trigger: trigger, SkipSteps: skipSteps})
	if err != nil {
		// The row exists but is neither queued nor owned: end it through the
		// shared setup-failure path (conditional, and it tells subscribers), so no
		// orphan is left to block the branch's active-run lookup.
		settlementErr := m.failUnadmittedRunSetup(ctx, run, fmt.Errorf("queue run: %w", err))
		trackStartFailure("queue_run")
		return "", fmt.Errorf("queue run: %w", errors.Join(err, settlementErr))
	}
	if m.afterPushCommitted != nil && retained != nil {
		m.afterPushCommitted()
	}
	if runIntent != nil && runIntent.OwnerAdmission != nil {
		slog.Warn(runIntent.OwnerAdmission.Warning,
			"event", "launch_owner_policy_bypassed", "run_id", run.ID, "repo_id", repo.ID, "branch", branch,
			"caller_path", "post-receive (TUI/direct push)", "ownerless_admitted", true)
	}
	if slot == nil {
		telemetry.Track("run", telemetry.Fields{
			"action":      "queued",
			"trigger":     trigger,
			"branch_role": branchRole,
		})
		return run.ID, nil
	}
	return m.launchRun(ctx, repo, run, globalCfg, trigger, skipSteps, slot, nil)
}

// queueSpec is what a queued run needs to start that its row does not already
// carry. Everything else (intent, PR base, pins, closing issues, shipping
// options) is persisted on the run itself.
type queueSpec struct {
	Trigger   string           `json:"trigger"`
	SkipSteps []types.StepName `json:"skip_steps,omitempty"`
}

// runHandle is the cancel/done pair registered for a run before its launch
// setup runs. A queued run's handle is registered when the queue drain picks
// it, so an abort that arrives during setup still reaches the run.
type runHandle struct {
	cancel   context.CancelCauseFunc
	runCtx   context.Context
	done     chan struct{}
	consumed bool
}

// admitRun decides a new run's admission and, when it does not fit, queues it.
// The capacity decision and the durable plus in-memory queue insertion happen
// under one admission lock, so a later launch can never be admitted into a slot
// that freed while an earlier launch was still on its way into the queue: that
// later launch waits for the lock, finds the earlier run queued, and queues
// behind it. A nil slot means the run was queued; otherwise the caller owns the
// returned slot.
func (m *RunManager) admitRun(run *db.Run, spec queueSpec) (*runSlot, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	m.sched.admitMu.Lock()
	defer m.sched.admitMu.Unlock()
	if slot, ok := m.sched.tryAdmit(run.ID); ok {
		return slot, nil
	}
	if m.afterFullDecision != nil {
		m.afterFullDecision()
	}
	m.mu.Lock()
	if _, err := m.markQueued(run.ID, string(raw)); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	run.Status = types.RunQueued
	m.runs[run.ID] = &managedRun{run: run, state: ownershipQueued}
	m.sched.queued = append(m.sched.queued, run.ID)
	m.mu.Unlock()
	m.sched.dispatch()
	return nil, nil
}

// launchRun performs the setup and background start of a run that already
// holds slot. handle is nil for a launch that runs inline in the request and
// non-nil for one the queue drain started. The slot is released on every
// return that did not hand it to the run goroutine.
func (m *RunManager) launchRun(ctx context.Context, repo *db.Repo, run *db.Run, globalCfg *config.GlobalConfig, trigger string, skipSteps []types.StepName, slot *runSlot, handle *runHandle) (runID string, err error) {
	if handle == nil {
		runCtx, cancel := context.WithCancelCause(context.Background())
		handle = &runHandle{runCtx: runCtx, cancel: cancel, done: make(chan struct{})}
		m.mu.Lock()
		m.runs[run.ID] = &managedRun{run: run, state: ownershipClaimed, handle: handle}
		m.mu.Unlock()
		defer func() {
			if !handle.consumed {
				if !m.terminalRunSettled(run.ID) {
					return
				}
				m.mu.Lock()
				delete(m.runs, run.ID)
				m.mu.Unlock()
				handle.cancel(nil)
				close(handle.done)
			}
		}()
	}
	setupCtx := ctx
	stop := context.AfterFunc(setupCtx, func() { handle.cancel(context.Cause(setupCtx)) })
	defer stop()
	ctx = handle.runCtx
	branch, headSHA := run.Branch, run.HeadSHA
	branchRole := telemetryBranchRole(branch, repo.DefaultBranch)
	trackStartFailure := func(stage string) {
		telemetry.Track("run", telemetry.Fields{
			"action":      "start_failed",
			"trigger":     trigger,
			"branch_role": branchRole,
			"stage":       stage,
		})
	}
	storedPRBaseBranch := ""
	if run.PRBaseBranch != nil {
		storedPRBaseBranch = *run.PRBaseBranch
	}
	bgOwnsSlot := false
	started := false
	var cleanupWorktree func()
	defer func() {
		if !bgOwnsSlot {
			if r := recover(); r != nil {
				err = fmt.Errorf("internal panic: %v", r)
			}
			if err != nil {
				err = errors.Join(err, m.failRunSetup(setupCtx, run.ID, started, err))
			}
			if !m.terminalRunSettled(run.ID) {
				return
			}
			if cleanupWorktree != nil {
				cleanupWorktree()
			}
			slot.Release()
		}
	}()

	// Create worktree from the gate bare repo, where this repository's
	// worktree placement says it belongs (see internal/worktrees). This is the
	// only point at which configuration decides placement: the resolved
	// directory is recorded on the run before it exists on disk, and every
	// later consumer reads that record back, so an edit to worktree_roots from
	// here on is inert for this run.
	gateDir := m.paths.RepoDir(repo.ID)
	layout := worktrees.New(m.paths, globalCfg.WorktreeRoots)
	checkouts, err := registeredCheckouts(m.db)
	if err != nil {
		trackStartFailure("list_registered_checkouts")
		return "", fmt.Errorf("list registered checkouts: %w", err)
	}
	if err := layout.ValidateCheckout(repo.WorkingPath, checkouts...); err != nil {
		trackStartFailure("invalid_worktree_placement")
		return "", fmt.Errorf("worktree placement: %w", err)
	}
	wtDir := layout.Dir(repo.ID, repo.WorkingPath, run.ID)
	if err := m.db.SetRunWorktreeDir(run.ID, wtDir); err != nil {
		trackStartFailure("record_worktree_placement")
		return "", fmt.Errorf("record worktree placement: %w", err)
	}
	if err := git.WorktreeAdd(ctx, gateDir, wtDir, headSHA); err != nil {
		trackStartFailure("create_worktree")
		return "", fmt.Errorf("create worktree: %w", err)
	}

	started = true

	cleanupWorktree = func() {
		m.removeRunWorktree(repo.ID, run.ID, gateDir, wtDir, "run_setup_failed")
	}

	if err := git.CopyLocalUserIdentity(ctx, repo.WorkingPath, wtDir); err != nil {
		trackStartFailure("configure_worktree_identity")
		return "", fmt.Errorf("configure worktree git identity: %w", err)
	}
	if storedPRBaseBranch != "" {
		if err := steps.VerifyRemoteBranchExists(ctx, wtDir, storedPRBaseBranch); err != nil {
			trackStartFailure("pr_base_branch_missing")
			return "", err
		}
	}
	// Pin a freshly fetched trusted commit independently of shared tracking refs.
	var trustedSHA string
	if repo.DefaultBranch != "" {
		if sha, err := fetchTrustedDefaultBranchSHA(ctx, wtDir, repo); err != nil {
			slog.Warn("failed to fetch trusted default branch", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else {
			trustedSHA = sha
		}
	}

	repoCfg, err := config.LoadRepo(wtDir)
	if err != nil {
		trackStartFailure("load_repo_config")
		return "", fmt.Errorf("load repo config: %w", err)
	}
	// SECURITY: load the code-executing selection fields (commands.* and
	// agent) from the trusted default-branch copy of .no-mistakes.yaml rather
	// than the pushed SHA. The worktree is checked out at headSHA (the
	// contributor's branch), so reading repoCfg above would honor a
	// contributor's commands/agent and let any pushed SHA run arbitrary shell
	// (sh -c) or pick the launched agent (incl. acp: targets) on the daemon
	// host with the maintainer's env (GH_TOKEN, SSH agent, ...).
	// EffectiveRepoConfig replaces commands + agent with the trusted
	// default-branch values unless the maintainer has explicitly opted in.
	//
	// allow_repo_commands is itself read ONLY from the trusted copy: a
	// contributor cannot self-enable it from the pushed branch. A readable
	// trusted tree with no config leaves the opt-in false and forces
	// commands/agent empty. An unreadable trusted tree aborts below.
	// SECURITY: a trusted-config fetch failure must abort, not silently disable
	// the disable_project_settings opt-out (see assertGateTrustedConfigReadable).
	if err := assertGateTrustedConfigReadable(ctx, wtDir, repo.DefaultBranch, trustedSHA); err != nil {
		trackStartFailure("trusted_config_unreadable")
		return "", err
	}
	trustedRepoCfg := loadTrustedRepoConfig(ctx, wtDir, trustedSHA, run.ID)
	allowRepoCommands := trustedRepoCfg != nil && trustedRepoCfg.AllowRepoCommands
	effectiveRepoCfg := config.EffectiveRepoConfig(repoCfg, trustedRepoCfg, allowRepoCommands)
	if allowRepoCommands {
		slog.Warn("allow_repo_commands is enabled on the default branch: honoring commands/agent from pushed branch", "run_id", run.ID, "branch", branch)
	} else if repoCfg.Commands != effectiveRepoCfg.Commands || repoCfg.Agent != effectiveRepoCfg.Agent || !agentListsEqual(repoCfg.Agents, effectiveRepoCfg.Agents) {
		// Surface the silent override so a maintainer who shipped a commands.*
		// or agent change on a feature branch understands why it did not run.
		// This is not an error: it is the secure default in action.
		slog.Info("repo commands/agent loaded from default branch, not pushed branch", "run_id", run.ID, "branch", branch, "default_branch", repo.DefaultBranch)
	}
	cfg := config.MergeForRemote(globalCfg, effectiveRepoCfg, repo.UpstreamURL)
	if err := cfg.Review.ValidatePathInstructionsBudget(); err != nil {
		trackStartFailure("review_path_instructions_budget")
		return "", err
	}
	if run.PiProfile != nil {
		if err := cfg.ValidatePiProfileAgents(); err != nil {
			return "", err
		}
		if err := cfg.ApplyPiProfile(run.PiProfile); err != nil {
			return "", err
		}
	}
	if err := m.paths.ValidateEvidenceRoot(cfg.Test.Evidence.LocalRoot); err != nil {
		trackStartFailure("evidence_root")
		return "", err
	}
	cfg.TrustedConfigSHA = trustedSHA
	if globalCfg.Eval.CaptureProvenance {
		if err := cfg.EnableEvalProvenance(globalCfg, effectiveRepoCfg); err != nil {
			trackStartFailure("eval_provenance")
			return "", err
		}
	}
	forgeCtx, err := forgecontext.Resolve(ctx, cfg.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
	if err == nil {
		err = forgecontext.RefuseProviderPluginOverlap(ctx, forgeCtx, cfg.ProviderPlugins, repo.UpstreamURL)
	}
	if err != nil {
		trackStartFailure("resolve_forge_profile")
		return "", fmt.Errorf("resolve forge profile: %w", err)
	}

	// Create agent. In demo mode, newPipelineAgent returns a no-op agent, and it
	// wires review-role routing plus the trusted-opt-out gate-neutralization
	// fail-closed check.
	ag, err := newPipelineAgent(ctx, cfg, m.paths.EvidenceRoot(cfg.Test.Evidence.LocalRoot), exec.LookPath, forgeEnvironment(forgeCtx))
	if err != nil {
		trackStartFailure("create_agent")
		return "", err
	}

	// Configuration decides this run's gates exactly once, here, and the
	// resolved list is recorded before the executor can write a single step
	// row. Every later consumer - above all crash recovery - reads that record
	// back through pinnedRunGates, so a gate merged onto (or removed from) the
	// default branch from here on is inert for this run instead of retargeting
	// its resume at a step sequence it never executed.
	pinnedGates, err := config.MarshalGates(cfg.Gates)
	if err != nil {
		trackStartFailure("record_gates")
		return "", fmt.Errorf("record gates: %w", err)
	}
	if err := m.db.SetRunGates(run.ID, pinnedGates); err != nil {
		trackStartFailure("record_gates")
		return "", fmt.Errorf("record gates: %w", err)
	}

	execSteps := steps.WithCustomGates(m.steps(), cfg.Gates)
	telemetry.Track("run", telemetry.Fields{
		"action":      "started",
		"trigger":     trigger,
		"agent":       string(cfg.Agent),
		"branch_role": branchRole,
		"step_count":  len(execSteps),
		"demo_mode":   steps.IsDemoMode(),
	})

	// Create executor with event broadcast.
	runCtx, cancel := handle.runCtx, handle.cancel
	executor := pipeline.NewExecutor(m.db, m.paths, cfg, ag, execSteps, m.broadcast)
	executor.SetRunSlot(slot)
	executor.SetRuntimeInstance(m.runtimeInstance)
	executor.SetForgeContext(forgeCtx)
	if hasRebaseCoreStep(execSteps) {
		executor.SetPRContextGuard(steps.GuardPRContext)
	}
	executor.SetSkippedSteps(skipSteps)
	executor.SetOnPRMerged(func(_ context.Context, runID string) {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.relabelEvalRun(context.Background(), cfg, runID)
		}()
	})

	// Attach the executor to the handle registered before setup, so cancellation
	// keeps the same owner as setup hands off to background execution.
	done := handle.done
	m.mu.Lock()
	// Shutdown admission is fenced under m.mu: a run whose setup raced a
	// shutdown ends through the setup-failure path instead of starting an
	// executor the shutdown can no longer wait for.
	if m.shutdownState.Load() != shutdownIdle {
		m.mu.Unlock()
		handle.cancel(pipeline.ErrDaemonShutdown)
		trackStartFailure("daemon_shutdown")
		return "", context.Cause(runCtx)
	}
	if cause := context.Cause(runCtx); cause != nil {
		m.mu.Unlock()
		return "", cause
	}
	m.runs[run.ID].executor = executor
	handle.consumed = true
	m.wg.Add(1)
	m.mu.Unlock()

	// Background goroutine now owns worktree cleanup and the run's slot.
	bgOwnsSlot = true

	// Launch pipeline in background.
	go func() {
		startedAt := time.Now()
		defer m.wg.Done()
		parkedAtShutdown := false
		// Registered before the recover handler below so it runs after it:
		// whether the pipeline completed, failed, was cancelled or panicked,
		// the slot is freed and the queue drains.
		defer slot.Release()
		defer func() {
			if r := recover(); r != nil {
				errMsg := fmt.Sprintf("internal panic: %v", r)
				slog.Error("panic in pipeline goroutine", "run_id", run.ID, "panic", r)
				run.Status = types.RunFailed
				run.Error = &errMsg
				fields := telemetry.Fields{
					"action":      "finished",
					"trigger":     trigger,
					"agent":       string(cfg.Agent),
					"branch_role": branchRole,
					"status":      string(run.Status),
					"duration_ms": time.Since(startedAt).Milliseconds(),
					"step_count":  len(execSteps),
					"pr_created":  run.PRURL != nil && *run.PRURL != "",
				}
				if failedStep := telemetryFailedStepName(m.db, run.ID); failedStep != "" {
					fields["failed_step"] = failedStep
				}
				addRunPerformanceSummary(m.db, run.ID, fields)
				telemetry.Track("run", fields)
				verifiedHead, verified := preserveRunHead(m.db, wtDir, run)
				var dbErr error
				if verified {
					dbErr = pipeline.RecordRunFailure(context.Background(), m.db, run.ID, errMsg, &verifiedHead)
				} else {
					dbErr = pipeline.RecordRunFailure(context.Background(), m.db, run.ID, errMsg, nil)
				}
				if dbErr != nil {
					slog.Error("failed to update run after panic", "run_id", run.ID, "error", dbErr)
				} else if err := executor.ReportSettledRunFailure(context.Background(), run, repo); err != nil {
					slog.Error("terminal notification failed", "run_id", run.ID, "error", err)
				}
			} else if run.Status == types.RunFailed || run.Status == types.RunCancelled {
				executor.ReportPublishedHeadFailure(context.Background(), run, repo)
			}
			// A run left for recovery keeps its non-terminal record by design;
			// any other run keeps custody until its terminal outcome settles.
			if parkedAtShutdown {
				if !m.releaseShutdownForRecovery(run.ID) {
					return
				}
			} else if !m.terminalRunSettled(run.ID) {
				return
			}
			cancel(nil)
			ag.Close()
			// Close subscriber channels for this run.
			m.closeSubscribers(run.ID)
			if !parkedAtShutdown {
				m.removeRunWorktree(repo.ID, run.ID, gateDir, wtDir, "run_finished")
				m.cleanupRunEvidence(cfg, run.ID)
			}
			// Remove tracking.
			m.mu.Lock()
			delete(m.runs, run.ID)
			m.mu.Unlock()
			close(done)
		}()

		err := executor.Execute(runCtx, run, repo, wtDir)
		// The pipeline's own work is over. Mark the run finishing first, so
		// abort and supersede leave its decided outcome alone even while
		// Release synchronously dispatches queued runs, then free the slot
		// before the eval collection below, which is local bookkeeping and
		// not pipeline load.
		m.markFinishing(run.ID)
		slot.Release()
		if pipeline.LeftForRecoveryAtShutdown(err) {
			parkedAtShutdown = true
			slog.Info("run left for recovery after daemon restart", "run_id", run.ID, "reason", err)
			return
		}
		if err != nil {
			fields := telemetry.Fields{
				"action":      "finished",
				"trigger":     trigger,
				"agent":       string(cfg.Agent),
				"branch_role": branchRole,
				"status":      string(run.Status),
				"duration_ms": time.Since(startedAt).Milliseconds(),
				"step_count":  len(execSteps),
				"pr_created":  run.PRURL != nil && *run.PRURL != "",
			}
			if failedStep := telemetryFailedStepName(m.db, run.ID); failedStep != "" {
				fields["failed_step"] = failedStep
			}
			addRunPerformanceSummary(m.db, run.ID, fields)
			telemetry.Track("run", fields)
			if sqlitewrite.IsContention(err) {
				slog.Error("pipeline infrastructure failure", "run_id", run.ID, "error", err)
			} else {
				slog.Error("pipeline failed", "run_id", run.ID, "error", err)
			}
		} else {
			fields := telemetry.Fields{
				"action":      "finished",
				"trigger":     trigger,
				"agent":       string(cfg.Agent),
				"branch_role": branchRole,
				"status":      string(run.Status),
				"duration_ms": time.Since(startedAt).Milliseconds(),
				"step_count":  len(execSteps),
				"pr_created":  run.PRURL != nil && *run.PRURL != "",
			}
			addRunPerformanceSummary(m.db, run.ID, fields)
			telemetry.Track("run", fields)
			slog.Info("pipeline completed", "run_id", run.ID)
		}
		// Collection runs here, on the finished run, because a case is only
		// honest once the human gate decision it labels is recorded - which is
		// exactly what reaching this point means. It is last on purpose: the
		// pipeline's own outcome is already decided and reported above, so
		// nothing below can change it.
		m.autoCaptureEvalCase(runCtx, cfg, run.ID)
		m.autoIngestCIFalseNegatives(runCtx, cfg, run.ID)
	}()

	return run.ID, nil
}

// evalAutoCaptureTimeout bounds one automatic collection pass. Collection is
// local Git and SQLite work whose slowest step is seeding a repository's object
// pool the first time it is seen, so this is generous rather than tight; the
// pass also stops early with the run context, which Shutdown cancels.
const evalAutoCaptureTimeout = 3 * time.Minute

// autoCaptureEvalCase freezes a finished run's review passes into the local
// eval corpus.
//
// Everything here is subordinate to the run: collection can be slow, can fail,
// can find nothing, and none of that may reach the pipeline. So it swallows its
// own panic rather than letting the run goroutine's recover mark a completed
// run as failed, it bounds its own time, and it reports failure only to the
// log. Runs are serialized against each other because they share one object
// pool and one registry file; the wait is harmless, since every run holding
// this lock has already finished its pipeline.
//
// A run with nothing to collect is the common case (no review step, a skipped
// gate, rounds recorded before provenance was on), so that outcome is DEBUG.
// Only a genuine capture fault is worth a warning.
func (m *RunManager) autoCaptureEvalCase(ctx context.Context, cfg *config.Config, runID string) {
	if cfg == nil || !cfg.Eval.AutoCapture || !cfg.Eval.CaptureProvenance {
		return
	}
	// A cancelled or aborted run has nothing worth freezing and every Git call
	// below would fail on the dead context anyway. Leaving early keeps that
	// ordinary outcome out of the warning log.
	if ctx.Err() != nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic while collecting eval case", "run_id", runID, "panic", r)
		}
	}()
	m.evalCaptureMu.Lock()
	defer m.evalCaptureMu.Unlock()

	if ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, evalAutoCaptureTimeout)
	defer cancel()

	result, err := eval.AutoCapture(ctx, m.paths, m.db, runID, cfg.Eval.MaxCases)
	switch {
	case err != nil:
		slog.Warn("failed to collect eval case", "run_id", runID, "error", err)
	case result.Skipped:
		slog.Debug("run has no eval case to collect", "run_id", runID, "reason", result.Reason)
	default:
		slog.Info("collected eval case", "run_id", runID, "cases", result.Captured, "pruned", result.Pruned)
	}
}

// autoIngestCIFalseNegatives writes false-negative gold for a finished run's
// fixed CI findings onto its green review case. Any real code defect CI
// surfaces (a failing ci-check or a review-bot comment), confirmed and fixed in
// the run, is by definition a Review false negative: Review passed green and
// missed it.
//
// Like autoCaptureEvalCase it is subordinate to the run: it swallows its own
// panic, bounds its own time, shares the eval mutex so it never races capture,
// and reports failure only to the log. It reads the CI findings the pipeline
// already persisted per round, so it never fabricates a case.
func (m *RunManager) autoIngestCIFalseNegatives(ctx context.Context, cfg *config.Config, runID string) {
	if cfg == nil || !cfg.Eval.AutoCapture || !cfg.Eval.CaptureProvenance {
		return
	}
	if ctx.Err() != nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic while ingesting CI false negatives", "run_id", runID, "panic", r)
		}
	}()
	m.evalCaptureMu.Lock()
	defer m.evalCaptureMu.Unlock()

	if ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, evalAutoCaptureTimeout)
	defer cancel()

	result, skipped, err := eval.AutoIngestCIFalseNegatives(ctx, m.paths, m.db, runID)
	switch {
	case err != nil:
		slog.Warn("failed to ingest CI false negatives", "run_id", runID, "error", err)
	case skipped:
		slog.Debug("run has no CI false negative to ingest", "run_id", runID)
	default:
		slog.Info("ingested CI false negatives", "run_id", runID, "case", result.CaseID, "added", result.Added, "total", result.Total)
	}
}

func (m *RunManager) relabelEvalRun(ctx context.Context, cfg *config.Config, runID string) {
	// Best-effort and off the CI step's call stack: a merge must not stall
	// the pipeline for eval I/O. The caller holds m.wg for daemon drain.
	if m == nil || m.paths == nil || m.db == nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic while relabeling eval case", "run_id", runID, "panic", r)
		}
	}()
	m.evalCaptureMu.Lock()
	defer m.evalCaptureMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, evalAutoCaptureTimeout)
	defer cancel()
	store, err := eval.Open(m.paths.EvalDir())
	if err != nil {
		slog.Warn("failed to open eval store for relabel", "run_id", runID, "error", err)
		return
	}
	defer store.Close()
	if cfg != nil {
		store.SetDiversifiedSize(cfg.Eval.DiversifiedSize)
	}
	if _, err := eval.RelabelRun(ctx, store, m.paths, m.db, runID); err != nil {
		slog.Warn("failed to relabel eval case after merge", "run_id", runID, "error", err)
	}
}

// addRunPerformanceSummary attaches the bounded per-run performance rollup
// to the terminal "run finished" event: low-cardinality counts only. The
// detailed per-invocation evidence (session keys, models, timings, tokens)
// stays in the local agent_invocations table and is never sent remotely.
func addRunPerformanceSummary(database *db.DB, runID string, fields telemetry.Fields) {
	summary, err := database.AgentInvocationSummaryForRun(runID)
	if err != nil {
		return
	}
	fields["agent_invocations"] = summary.Count
	fields["resumed_invocations"] = summary.Resumed
	fields["fallback_invocations"] = summary.Fallback
}

func telemetryBranchRole(branch, defaultBranch string) string {
	if branch == "" {
		return "unknown"
	}
	if defaultBranch != "" && branch == defaultBranch {
		return "default"
	}
	return "feature"
}

func telemetryFailedStepName(database *db.DB, runID string) string {
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return ""
	}
	for _, step := range steps {
		if step.Status == types.StepStatusFailed {
			return telemetry.StepName(step.StepName)
		}
	}
	return ""
}

// activeAgentPIDs snapshots live executors without depending on persisted state.
func (m *RunManager) activeAgentPIDs() []int {
	m.mu.Lock()
	var executors []*pipeline.Executor
	for _, owned := range m.runs {
		if owned.executor != nil {
			executors = append(executors, owned.executor)
		}
	}
	m.mu.Unlock()
	var pids []int
	for _, exec := range executors {
		if pid := exec.ActiveAgentPID(); pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// HandleRespond routes a user approval action to the executor for the given run.
func (m *RunManager) HandleRespond(runID string, step types.StepName, action types.ApprovalAction, findingIDs []string, expected *types.GateIdentity) error {
	_, err := m.HandleRespondWithOverrides(runID, step, action, findingIDs, nil, nil, nil, "", expected)
	return err
}

// HandleRespondWithOverrides is like HandleRespond but also forwards explicit
// declines, user instructions, and user-authored findings to the executor, and
// returns the dispositions the response recorded.
func (m *RunManager) HandleRespondWithOverrides(runID string, step types.StepName, action types.ApprovalAction, findingIDs, ignoreFindingIDs []string, instructions map[string]string, addedFindings []types.Finding, approvalReason string, expected *types.GateIdentity) (pipeline.RespondDispositions, error) {
	return m.HandleRespondWithAcceptedGaps(runID, step, action, findingIDs, ignoreFindingIDs, instructions, addedFindings, approvalReason, nil, expected)
}

// HandleRespondWithAcceptedGaps is HandleRespondWithOverrides for an approval
// that may carry an owner-accepted Test gap. The pre-check here and the
// executor's live gate share db.StepResult.AcceptedGapRefusal.
func (m *RunManager) HandleRespondWithAcceptedGaps(runID string, step types.StepName, action types.ApprovalAction, findingIDs, ignoreFindingIDs []string, instructions map[string]string, addedFindings []types.Finding, approvalReason string, gaps *db.AcceptedGaps, expected *types.GateIdentity) (pipeline.RespondDispositions, error) {
	if action == types.ActionAbort {
		m.mu.Lock()
		owned := m.runs[runID]
		var executor *pipeline.Executor
		recovering := owned != nil && owned.state == ownershipRecoveringGate
		if owned != nil {
			executor = owned.executor
		}
		m.mu.Unlock()
		// Recovery has not dispatched the run and may have no slot. Stop
		// it through the same cancellation path as a standalone run abort.
		if recovering {
			return pipeline.RespondDispositions{}, m.HandleCancel(runID)
		}
		if executor != nil {
			// A waiting gate keeps its original failed outcome and reporting.
			// Admission checks under the executor lock distinguish an accepted
			// response, which cannot consume another response while slot-bound.
			result, err := executor.RespondWithAcceptedGaps(step, action, findingIDs, ignoreFindingIDs, instructions, addedFindings, approvalReason, gaps, expected)
			var pending *pipeline.ResponsePendingError
			if errors.As(err, &pending) {
				return pipeline.RespondDispositions{}, m.HandleCancel(runID)
			}
			return result, err
		}
	}
	run, err := m.db.GetRun(runID)
	if err != nil {
		return pipeline.RespondDispositions{}, err
	}
	// A completed run may no longer have an executor. A bound late response
	// still gets the typed current identity rather than a transport error.
	if action != types.ActionAbort {
		m.mu.Lock()
		owned := m.runs[runID]
		var executor *pipeline.Executor
		if owned != nil {
			executor = owned.executor
		}
		m.mu.Unlock()
		current := types.GateIdentity{RunID: runID}
		if executor != nil {
			current = executor.CurrentGate()
		} else {
			rows, readErr := m.db.GetStepsByRun(runID)
			if readErr != nil {
				return pipeline.RespondDispositions{}, readErr
			}
			for _, row := range rows {
				if row.StepName == step {
					current = row.GateIdentity()
				}
				if row.Status == types.StepStatusAwaitingApproval || row.Status == types.StepStatusFixReview {
					current = row.GateIdentity()
					break
				}
			}
		}
		if executor == nil || expected == nil || !expected.SameGate(current) {
			return pipeline.RespondDispositions{}, &pipeline.GateSupersededError{Current: current}
		}
	}
	if gaps != nil && action != types.ActionApprove {
		return pipeline.RespondDispositions{}, fmt.Errorf("--accept-gap requires --action approve")
	}
	if run != nil && run.Shipping == nil && gaps != nil {
		rows, err := m.db.GetStepsByRun(runID)
		if err != nil {
			return pipeline.RespondDispositions{}, err
		}
		for _, row := range rows {
			if row.StepName == step {
				if refusal := row.AcceptedGapRefusal(false, *gaps); refusal != "" {
					return pipeline.RespondDispositions{}, errors.New(refusal)
				}
			}
		}
	}
	if run != nil && run.Shipping != nil {
		if action == types.ActionSkip {
			return pipeline.RespondDispositions{}, fmt.Errorf("strict shipping forbids skipped steps")
		}
		if action == types.ActionFix && run.Shipping.Mode == types.ShippingImmutableSource {
			return pipeline.RespondDispositions{}, fmt.Errorf("immutable source forbids repairs")
		}
		if action == types.ActionApprove {
			rows, err := m.db.GetStepsByRun(runID)
			if err != nil {
				return pipeline.RespondDispositions{}, err
			}
			for _, row := range rows {
				if row.StepName == step {
					if refusal := row.StrictApprovalRefusalWithGaps(approvalReason, gaps); refusal != "" {
						return pipeline.RespondDispositions{}, errors.New(refusal)
					}
				}
			}
		}
	}
	m.mu.Lock()
	owned := m.runs[runID]
	var exec *pipeline.Executor
	if owned != nil {
		exec = owned.executor
	}
	m.mu.Unlock()
	ok := exec != nil

	if !ok {
		if run != nil && run.Status == types.RunQueued {
			return pipeline.RespondDispositions{}, fmt.Errorf("run %s is queued and has not started; there is no gate to respond to yet", runID)
		}
		return pipeline.RespondDispositions{}, fmt.Errorf("no active executor for run %s", runID)
	}

	return exec.RespondWithAcceptedGaps(step, action, findingIDs, ignoreFindingIDs, instructions, addedFindings, approvalReason, gaps, expected)
}

// HandleAnswerReviewQuestion records an operator answer in the run's review
// conversation.
//
// Answer delivery, including superseded gate releases, is owned by
// docs/src/content/docs/concepts/review-conversation.md.
//
// Every answer is stamped with the ask it settles, so a correction binds to the
// already-settled ask instead of pre-answering a later re-ask of the same id.
// The two ways that stamp cannot be trusted are refused before anything is
// written, and they stay distinguishable from each other and from a recorded
// answer that closed no open question: a conversation that cannot be READ AT
// ALL, and one whose question history could not be read TO THE END.
//
// The write happens before the release decision, so a failure to resume never
// loses the answer - the next answer, or a recovered gate, finds it on disk.
func (m *RunManager) HandleAnswerReviewQuestion(runID, questionID, answer, answeredBy string) (*ipc.AnswerReviewQuestionResult, error) {
	questionID = strings.TrimSpace(questionID)
	answer = strings.TrimSpace(answer)
	if questionID == "" || answer == "" {
		return nil, fmt.Errorf("answering a review question needs a question id and an answer")
	}

	m.mu.Lock()
	owned := m.runs[runID]
	var exec *pipeline.Executor
	if owned != nil {
		exec = owned.executor
	}
	m.mu.Unlock()
	ok := exec != nil
	if !ok {
		return nil, fmt.Errorf("no active executor for run %s", runID)
	}
	// Capture before reading the conversation or publishing the answer. Another
	// answer may settle that snapshot while this handler is delayed. Even a zero
	// identity is explicit: a later park must not inherit this response.
	expectedGate := exec.CurrentGate()
	// A repository that has not turned the conversation on has no channel to
	// answer into, and the reviewer was never told to ask, so say which setting
	// would accept an answer rather than report a missing directory.
	// A question already on disk stays answerable even if review.conversation
	// has since been turned off: the reviewer asked it while the channel was
	// open, and the review step reads the same conversation from disk for the
	// finalize turn, so the answer reaches it. A repository that never enabled
	// the conversation has no questions file, so it is refused by naming the
	// setting exactly as before.
	dir := exec.ReviewConversationAnswerDir(runID)
	if dir == "" {
		if !exec.ReviewConversationEnabled() {
			return nil, fmt.Errorf("run %s has no review conversation: set review.conversation: true in .no-mistakes.yaml on the default branch to let the reviewer ask questions", runID)
		}
		return nil, fmt.Errorf("run %s has no review conversation directory", runID)
	}
	// Snapshot what was OPEN before the append, because "nothing is open now"
	// is not evidence that THIS answer closed anything. An answer for an id
	// nobody asked is recorded as an orphan and leaves the open count at zero,
	// as does a duplicate or corrected answer sent after the last question was
	// already closed. Releasing on the count alone let either of those release
	// a review gate that had parked on ordinary ask-user CODE findings: the
	// step re-executed as a finalize turn, burned a review round, and the
	// operator's pending verdict never happened - their next axi respond then
	// failed with "no step awaiting approval".
	//
	// The same snapshot supplies the ask this answer settles. reviewqa cannot
	// recover that at load time - the two files are appended independently, so
	// two asks and two answers read the same whether the second answer is a
	// correction to the first ask or the answer to a re-ask - and binding it
	// here, at the only writer of answers.ndjson, is what stops a correction
	// pre-answering the next re-ask of that id.
	//
	// So a conversation this cannot READ is refused rather than written
	// through: an unstamped answer settles nothing, so the questions it was
	// meant for would park forever with the operator told they had answered
	// them. The refusal names the read failure, because an operator told "no open
	// question" about a conversation nobody could read would go looking for
	// the wrong thing entirely. A conversation that reads fine with nothing
	// open is unaffected: that answer is still recorded, still stamped, and
	// still releases no gate.
	before, err := reviewqa.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("read run %s's review conversation before recording the answer: %w", runID, err)
	}
	// A question history the reader could not reach the end of is refused for
	// the same reason, and it is a DIFFERENT failure from the one above: the
	// file opened and parsed, so nothing here errored, but a later ask of any
	// id is past the seen region, so the load settles nothing and never will.
	// Stamping an answer against it would record an answer that can never
	// close its question, leaving the gate parked forever with the operator
	// told they had answered it. Refusing before the append is what keeps this
	// from becoming that silent strand - and it is not the ordinary case of an
	// answer that closed nothing, which reads fine, stays recorded and is
	// deliberately not an error.
	if before.QuestionsIncomplete {
		return nil, fmt.Errorf("run %s's review question history could not be read to the end (%s), so an answer cannot be bound to the ask it settles; nothing was recorded - read that file for the questions asked, and resolve the parked review with `no-mistakes axi respond` instead", runID, filepath.Join(dir, reviewqa.QuestionsFile))
	}
	wasOpen := false
	askOrdinal := 0
	for _, e := range before.Open() {
		if e.ID == questionID {
			wasOpen = true
			break
		}
	}
	for _, ask := range before.Asks {
		if ask.Question.ID == questionID {
			askOrdinal = ask.Ordinal
		}
	}

	if err := reviewqa.AppendAnswer(dir, reviewqa.Answer{
		ID:         questionID,
		Answer:     answer,
		AnsweredBy: strings.TrimSpace(answeredBy),
		AskOrdinal: askOrdinal,
	}); err != nil {
		return nil, err
	}

	conv, err := reviewqa.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("read review conversation after recording the answer: %w", err)
	}
	open := conv.Open()
	result := &ipc.AnswerReviewQuestionResult{OK: true, Open: len(open)}
	for _, e := range open {
		result.OpenIDs = append(result.OpenIDs, e.ID)
	}
	if len(open) > 0 {
		result.Note = "recorded; the reviewer still has open questions"
		return result, nil
	}
	if !wasOpen {
		// Recorded durably and deliberately inert: it corrects the ask it is
		// stamped with, and no gate is touched. A later re-ask of this id is a
		// different question and stays open until it is answered itself.
		result.Note = "recorded; it answered no open question, so no review gate was released"
		return result, nil
	}
	result.ClosedLast = true
	if expectedGate.Nonce == "" || expectedGate.Step != types.StepReview {
		result.Note = "recorded; the review gate was not released because none was parked when this answer began; the reviewer will read it at its next checkpoint"
		return result, nil
	}
	if m.beforeAnswerRespond != nil {
		m.beforeAnswerRespond()
	}
	if err := exec.Respond(types.StepReview, types.ActionAnswer, nil, &expectedGate); err != nil {
		// The answer stays recorded if this gate was consumed or superseded.
		// Automatic resumption already delivered it, or a later park will read it.
		// Report success without dispatching it against a replacement gate.
		result.Note = fmt.Sprintf("recorded; the review gate was not released (%v)", err)
		return result, nil
	}
	result.Resumed = true
	result.Note = "recorded; every question is answered and the reviewer was resumed"
	return result, nil
}

// Shutdown cancels all active runs. Called during daemon shutdown to prevent
// orphaned goroutines from continuing agent calls and git operations.
func (m *RunManager) Shutdown() {
	m.shutdownState.Store(shutdownRaw)
	m.stopLimitWatcher() // no watcher may start after shutdown begins

	m.mu.Lock()
	cancels := m.runCancelsLocked()
	ids := make([]string, 0, len(cancels))
	for id := range cancels {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	// A raw shutdown durably supersedes any uncommitted preservation before the
	// cancellations it races with can terminalize a run.
	if err := pipeline.RecordRawShutdown(context.Background(), m.db, ids, m.runtimeInstance, func() {
		for id, cancel := range cancels {
			cancel(pipeline.ErrDaemonShutdown)
			slog.Info("cancelled run on shutdown", "run_id", id)
		}
	}); err != nil {
		slog.Error("raw shutdown terminal outcome unsettled", "error", err)
	}

	m.wg.Wait()
}

// runCancelsLocked snapshots the cancel handle of every run that has one.
// Queued runs have none: they stay queued across a shutdown.
func (m *RunManager) runCancelsLocked() map[string]context.CancelCauseFunc {
	cancels := make(map[string]context.CancelCauseFunc, len(m.runs))
	for id, owned := range m.runs {
		if owned.handle != nil {
			cancels[id] = owned.handle.cancel
		}
	}
	return cancels
}

// terminalRunSettled reports whether a run's terminal outcome is durable.
// Until it is, the run goroutine keeps custody of the run.
func (m *RunManager) terminalRunSettled(runID string) bool {
	run, err := pipeline.ReadSettledRun(m.db, runID)
	if err != nil || run == nil || !run.Status.Terminal() {
		slog.Error("terminal outcome unsettled; retaining run custody", "run_id", runID, "error", err)
		return false
	}
	return true
}

// releaseShutdownForRecovery settles the shutdown intent of a run the
// shutdown deliberately left non-terminal (parked at a gate or monitoring CI).
// Such a run follows startup recovery, so its shutdown record must
// not make the next start treat it as an unsettled, obsolete shutdown.
func (m *RunManager) releaseShutdownForRecovery(runID string) bool {
	if err := pipeline.SettleTerminalWrite(m.db, func() error {
		return m.db.ReleaseShutdownForRecovery(runID)
	}); err != nil {
		slog.Error("failed to settle shutdown intent for a run left for recovery", "run_id", runID, "error", err)
		return false
	}
	m.mu.Lock()
	m.leftForRecovery[runID] = true
	m.mu.Unlock()
	return true
}

// HandleCancel stops an active run and propagates cancellation to the executor.
func (m *RunManager) HandleCancel(runID string) error {
	m.mu.Lock()
	owned := m.runs[runID]
	m.mu.Unlock()
	if owned == nil {
		return fmt.Errorf("no active run %s", runID)
	}
	_, err := m.cancelManagedRun(runID, types.RunCancelReasonAbortedByUser)
	return err
}

// cancelActiveRuns cancels any in-progress runs for the given repo+branch
// and waits for their goroutines to finish before returning, preventing
// concurrent pushes to upstream.
// The cancellation cause is propagated via context.Cause; Executor.FailRun
// keeps an independent execution failure rather than replacing it with that cause.
func (m *RunManager) cancelActiveRuns(repoID, branch string) error {
	return m.cancelRunIDs(m.activeRunIDs(repoID, branch))
}

func (m *RunManager) activeRunIDs(repoID, branch string) []string {
	m.mu.Lock()
	var ids []string
	for id, owned := range m.runs {
		// A finishing run's outcome is decided and its pipeline is over, so a
		// new push neither cancels nor waits for it.
		if owned.run.RepoID == repoID && owned.run.Branch == branch && owned.state != ownershipFinishing {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	return ids
}

func (m *RunManager) cancelRunIDs(ids []string) error {
	if m.afterSupersedeSnapshot != nil {
		m.afterSupersedeSnapshot()
	}
	// The snapshot only names candidates. Each is cancelled by re-reading its
	// state under the ownership lock (cancelManagedRun), which also hands back
	// the completion channel of the handle it actually signalled - so a run that
	// dispatch claimed after the snapshot is waited for like any other.
	var toWait []<-chan struct{}
	for _, id := range ids {
		done, err := m.cancelManagedRun(id, types.RunCancelReasonSuperseded)
		if err != nil {
			return err
		}
		if done != nil {
			toWait = append(toWait, done)
		}
	}

	timeout := time.After(30 * time.Second)
	for _, done := range toWait {
		select {
		case <-done:
		case <-timeout:
			slog.Warn("timed out waiting for cancelled runs to finish")
			return fmt.Errorf("timed out waiting for cancelled runs")
		}
	}
	return nil
}
