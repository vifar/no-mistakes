package pipeline

import "github.com/kunchenguid/no-mistakes/internal/git"

func (sctx *StepContext) CommitAttributionEnv(env []string, run func(...string) (string, error)) ([]string, error) {
	submitted := ""
	if sctx.Run != nil {
		submitted = sctx.Run.HeadSHA
		if sctx.Run.SubmittedHeadSHA != nil {
			submitted = *sctx.Run.SubmittedHeadSHA
		}
	}
	var configured []string
	if sctx.Config != nil {
		configured = sctx.Config.Commit.Trailers
	}
	return git.AttributionHookEnv(sctx.WorkDir, env, run, submitted, configured...)
}

func (sctx *StepContext) ValidateCommitAttribution(before, after string, run func(...string) (string, error)) error {
	submitted := ""
	if sctx.Run != nil {
		submitted = sctx.Run.HeadSHA
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
