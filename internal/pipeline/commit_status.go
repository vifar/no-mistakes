package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const commitStatusTimeout = 15 * time.Second

// CommitStatusLock serializes publication ownership, terminal decisions and POSTs.
var CommitStatusLock = make(chan struct{}, 1)
var terminalSettlementMu sync.Mutex
var terminalSettlements []*publicationSettlement

type publicationSettlement struct {
	write       func() error
	done        chan error
	unavailable func() bool
}

var rawPublicationSettlements []*publicationSettlement

var publicationRecoveryRunning bool

func startPublicationRecoveryLocked() {
	if publicationRecoveryRunning {
		return
	}
	publicationRecoveryRunning = true
	go func() {
		for {
			timer := time.NewTimer(time.Second)
			<-timer.C
			ctx, cancel := context.WithTimeout(context.Background(), commitStatusTimeout)
			if err := AcquireCommitStatusLock(ctx); err == nil {
				ReleaseCommitStatusLock()
			}
			cancel()
			terminalSettlementMu.Lock()
			if len(rawPublicationSettlements) == 0 && len(terminalSettlements) == 0 {
				publicationRecoveryRunning = false
				terminalSettlementMu.Unlock()
				return
			}
			terminalSettlementMu.Unlock()
		}
	}()
}

func drainPublicationSettlements() error {
	for {
		terminalSettlementMu.Lock()
		for i := 0; i < len(terminalSettlements); {
			pending := terminalSettlements[i]
			if !pending.unavailable() {
				i++
				continue
			}
			terminalSettlements = append(terminalSettlements[:i], terminalSettlements[i+1:]...)
			pending.done <- fmt.Errorf("database closed before terminal settlement")
		}
		if len(rawPublicationSettlements) > 0 {
			pending := rawPublicationSettlements[0]
			terminalSettlementMu.Unlock()
			err := pending.write()
			terminalSettlementMu.Lock()
			if err != nil {
				startPublicationRecoveryLocked()
				for _, owned := range rawPublicationSettlements {
					select {
					case owned.done <- err:
					default:
					}
				}
				terminalSettlementMu.Unlock()
				return err
			}
			rawPublicationSettlements = rawPublicationSettlements[1:]
			select {
			case pending.done <- nil:
			default:
			}
			terminalSettlementMu.Unlock()
			continue
		}
		if len(terminalSettlements) == 0 {
			terminalSettlementMu.Unlock()
			return nil
		}
		pending := terminalSettlements[0]
		terminalSettlementMu.Unlock()
		err := pending.write()
		terminalSettlementMu.Lock()
		if err != nil && !pending.unavailable() {
			startPublicationRecoveryLocked()
			terminalSettlementMu.Unlock()
			return err
		}
		terminalSettlements = terminalSettlements[1:]
		pending.done <- err
		terminalSettlementMu.Unlock()
	}
}

func ReleaseCommitStatusLock() {
	for {
		if err := drainPublicationSettlements(); err != nil {
			terminalSettlementMu.Lock()
			<-CommitStatusLock
			terminalSettlementMu.Unlock()
			return
		}
		terminalSettlementMu.Lock()
		if len(terminalSettlements) == 0 && len(rawPublicationSettlements) == 0 {
			<-CommitStatusLock
			terminalSettlementMu.Unlock()
			return
		}
		terminalSettlementMu.Unlock()
	}
}

func SettleIneligibleTerminal(d *db.DB, runID, message string, status types.RunStatus) error {
	return SettleTerminalWrite(d, func() error { return d.RecordIneligibleTerminal(runID, message, status, nil) })
}

func SettleTerminalWrite(d *db.DB, write func() error) error {
	if d.Closed() {
		return fmt.Errorf("database closed before terminal settlement")
	}
	done := make(chan error, 1)
	terminalSettlementMu.Lock()
	terminalSettlements = append(terminalSettlements, &publicationSettlement{write: write, done: done, unavailable: d.Closed})
	startPublicationRecoveryLocked()
	terminalSettlementMu.Unlock()
	select {
	case CommitStatusLock <- struct{}{}:
		ReleaseCommitStatusLock()
	default:
	}
	return <-done
}

func AcquireCommitStatusLock(ctx context.Context) error {
	select {
	case CommitStatusLock <- struct{}{}:
		if err := drainPublicationSettlements(); err != nil {
			terminalSettlementMu.Lock()
			<-CommitStatusLock
			terminalSettlementMu.Unlock()
			return err
		}
		if err := ctx.Err(); err != nil {
			ReleaseCommitStatusLock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func WithCommitStatusLock(ctx context.Context, write func() error) error {
	bounded, cancel := context.WithTimeout(ctx, commitStatusTimeout)
	defer cancel()
	if err := AcquireCommitStatusLock(bounded); err != nil {
		return err
	}
	defer ReleaseCommitStatusLock()
	return write()
}

// RecordRunFailure is shared by daemon panic and recovery fallbacks.
func RecordRunFailure(ctx context.Context, d *db.DB, runID, message string, head *string) error {
	return RecordRunTerminal(ctx, d, runID, message, types.RunFailed, head)
}

// RecordRunTerminal records a failed or cancelled fallback outcome through the
// same settled, preservation-revoking write as RecordRunFailure.
func RecordRunTerminal(ctx context.Context, d *db.DB, runID, message string, status types.RunStatus, head *string) error {
	err := WithCommitStatusLock(ctx, func() error { return d.RecordIneligibleTerminal(runID, message, status, head) })
	if err != nil {
		slog.Warn("recording ineligible terminal fallback", "run", runID, "error", err)
		if settlementErr := SettleIneligibleTerminal(d, runID, message, status); settlementErr != nil {
			return errors.Join(err, settlementErr)
		}
	}
	return nil
}

// PublishedHeadStatusReporter is an optional step hook for best-effort status
// updates at run completion, failure, and CI readiness, including recovery.
type PublishedHeadStatusReporter interface {
	ReportPublishedHeadStatus(*StepContext, string, string)
}

// PublishedHeadStatusAttacher is an optional step hook that adopts an exact
// already-published run head before validation starts or resumes.
type PublishedHeadStatusAttacher interface {
	AttachPublishedHeadStatus(*StepContext)
}

func (e *Executor) attachPublishedHeadStatus(ctx context.Context, run *db.Run, repo *db.Repo) {
	for _, step := range e.steps {
		if attacher, ok := step.(PublishedHeadStatusAttacher); ok {
			e.withCommitStatusContext(ctx, run, repo, attacher.AttachPublishedHeadStatus)
		}
	}
}

func (e *Executor) reportPublishedHeadStatus(ctx context.Context, run *db.Run, repo *db.Repo, state string, validatedSHAs ...string) {
	if state == "failure" && e.storageFailure(run) {
		slog.Error("SQLITE_WRITE_CONTENTION: infrastructure failure; preserving published-head status", "run", run.ID)
		return
	}
	e.withCommitStatusContext(ctx, run, repo, func(sctx *StepContext) {
		for _, step := range e.steps {
			if reporter, ok := step.(PublishedHeadStatusReporter); ok {
				validatedSHA := ""
				if len(validatedSHAs) > 0 {
					validatedSHA = validatedSHAs[0]
				}
				reporter.ReportPublishedHeadStatus(sctx, state, validatedSHA)
			}
		}
	})
}

func (e *Executor) withCommitStatusContext(runCtx context.Context, run *db.Run, repo *db.Repo, report func(*StepContext)) {
	ctx, cancel := context.WithTimeout(runCtx, commitStatusTimeout)
	defer cancel()
	log := func(message string) {
		// Terminal updates use a fresh file handle: the Push step's log is closed.
		if e.paths != nil {
			if f, err := os.OpenFile(filepath.Join(e.paths.RunLogDir(run.ID), "push.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); err == nil {
				fmt.Fprintln(f, message)
				f.Close()
			}
		}
		e.emitLogChunk(run, repo, types.StepPush, message+"\n")
	}
	statusConfig := e.config
	forge := e.forge
	if forge == nil {
		var profiles config.ForgeProfiles
		var plugins config.ProviderPlugins
		var err error
		if e.config != nil {
			profiles, plugins = e.config.ForgeProfiles, e.config.ProviderPlugins
		} else if e.paths != nil {
			var global *config.GlobalConfig
			global, err = config.LoadGlobal(e.paths.ConfigFile())
			if err == nil {
				profiles, plugins = global.ForgeProfiles, global.ProviderPlugins
				statusConfig = config.Merge(global, &config.RepoConfig{})
			}
		}
		if err == nil {
			remote := repo.UpstreamURL
			if remote == "" && run.PRURL != nil {
				remote = *run.PRURL
			}
			if name, owned := plugins.Select(scm.ExtractHost(remote), func() string { return scm.ResolveHost(ctx, remote) }); owned {
				message := fmt.Sprintf("warning: no-mistakes commit status suppressed: provider plugin %q does not support commit statuses", name)
				slog.Warn(message)
				log(message)
				return
			}
		}
		if err == nil {
			forge, err = forgecontext.Resolve(ctx, profiles, repo.UpstreamURL, repo.ForkURL)
		}
		if err == nil {
			err = forgecontext.RefuseProviderPluginOverlap(ctx, forge, plugins, repo.UpstreamURL)
		}
		if err != nil {
			message := safeurl.RedactText(fmt.Sprintf("warning: no-mistakes commit status resolve forge profile: %v", err))
			slog.Warn(message)
			log(message)
			return
		}
	}
	sctx := &StepContext{Ctx: ctx, Run: run, Repo: repo, DB: e.db, Config: statusConfig, ForgeContext: forge, WorkDir: e.workDir, Shared: e.shared, Log: log}
	report(sctx)
}

func (e *Executor) ReportPublishedHeadFailure(ctx context.Context, run *db.Run, repo *db.Repo) {
	e.reportPublishedHeadStatus(ctx, run, repo, "failure")
}

// ReportRecoveredPublishedHeadStatus reconciles the durable terminal outcome,
// including runs terminalized before a crash that interrupted their final POST.
func (e *Executor) ReportRecoveredPublishedHeadStatus(ctx context.Context, run *db.Run, repo *db.Repo) {
	if info, err := os.Stat(repo.WorkingPath); err == nil && info.IsDir() {
		e.workDir = repo.WorkingPath
	}
	state := "failure"
	if run.Status == types.RunCompleted {
		state = "success"
	}
	e.reportPublishedHeadStatus(ctx, run, repo, state)
}

func (e *Executor) ReportSettledRunFailure(ctx context.Context, run *db.Run, repo *db.Repo) error {
	durable, err := ReadSettledRun(e.db, run.ID)
	if err != nil {
		return err
	}
	if durable == nil || (durable.Status != types.RunFailed && durable.Status != types.RunCancelled) {
		return fmt.Errorf("terminal failure outcome unsettled")
	}
	run.Status, run.Error, run.HeadSHA = durable.Status, durable.Error, durable.HeadSHA
	e.ReportPublishedHeadFailure(ctx, run, repo)
	e.emitRunEvent(ipc.EventRunCompleted, run, repo)
	return nil
}

func RecordRawShutdown(ctx context.Context, d *db.DB, runIDs []string, instance string, cancelRuns func()) error {
	if len(runIDs) == 0 {
		return nil
	}
	var cancelOnce sync.Once
	write := func() error {
		cancelOnce.Do(cancelRuns)
		return d.SupersedeUncommittedCancellations(runIDs, instance)
	}
	pending := &publicationSettlement{write: write, done: make(chan error, 1)}
	terminalSettlementMu.Lock()
	rawPublicationSettlements = append(rawPublicationSettlements, pending)
	terminalSettlementMu.Unlock()
	bounded, cancel := context.WithTimeout(ctx, commitStatusTimeout)
	defer cancel()
	if err := AcquireCommitStatusLock(bounded); err != nil {
		slog.Warn("raw shutdown terminal outcome unsettled", "error", err)
		return err
	}
	ReleaseCommitStatusLock()
	if err := <-pending.done; err != nil {
		slog.Warn("raw shutdown terminal outcome unsettled", "error", err)
		return err
	}
	return nil
}

func ReadSettledRun(d *db.DB, runID string) (*db.Run, error) {
	durable, err := d.GetRun(runID)
	if err != nil {
		err = SettleTerminalWrite(d, func() error {
			var readErr error
			durable, readErr = d.GetRun(runID)
			return readErr
		})
	}
	return durable, err
}
