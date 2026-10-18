package db

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
)

// ReviewComparison is captured before the independent review turn and granted
// atomically with successful Review completion, never by a receipt exporter.
type ReviewComparison struct {
	SourceRepo   string
	SourceBranch string
	TargetBranch string
	TargetSHA    string
	HeadSHA      string
	MergeBaseSHA string
	DiffDigest   string
	Generation   int64
	Approved     bool
}

var fullShippingSHA = regexp.MustCompile(`^[a-f0-9]{40}$`)
var shippingDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (d *DB) CaptureReviewComparison(runID string, c ReviewComparison) (*ReviewComparison, error) {
	if c.SourceRepo == "" || c.SourceBranch == "" || c.TargetBranch == "" || !fullShippingSHA.MatchString(c.TargetSHA) || !fullShippingSHA.MatchString(c.HeadSHA) || !fullShippingSHA.MatchString(c.MergeBaseSHA) || !shippingDigest.MatchString(c.DiffDigest) {
		return nil, fmt.Errorf("invalid review comparison")
	}
	_, err := sqlitewrite.Exec(context.Background(), d.sql, "CaptureReviewComparison", `INSERT INTO review_comparisons(run_id,source_repo,source_branch,target_branch,target_sha,head_sha,merge_base_sha,diff_digest,generation,approved) VALUES(?,?,?,?,?,?,?,?,1,0) ON CONFLICT(run_id) DO UPDATE SET source_repo=excluded.source_repo,source_branch=excluded.source_branch,target_branch=excluded.target_branch,target_sha=excluded.target_sha,head_sha=excluded.head_sha,merge_base_sha=excluded.merge_base_sha,diff_digest=excluded.diff_digest,generation=review_comparisons.generation+1,approved=0`, runID, c.SourceRepo, c.SourceBranch, c.TargetBranch, c.TargetSHA, c.HeadSHA, c.MergeBaseSHA, c.DiffDigest)
	if err != nil {
		return nil, fmt.Errorf("capture review comparison: %w", err)
	}
	if _, err := sqlitewrite.Exec(context.Background(), d.sql, "CaptureReviewComparison", `UPDATE runs SET ci_ready_at=NULL,ci_ready_no_ci=0 WHERE id=? AND shipping_options IS NOT NULL`, runID); err != nil {
		return nil, err
	}
	return d.GetReviewComparison(runID)
}
func (d *DB) GetReviewComparison(runID string) (*ReviewComparison, error) {
	c := &ReviewComparison{}
	err := d.sql.QueryRow(`SELECT source_repo,source_branch,target_branch,target_sha,head_sha,merge_base_sha,diff_digest,generation,approved FROM review_comparisons WHERE run_id=?`, runID).Scan(&c.SourceRepo, &c.SourceBranch, &c.TargetBranch, &c.TargetSHA, &c.HeadSHA, &c.MergeBaseSHA, &c.DiffDigest, &c.Generation, &c.Approved)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read review comparison: %w", err)
	}
	return c, nil
}
