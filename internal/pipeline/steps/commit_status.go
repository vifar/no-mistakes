package steps

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var commitStatusLock = make(chan struct{}, 1)

const commitStatusOperationTimeout = 15 * time.Second

func acquireCommitStatusLock(ctx context.Context) error {
	select {
	case commitStatusLock <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-commitStatusLock
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Plugins currently expose PR/CI operations, but no commit-status capability.
// Their host ownership must also apply to persisted publications from older runs.
func suppressPluginCommitStatus(sctx *pipeline.StepContext, hosts ...string) bool {
	name, owned := "", false
	if sctx.Repo != nil {
		name, owned = providerPluginForStep(sctx)
	}
	if !owned && sctx.Config != nil {
		for _, host := range hosts {
			if name, owned = sctx.Config.ProviderPlugins.Select(host, nil); owned {
				break
			}
		}
	}
	if owned {
		statusWarning(sctx.Log, "suppressed", fmt.Errorf("provider plugin %q does not support commit statuses", name))
	}
	return owned
}

// AttachPublishedHeadStatus replaces stale statuses only after proving that
// this run's current head is already at its exact push target. It grants no
// validation credit. Confirmation and reporting share the existing ownership
// lock with terminal/recovery reporters, so an older POST either finishes
// before adoption or observes its permanently retired publication.
func (*PushStep) AttachPublishedHeadStatus(sctx *pipeline.StepContext) {
	if suppressPluginCommitStatus(sctx) || resolvedProvider(sctx) != scm.ProviderGitHub || sctx.Run.HeadSHA == "" {
		return
	}
	ctx, cancel := context.WithTimeout(sctx.Ctx, commitStatusOperationTimeout)
	defer cancel()
	bounded := *sctx
	bounded.Ctx = ctx
	publication, err := attachCommitStatusPublication(&bounded)
	if err != nil {
		statusWarning(sctx.Log, "attach published head", err)
		return
	}
	if publication != nil && (publication.LastState == "" || publication.LastState == "pending") {
		commitStatusReporter(&bounded, publication)(ctx, "pending", "", sctx.Log)
	}
}

func attachCommitStatusPublication(sctx *pipeline.StepContext) (*db.CommitStatusPublication, error) {
	if err := acquireCommitStatusLock(sctx.Ctx); err != nil {
		return nil, err
	}
	defer func() { <-commitStatusLock }()
	target := resolvePushURL(sctx)
	if scm.ExtractHost(target) == "" {
		return nil, nil
	}
	publication := db.CommitStatusPublication{SHA: sctx.Run.HeadSHA, Host: resolvedHost(sctx, target), Repo: github.RepoSlug(target)}
	if publication.Repo == "" {
		return nil, nil
	}
	current, err := sctx.DB.GetCommitStatusPublication(sctx.Ctx, sctx.Run.ID, publication)
	if err != nil || current != nil {
		return current, err
	}
	ref := normalizedBranchRef(sctx.Run.Branch)
	remote, err := commitStatusRemoteSHA(sctx, target, ref)
	if err != nil || remote != publication.SHA {
		return nil, err
	}
	// Preserve the validation evidence of a pre-crash push attempt. An
	// attachment without such an attempt always starts unvalidated.
	attempt, err := sctx.DB.GetCommitStatusAttempt(sctx.Run.ID)
	if err != nil {
		return nil, err
	}
	if attempt != nil && attempt.SHA == publication.SHA && attempt.Target == safeurl.Redact(target) && attempt.Ref == ref {
		publication = attempt.CommitStatusPublication
	} else {
		// This is an observation of code already submitted before this run,
		// not a push. Keep that distinction durable across confirmation and
		// status reports so target rewinds remain available during validation.
		publication.AdoptedOnly = attempt == nil
		if err := sctx.DB.PutCommitStatusAttempt(sctx.Run.ID, db.CommitStatusAttempt{
			CommitStatusPublication: publication, Target: safeurl.Redact(target), Ref: ref,
		}); err != nil {
			return nil, err
		}
	}
	confirmed, err := sctx.DB.ConfirmCommitStatusAttempt(sctx.Run.ID, publication)
	if err != nil || !confirmed {
		return nil, err
	}
	return &publication, nil
}

// Only an owned attachment permits a status update when no push is needed.
func hasAttachedCommitStatusPublication(sctx *pipeline.StepContext, sha, target string) bool {
	if suppressPluginCommitStatus(sctx) || resolvedProvider(sctx) != scm.ProviderGitHub {
		return false
	}
	publication, err := sctx.DB.GetCommitStatusPublication(sctx.Ctx, sctx.Run.ID, db.CommitStatusPublication{
		SHA: sha, Host: resolvedHost(sctx, target), Repo: github.RepoSlug(target),
	})
	if err != nil {
		statusWarning(sctx.Log, "read attached publication", err)
	}
	return publication != nil
}

func recordCommitStatusAttempt(sctx *pipeline.StepContext, sha, target, ref string, validated, ciEligible bool) error {
	if suppressPluginCommitStatus(sctx) || resolvedProvider(sctx) != scm.ProviderGitHub || scm.ExtractHost(target) == "" {
		return nil
	}
	p := db.CommitStatusAttempt{CommitStatusPublication: db.CommitStatusPublication{SHA: sha, Host: resolvedHost(sctx, target), Repo: github.RepoSlug(target), Validated: validated, CIEligible: ciEligible}, Target: safeurl.Redact(target), Ref: ref}
	if p.Repo == "" {
		return nil
	}
	return sctx.DB.PutCommitStatusAttempt(sctx.Run.ID, p)
}

// reconcileCommitStatusAttempt promotes only an attempted SHA observed at the
// recorded target ref. It never infers publication from the mutable run head.
func reconcileCommitStatusAttempt(sctx *pipeline.StepContext) (*db.CommitStatusPublication, error) {
	ctx, cancel := context.WithTimeout(sctx.Ctx, commitStatusOperationTimeout)
	defer cancel()
	if err := acquireCommitStatusLock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-commitStatusLock }()
	bounded := *sctx
	bounded.Ctx = ctx
	attempt, err := sctx.DB.GetCommitStatusAttempt(sctx.Run.ID)
	if err != nil || attempt == nil {
		return nil, err
	}
	target := attempt.Target
	if sctx.Repo != nil {
		if current := resolvePushURL(sctx); safeurl.Redact(current) == target {
			target = current
		}
	}
	remote, err := commitStatusRemoteSHA(&bounded, target, attempt.Ref)
	if err != nil {
		return nil, err
	}
	if remote != attempt.SHA {
		return nil, nil
	}
	confirmed, err := sctx.DB.ConfirmCommitStatusAttempt(sctx.Run.ID, attempt.CommitStatusPublication)
	if err != nil || !confirmed {
		return nil, err
	}
	return &attempt.CommitStatusPublication, nil
}

// Use the same bounded, process-group-aware remote check for attachment and
// crash reconciliation, including Git credential and forge-profile settings.
func commitStatusRemoteSHA(sctx *pipeline.StepContext, target, ref string) (string, error) {
	return lsRemoteSHA(func(args ...string) (string, error) {
		cmd := stepCmd(sctx, "git", args...)
		cmd.Env = git.NonInteractiveEnvFrom(cmd.Env, sctx.WorkDir)
		shellenv.ConfigureShellCommand(cmd)
		out, err := shellenv.OutputShellCommand(cmd)
		return strings.TrimSpace(string(out)), err
	}, target, ref)
}

func beginPublishedCommitStatus(sctx *pipeline.StepContext) func(string) {
	if suppressPluginCommitStatus(sctx) {
		return func(string) {}
	}
	publication, err := reconcileCommitStatusAttempt(sctx)
	if err != nil {
		statusWarning(sctx.Log, "confirm pushed head", err)
	}
	if publication == nil {
		return func(string) {}
	}
	report := commitStatusReporter(sctx, publication)
	update := func(state string) { report(sctx.Ctx, state, "", sctx.Log) }
	update("pending")
	return update
}

func confirmVerifiedCommitStatus(sctx *pipeline.StepContext, sha, target, ref string) (*db.CommitStatusPublication, error) {
	if suppressPluginCommitStatus(sctx) || resolvedProvider(sctx) != scm.ProviderGitHub {
		return nil, nil
	}
	attempt, err := sctx.DB.GetCommitStatusAttempt(sctx.Run.ID)
	if err != nil || attempt == nil {
		return nil, err
	}
	if attempt.SHA != sha || attempt.Target != safeurl.Redact(target) || attempt.Ref != ref {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(sctx.Ctx, commitStatusOperationTimeout)
	defer cancel()
	if err := acquireCommitStatusLock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-commitStatusLock }()
	confirmed, err := sctx.DB.ConfirmCommitStatusAttempt(sctx.Run.ID, attempt.CommitStatusPublication)
	if err != nil || !confirmed {
		return nil, err
	}
	return &attempt.CommitStatusPublication, nil
}

func statusWarning(log func(string), operation string, err error) {
	message := safeurl.RedactText(fmt.Sprintf("warning: no-mistakes commit status %s: %v", operation, err))
	slog.Warn(message)
	if log != nil {
		log(message)
	}
}

func commitStatusReporter(sctx *pipeline.StepContext, publication *db.CommitStatusPublication) func(context.Context, string, string, func(string)) {
	host := github.New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return stepCmdContext(sctx, ctx, name, args...)
	}, nil, publication.Host, publication.Repo)
	targetURL := "https://" + publication.Host + "/" + publication.Repo + "/commit/" + publication.SHA
	sha := publication.SHA
	statusHost, statusRepo := publication.Host, publication.Repo
	report := func(ctx context.Context, state, validatedSHA string, log func(string)) {
		if suppressPluginCommitStatus(sctx, statusHost) {
			return
		}
		bounded, cancel := context.WithTimeout(ctx, commitStatusOperationTimeout)
		defer cancel()
		if err := acquireCommitStatusLock(bounded); err != nil {
			statusWarning(log, "wait for reporting", err)
			return
		}
		defer func() { <-commitStatusLock }()
		current, err := sctx.DB.GetCommitStatusPublication(bounded, sctx.Run.ID, db.CommitStatusPublication{SHA: sha, Host: statusHost, Repo: statusRepo})
		if err != nil {
			statusWarning(log, "read publication ownership", err)
			return
		}
		if current == nil {
			return
		}
		publication := current
		if state == "success" && publication.CIEligible && validatedSHA == publication.SHA && !publication.Validated {
			publication.Validated = true
			if err := sctx.DB.PutCommitStatusPublication(sctx.Run.ID, *publication); err != nil {
				statusWarning(log, "record validated head", err)
				return
			}
		}
		if state == "success" && !publication.Validated {
			if sctx.Run.Status != types.RunCompleted {
				return
			}
			state = "failure"
		}
		if state == publication.LastState {
			return
		}
		description := "no-mistakes pipeline running"
		switch state {
		case "success":
			description = "no-mistakes validation passed"
		case "failure":
			description = "no-mistakes pipeline failed or cancelled"
		}
		if err := host.PostCommitStatus(bounded, sha, state, description, targetURL); err != nil {
			statusWarning(log, state+" for "+sha, err)
			return
		}
		publication.LastState = state
		if err := sctx.DB.PutCommitStatusPublication(sctx.Run.ID, *publication); err != nil {
			statusWarning(log, "record posted state", err)
		}
	}
	return report
}

func (*PushStep) ReportPublishedHeadStatus(sctx *pipeline.StepContext, state, validatedSHA string) {
	if suppressPluginCommitStatus(sctx) {
		return
	}
	confirmed, err := reconcileCommitStatusAttempt(sctx)
	if err != nil {
		statusWarning(sctx.Log, "reconcile push attempt", err)
	}
	if confirmed != nil {
		commitStatusReporter(sctx, confirmed)(sctx.Ctx, "pending", "", sctx.Log)
	}
	publications, err := sctx.DB.GetCommitStatusPublications(sctx.Ctx, sctx.Run.ID)
	if err != nil {
		statusWarning(sctx.Log, "read pushed heads", err)
		return
	}
	for _, publication := range publications {
		commitStatusReporter(sctx, &publication)(sctx.Ctx, state, validatedSHA, sctx.Log)
	}
}

// A skipped or overridden local gate cannot claim that all local validation
// passed. CI repairs published without revalidation therefore stay pending
// until exact-head CI readiness proves completion.
func localValidationPassed(sctx *pipeline.StepContext, head string) bool {
	if head != "" {
		run, err := sctx.DB.GetRun(sctx.Run.ID)
		if err != nil || run == nil || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != head {
			return false
		}
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return false
	}
	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
		passed := false
		for _, step := range steps {
			if step.StepName == name && step.Status == types.StepStatusCompleted && step.OverrideReason == nil && step.TestOverrideReason() == "" {
				if name == types.StepTest && head != "" {
					if step.FindingsJSON == nil {
						return false
					}
					findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
					if err != nil || findings.TestedHeadSHA != head {
						return false
					}
				}
				passed = true
				break
			}
		}
		if !passed {
			return false
		}
	}
	return true
}
