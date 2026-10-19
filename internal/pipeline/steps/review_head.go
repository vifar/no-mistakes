package steps

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// ReviewHeadPreconditionError is an execution-state mismatch, not an analyzer
// formatting failure. No analyzer attempt can repair it.
type ReviewHeadPreconditionError struct {
	RunHead, PinnedHead, LocalHead string
	Reason                         string
}

func (e *ReviewHeadPreconditionError) Error() string {
	return fmt.Sprintf("review_head_precondition: run head %s, pinned PR comparison head %s, local head %s: %s", e.RunHead, e.PinnedHead, e.LocalHead, e.Reason)
}

func ensureReviewComparisonHead(c *pipeline.StepContext) error {
	if c.PRContext == nil {
		return nil
	}
	pinned := c.PRContext.LocalHeadSHA
	local, err := git.HeadSHA(c.Ctx, c.WorkDir)
	failure := &ReviewHeadPreconditionError{PinnedHead: pinned, LocalHead: local}
	if c.Run == nil {
		failure.Reason = "run is missing"
		return failure
	}
	failure.RunHead = c.Run.HeadSHA
	if err != nil || local == "" {
		failure.Reason = fmt.Sprintf("cannot verify local head: %v", err)
		return failure
	}
	if c.Run.HeadSHA == local && local == pinned {
		return nil
	}
	if c.Fixing && c.Run.HeadSHA == local && pinned != "" {
		if _, err := git.Run(c.Ctx, c.WorkDir, "merge-base", "--is-ancestor", pinned, local); err == nil {
			return nil
		}
	}
	if !c.Fixing && !c.FinalizingAnswers && local == pinned && c.DB != nil {
		current, err := c.DB.GetRun(c.Run.ID)
		if err == nil && current != nil && current.HeadSHA == pinned {
			c.Run.HeadSHA = pinned
			return nil
		}
	}
	failure.Reason = "review requires matching Git, durable run and pinned comparison; an owned fix must descend from the pin"
	return failure
}
