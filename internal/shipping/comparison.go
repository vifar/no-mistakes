// Package shipping owns strict shipping comparison and receipt validation.
package shipping

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func Digest(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

type remoteObservationError struct{ cause error }

func (*remoteObservationError) Error() string {
	return "observe shipping ref: remote observation failed"
}

func (e *remoteObservationError) Unwrap() error { return e.cause }

func RemoteHead(ctx context.Context, dir, remote, branch string) (string, error) {
	out, err := git.Run(ctx, dir, "ls-remote", "--exit-code", remote, "refs/heads/"+branch)
	if err != nil {
		return "", &remoteObservationError{cause: err}
	}
	rows := strings.Fields(out)
	if len(rows) != 2 || rows[1] != "refs/heads/"+branch || len(rows[0]) != 40 {
		return "", fmt.Errorf("observe shipping ref: ambiguous ref")
	}
	return rows[0], nil
}

func Compare(ctx context.Context, dir, targetSHA, head string) (string, string, error) {
	base, err := git.Run(ctx, dir, "merge-base", targetSHA, head)
	if err != nil {
		return "", "", fmt.Errorf("resolve shipping merge base: %w", err)
	}
	raw, err := git.RunRaw(ctx, dir, "diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", targetSHA+"..."+head)
	if err != nil {
		return "", "", fmt.Errorf("resolve shipping diff: %w", err)
	}
	return base, Digest([]byte(raw)), nil
}

func Capture(ctx context.Context, database *db.DB, repo *db.Repo, run *db.Run, dir, target string) (*db.ReviewComparison, error) {
	upstreamURL := git.ResolveUpstreamURL(ctx, dir, repo.UpstreamURL, repo.URLsVerified)
	tip, err := RemoteHead(ctx, dir, upstreamURL, target)
	if err != nil {
		return nil, err
	}
	if _, err := git.Run(ctx, dir, "fetch", "--no-tags", "--no-write-fetch-head", upstreamURL, tip); err != nil {
		return nil, fmt.Errorf("fetch shipping comparison objects: %w", err)
	}
	head, err := git.HeadSHA(ctx, dir)
	if err != nil {
		return nil, err
	}
	base, digest, err := Compare(ctx, dir, tip, head)
	if err != nil {
		return nil, err
	}
	if after, err := RemoteHead(ctx, dir, upstreamURL, target); err != nil || after != tip {
		return nil, fmt.Errorf("shipping target moved during review capture")
	}
	if run.Shipping.Mode == types.ShippingImmutableSource {
		if err := CheckImmutable(ctx, run, repo, dir); err != nil {
			return nil, err
		}
	}
	return database.CaptureReviewComparison(run.ID, db.ReviewComparison{SourceRepo: github.RepoSlug(repo.PushURL()), SourceBranch: strings.TrimPrefix(run.Branch, "refs/heads/"), TargetBranch: target, TargetSHA: tip, HeadSHA: head, MergeBaseSHA: base, DiffDigest: digest})
}

func CheckImmutableSource(ctx context.Context, run *db.Run, repo *db.Repo, dir string) error {
	if run == nil || run.Shipping == nil || run.Shipping.Mode != types.ShippingImmutableSource {
		return nil
	}
	if run.SubmittedHeadSHA == nil {
		return fmt.Errorf("immutable source lacks submitted head")
	}
	head, err := git.HeadSHA(ctx, dir)
	if err != nil {
		return err
	}
	status, err := git.Run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	if head != *run.SubmittedHeadSHA || strings.TrimSpace(status) != "" {
		return fmt.Errorf("immutable source changed; source repairs and publication are refused")
	}
	pushURL := git.ResolvePushURL(ctx, dir, repo.UpstreamURL, repo.ForkURL, repo.URLsVerified)
	remote, err := RemoteHead(ctx, dir, pushURL, strings.TrimPrefix(run.Branch, "refs/heads/"))
	if err != nil {
		return err
	}
	if remote != head {
		return fmt.Errorf("immutable published source moved; start a new run")
	}
	return nil
}

func CheckImmutable(ctx context.Context, run *db.Run, repo *db.Repo, dir string) error {
	if err := CheckImmutableSource(ctx, run, repo, dir); err != nil {
		return err
	}
	return CheckImmutableTarget(ctx, run, repo, dir, RemoteHead)
}

func CheckImmutableTarget(ctx context.Context, run *db.Run, repo *db.Repo, dir string, remoteHead func(context.Context, string, string, string) (string, error)) error {
	if run == nil || run.Shipping == nil || run.Shipping.Mode != types.ShippingImmutableSource {
		return nil
	}
	if run.Shipping.TargetSHA != "" {
		upstreamURL := git.ResolveUpstreamURL(ctx, dir, repo.UpstreamURL, repo.URLsVerified)
		target, err := remoteHead(ctx, dir, upstreamURL, run.Shipping.TargetBranch)
		if err != nil {
			return err
		}
		if target != run.Shipping.TargetSHA {
			return fmt.Errorf("immutable target moved; start a new run")
		}
	}
	return nil
}
