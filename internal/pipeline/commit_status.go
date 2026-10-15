package pipeline

import (
	"context"
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
