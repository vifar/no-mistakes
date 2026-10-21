package pipeline

import "github.com/kunchenguid/no-mistakes/internal/git"

func (sctx *StepContext) CommitAttributionEnv(env []string, run func(...string) (string, error), imported ...string) ([]string, error) {
	submitted := ""
	if sctx.Run != nil {
		if sctx.Run.SubmittedHeadSHA != nil {
			submitted = *sctx.Run.SubmittedHeadSHA
		}
	}
	var configured []string
	if sctx.Config != nil {
		configured = sctx.Config.Commit.Trailers
	}
	return git.AttributionHookEnv(sctx.WorkDir, env, run, submitted, imported, configured...)
}

func (sctx *StepContext) ValidateCommitAttribution(before, after string, run func(...string) (string, error)) error {
	submitted := ""
	if sctx.Run != nil {
		if sctx.Run.SubmittedHeadSHA != nil {
			submitted = *sctx.Run.SubmittedHeadSHA
		}
	}
	var configured []string
	if sctx.Config != nil {
		configured = sctx.Config.Commit.Trailers
	}
	return git.ValidateCommitAttribution(before, after, run, submitted, configured...)
}
