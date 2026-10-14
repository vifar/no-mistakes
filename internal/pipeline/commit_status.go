package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const commitStatusTimeout = 15 * time.Second

// CommitStatusLock serializes publication ownership, terminal decisions and POSTs.
var CommitStatusLock = make(chan struct{}, 1)

func AcquireCommitStatusLock(ctx context.Context) error {
	select {
	case CommitStatusLock <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-CommitStatusLock
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
	defer func() { <-CommitStatusLock }()
	return write()
}

func UpdateRunPRState(ctx context.Context, d *db.DB, runID, state string) error {
	return WithCommitStatusLock(ctx, func() error { return d.UpdateRunPRState(runID, state) })
}

// RecordRunFailure is shared by daemon panic and recovery fallbacks.
func RecordRunFailure(ctx context.Context, d *db.DB, runID, message string, head *string) error {
	err := WithCommitStatusLock(ctx, func() error { return d.RecordIneligibleTerminal(runID, message, types.RunFailed, head) })
	if err != nil {
		slog.Warn("recording ineligible terminal fallback", "run", runID, "error", err)
		if settlementErr := WithCommitStatusLock(context.Background(), func() error { return d.RecordIneligibleTerminal(runID, message, types.RunFailed, nil) }); settlementErr != nil {
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

func (e *Executor) reportPublishedHeadStatus(runCtx context.Context, run *db.Run, repo *db.Repo, state string, validatedSHAs ...string) {
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
	for _, step := range e.steps {
		if reporter, ok := step.(PublishedHeadStatusReporter); ok {
			validatedSHA := ""
			if len(validatedSHAs) > 0 {
				validatedSHA = validatedSHAs[0]
			}
			reporter.ReportPublishedHeadStatus(sctx, state, validatedSHA)
		}
	}
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
