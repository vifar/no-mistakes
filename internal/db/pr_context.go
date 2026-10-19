package db

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PRContextCandidate is the exact attached PR identity and comparison observed
// by a caller. The PR URL and forge head may be empty while discovery supplies
// an existing PR's target without binding its identity to the run.
type PRContextCandidate struct {
	PRURL        string
	SourceRepo   string
	SourceBranch string
	ForgeHeadSHA string
	LocalHeadSHA string
	TargetBranch string
	TargetSHA    string
	MergeBaseSHA string
	DiffDigest   string
}

type PRContext struct {
	PRContextCandidate
	ObservedAt int64
	Generation int64
	// CIQualified survives transient readiness failures only for this exact
	// comparison. Rebinding a changed comparison clears it.
	CIQualified bool
}

type PRContextBindResult struct {
	Changed    bool
	Generation int64
}

// GetRunPRContext returns nil for an unbound run. No legacy run fields are
// interpreted as a receipt.
func (d *DB) GetRunPRContext(runID string) (*PRContext, error) {
	context := &PRContext{}
	var prURL, sourceRepo, sourceBranch, forgeHead sql.NullString
	err := d.sql.QueryRow(`SELECT pr_url, source_repo, source_branch, forge_head_sha,
		local_head_sha, target_branch, target_sha, merge_base_sha, diff_digest,
		observed_at, generation, ci_qualified FROM run_pr_contexts WHERE run_id = ?`, runID).Scan(
		&prURL, &sourceRepo, &sourceBranch, &forgeHead,
		&context.LocalHeadSHA, &context.TargetBranch, &context.TargetSHA,
		&context.MergeBaseSHA, &context.DiffDigest, &context.ObservedAt, &context.Generation, &context.CIQualified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run PR context: %w", err)
	}
	context.PRURL = prURL.String
	context.SourceRepo = sourceRepo.String
	context.SourceBranch = sourceBranch.String
	context.ForgeHeadSHA = forgeHead.String
	return context, nil
}

// BindRunPRContext records an exact observed comparison. Repeating identical
// content has no side effect. A changed comparison invalidates review authority
// and step results in the same transaction. Attaching the first PR identity to
// an identical prospective comparison preserves completed steps. Rebase is the
// boundary when the target moved; Review is the boundary after Rebase itself
// changed local HEAD.
func (d *DB) BindRunPRContext(runID string, candidate PRContextCandidate, resetFrom types.StepName) (PRContextBindResult, error) {
	return d.bindRunPRContext(runID, candidate, resetFrom, false, nil)
}

// AdvanceRunPRContext records a commit made by the current forward pipeline
// step. Earlier completed steps keep their own evidence and review anchor.
func (d *DB) AdvanceRunPRContext(runID string, candidate PRContextCandidate) (PRContextBindResult, error) {
	return d.bindRunPRContext(runID, candidate, types.StepReview, true, nil)
}

func (d *DB) bindRunPRContext(runID string, candidate PRContextCandidate, resetFrom types.StepName, forward bool, options *prContextBindOptions) (PRContextBindResult, error) {
	const attempts = 4
	const initialBackoff = 25 * time.Millisecond
	for attempt := 0; ; attempt++ {
		result, err := d.bindRunPRContextOnce(runID, candidate, resetFrom, forward, options)
		var sqliteErr *sqlite.Error
		// Include extended codes such as SQLITE_BUSY_SNAPSHOT (517). Its
		// snapshot cannot be upgraded even after busy_timeout: release the
		// transaction and re-read the receipt, generation and authority checks.
		// SQLITE_LOCKED and non-driver errors are not this recovery contract.
		if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY {
			return result, err
		}
		if attempt+1 == attempts {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context after %d attempts: %w", attempts, err)
		}
		// Once has already rolled back before waiting, so the backoff holds
		// neither the stale snapshot nor this DB's sole connection.
		time.Sleep(initialBackoff << attempt)
	}
}

func (d *DB) bindRunPRContextOnce(runID string, candidate PRContextCandidate, resetFrom types.StepName, forward bool, options *prContextBindOptions) (PRContextBindResult, error) {
	if err := validatePRContextCandidate(candidate); err != nil {
		return PRContextBindResult{}, err
	}
	if resetFrom != types.StepRebase && resetFrom != types.StepReview {
		return PRContextBindResult{}, fmt.Errorf("bind run PR context: reset boundary must be rebase or review")
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return PRContextBindResult{}, fmt.Errorf("bind run PR context: begin: %w", err)
	}
	defer tx.Rollback()
	var recordedPRURL sql.NullString
	if err := tx.QueryRow(`SELECT pr_url FROM runs WHERE id = ?`, runID).Scan(&recordedPRURL); err != nil {
		return PRContextBindResult{}, fmt.Errorf("bind run PR context: run lookup: %w", err)
	}
	if recordedPRURL.Valid && recordedPRURL.String != "" && recordedPRURL.String != candidate.PRURL {
		return PRContextBindResult{}, fmt.Errorf("bind run PR context: PR identity conflicts with run")
	}
	previous := &PRContext{}
	var prURL, sourceRepo, sourceBranch, forgeHead sql.NullString
	err = tx.QueryRow(`SELECT pr_url, source_repo, source_branch, forge_head_sha,
		local_head_sha, target_branch, target_sha, merge_base_sha, diff_digest,
		observed_at, generation FROM run_pr_contexts WHERE run_id = ?`, runID).Scan(
		&prURL, &sourceRepo, &sourceBranch, &forgeHead,
		&previous.LocalHeadSHA, &previous.TargetBranch, &previous.TargetSHA,
		&previous.MergeBaseSHA, &previous.DiffDigest, &previous.ObservedAt, &previous.Generation)
	initial := errors.Is(err, sql.ErrNoRows)
	if err != nil && !initial {
		return PRContextBindResult{}, fmt.Errorf("bind run PR context: read prior receipt: %w", err)
	}
	if !initial {
		previous.PRURL = prURL.String
		previous.SourceRepo = sourceRepo.String
		previous.SourceBranch = sourceBranch.String
		previous.ForgeHeadSHA = forgeHead.String
		if previous.SourceRepo != "" && (candidate.SourceRepo != previous.SourceRepo || candidate.SourceBranch != previous.SourceBranch) {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context: source identity conflicts with receipt")
		}
		if previous.PRURL != "" && candidate.PRURL != previous.PRURL {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context: PR identity conflict")
		}
		if previous.PRContextCandidate == candidate && (options == nil || options.completeGeneration == nil) {

			if options != nil && options.rewind != nil {
				pending, err := pendingTargetRewind(tx, runID)
				if err != nil {
					return PRContextBindResult{}, err
				}
				if pending != nil && (!sameOptionalSHA(pending.SourceSHA, options.rewind.SourceSHA) ||
					!sameOptionalSHA(pending.ForgeHeadSHA, prHeadObservation(candidate)) || options.rewind.HasPR != (candidate.PRURL != "")) {
					return PRContextBindResult{}, fmt.Errorf("remote source or PR observation changed during pending rewind")
				}
			}
			return PRContextBindResult{Generation: previous.Generation}, nil
		}
		if previous.Generation == math.MaxInt64 && previous.PRContextCandidate != candidate {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context: generation exhausted")
		}
	}

	var rewindRun *Run
	var pending *TargetAdvanceRewind
	if options != nil && options.rewind != nil {
		if initial {
			return PRContextBindResult{}, ErrTargetAdvanceIneligible
		}
		rewindRun, pending, err = checkTargetRewindEligibility(tx, runID, previous, candidate, options.rewind)
		if err != nil {
			return PRContextBindResult{}, err
		}
	}
	if options != nil && options.completeGeneration != nil {
		pending, err = pendingTargetRewind(tx, runID)
		if err != nil {
			return PRContextBindResult{}, err
		}
		var head string
		if err = tx.QueryRow(`SELECT head_sha FROM runs WHERE id=?`, runID).Scan(&head); err != nil {
			return PRContextBindResult{}, err
		}
		if initial || previous.Generation != *options.completeGeneration || pending == nil ||
			pending.TargetSHA != candidate.TargetSHA || head != candidate.LocalHeadSHA ||
			!sameOptionalSHA(pending.ForgeHeadSHA, prHeadObservation(candidate)) ||
			previous.TargetBranch != candidate.TargetBranch || previous.PRURL != candidate.PRURL ||
			previous.SourceRepo != candidate.SourceRepo || previous.SourceBranch != candidate.SourceBranch || previous.ForgeHeadSHA != candidate.ForgeHeadSHA {
			return PRContextBindResult{}, fmt.Errorf("pending Rebase completion differs from classified comparison")
		}
		if _, err = tx.Exec(`UPDATE runs SET pending_target_rewind_id=NULL WHERE id=?`, runID); err != nil {
			return PRContextBindResult{}, err
		}
		if previous.PRContextCandidate == candidate {
			if err = tx.Commit(); err != nil {
				return PRContextBindResult{}, err
			}
			return PRContextBindResult{Generation: previous.Generation}, nil
		}
	}
	identityOnlyAttach := !initial && previous.PRURL == "" && candidate.PRURL != "" &&
		candidate.ForgeHeadSHA == candidate.LocalHeadSHA &&
		previous.SourceRepo == candidate.SourceRepo && previous.SourceBranch == candidate.SourceBranch &&
		previous.LocalHeadSHA == candidate.LocalHeadSHA &&
		previous.TargetBranch == candidate.TargetBranch &&
		previous.TargetSHA == candidate.TargetSHA &&
		previous.MergeBaseSHA == candidate.MergeBaseSHA &&
		previous.DiffDigest == candidate.DiffDigest
	generation := int64(1)
	if !initial {
		generation = previous.Generation + 1
	}

	if rewindRun != nil {
		req := options.rewind
		source, forgeHead := req.SourceSHA, prHeadObservation(candidate)
		if pending != nil {
			source, forgeHead = pending.SourceSHA, pending.ForgeHeadSHA
		}
		id := newID()
		if _, err = tx.Exec(`INSERT INTO run_target_rewinds (id,run_id,old_target_sha,target_sha,local_head_sha,source_sha,forge_head_sha,step,generation,at)
            VALUES (?,?,?,?,?,?,?,?,?,?)`, id, runID, previous.TargetSHA, candidate.TargetSHA, candidate.LocalHeadSHA, source, forgeHead, req.Step, generation, now()); err != nil {
			return PRContextBindResult{}, err
		}
		if err = archiveTargetRewind(tx, id, runID, previous, rewindRun.ReviewApprovedHeadSHA); err != nil {
			return PRContextBindResult{}, err
		}
		if _, err = tx.Exec(`UPDATE runs SET target_advance_rewinds=target_advance_rewinds+1,pending_target_rewind_id=? WHERE id=?`, id, runID); err != nil {
			return PRContextBindResult{}, err
		}
	}
	ts := now()
	if _, err := tx.Exec(`INSERT INTO run_pr_contexts (
		run_id, pr_url, source_repo, source_branch, forge_head_sha,
		local_head_sha, target_branch, target_sha, merge_base_sha, diff_digest,
		observed_at, generation) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
		pr_url=excluded.pr_url, source_repo=excluded.source_repo,
		source_branch=excluded.source_branch, forge_head_sha=excluded.forge_head_sha,
		local_head_sha=excluded.local_head_sha, target_branch=excluded.target_branch,
		target_sha=excluded.target_sha, merge_base_sha=excluded.merge_base_sha,
		diff_digest=excluded.diff_digest, observed_at=excluded.observed_at,
		generation=excluded.generation, ci_qualified=0, retained_target_sha=NULL`,
		runID, optionalPRField(candidate.PRURL), optionalPRField(candidate.SourceRepo),
		optionalPRField(candidate.SourceBranch), optionalPRField(candidate.ForgeHeadSHA),
		candidate.LocalHeadSHA, candidate.TargetBranch, candidate.TargetSHA,
		candidate.MergeBaseSHA, candidate.DiffDigest, ts, generation); err != nil {
		return PRContextBindResult{}, fmt.Errorf("bind run PR context: write receipt: %w", err)
	}
	if candidate.PRURL != "" && recordedPRURL.String == "" {
		if _, err := tx.Exec(`UPDATE runs SET pr_url=?, pr_state='open', pr_state_observed_at=?, updated_at=? WHERE id=? AND (pr_url IS NULL OR pr_url='')`, candidate.PRURL, ts, ts, runID); err != nil {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context: attach discovered PR: %w", err)
		}
	}
	if !initial && !identityOnlyAttach && forward {
		if _, err := tx.Exec(`UPDATE runs SET ci_ready_at=NULL, ci_ready_no_ci=0,
			terminal_head_verified_at=NULL, updated_at=? WHERE id=?`, ts, runID); err != nil {
			return PRContextBindResult{}, fmt.Errorf("advance run PR context: revoke terminal evidence: %w", err)
		}
	}
	if !initial && !identityOnlyAttach && !forward {
		if rewindRun != nil {
			if _, err := tx.Exec(`UPDATE review_comparisons SET approved=0 WHERE run_id=?`, runID); err != nil {
				return PRContextBindResult{}, err
			}
		}
		if _, err := tx.Exec(`DELETE FROM step_rounds WHERE step_result_id IN (
			SELECT id FROM step_results WHERE run_id=? AND step_order>=?)`, runID, resetFrom.Order()); err != nil {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context: discard superseded rounds: %w", err)
		}
		if _, err := tx.Exec(`UPDATE runs SET review_approved_head_sha=NULL,
			ci_ready_at=NULL, ci_ready_no_ci=0, terminal_head_verified_at=NULL,
			awaiting_agent_since=NULL, updated_at=? WHERE id=?`, ts, runID); err != nil {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context: revoke run authority: %w", err)
		}
		// A skipped core step is a pinned execution choice, not validation
		// credit. Keep that choice durable if this rewind later parks, while
		// discarding all its evidence and rerunning every custom gate.
		preserveSkips := options != nil && (options.rewind != nil || options.completeGeneration != nil)
		if _, err := tx.Exec(`UPDATE step_results SET status=CASE WHEN ? AND status='skipped'
            AND step_name IN ('review','test','document','lint','push','pr','ci') THEN 'skipped' ELSE ? END, exit_code=NULL,
			duration_ms=NULL, log_path=NULL, findings_json=NULL, error=NULL,
			started_at=NULL, round_started_at=NULL, completed_at=NULL,
			last_activity_at=NULL, last_activity=NULL, agent_pid=NULL,
			auto_fix_limit=NULL, ci_fix_attempts=0, override_reason=NULL,
			approval_reason=NULL, accepted_gaps_json=NULL, skip_reason=NULL,
			pending_response=CASE WHEN ? THEN NULL ELSE pending_response END,
			gate_nonce=CASE WHEN ? THEN NULL ELSE gate_nonce END,
			gate_round_id=CASE WHEN ? THEN NULL ELSE gate_round_id END
			WHERE run_id=? AND step_order>=?`, preserveSkips, types.StepStatusPending, rewindRun != nil, rewindRun != nil, rewindRun != nil, runID, resetFrom.Order()); err != nil {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context: reset dependent steps: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return PRContextBindResult{}, fmt.Errorf("bind run PR context: commit: %w", err)
	}
	return PRContextBindResult{Changed: true, Generation: generation}, nil
}

func optionalPRField(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func validatePRContextCandidate(c PRContextCandidate) error {
	for name, value := range map[string]string{
		"local head SHA": c.LocalHeadSHA, "target branch": c.TargetBranch,
		"target SHA": c.TargetSHA, "merge-base SHA": c.MergeBaseSHA,
		"diff digest": c.DiffDigest,
	} {
		if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\t") {
			return fmt.Errorf("bind run PR context: invalid %s", name)
		}
	}
	if !validGitSHA(c.LocalHeadSHA) || !validGitSHA(c.TargetSHA) || !validGitSHA(c.MergeBaseSHA) || !validHex(c.DiffDigest, 64) {
		return fmt.Errorf("bind run PR context: invalid SHA or digest")
	}
	if (c.SourceRepo == "") != (c.SourceBranch == "") ||
		(c.SourceRepo != "" && (c.SourceRepo != strings.TrimSpace(c.SourceRepo) || strings.ContainsAny(c.SourceRepo, "\r\n\t") || c.SourceBranch != strings.TrimSpace(c.SourceBranch) || strings.ContainsAny(c.SourceBranch, "\r\n\t"))) {
		return fmt.Errorf("bind run PR context: incomplete source identity")
	}
	if c.PRURL == "" {
		if c.ForgeHeadSHA != "" {
			return fmt.Errorf("bind run PR context: incomplete PR identity")
		}
		return nil
	}
	u, err := url.Parse(c.PRURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" || c.PRURL != strings.TrimSpace(c.PRURL) {
		return fmt.Errorf("bind run PR context: invalid PR URL")
	}
	if c.SourceRepo == "" || !validGitSHA(c.ForgeHeadSHA) {
		return fmt.Errorf("bind run PR context: incomplete or invalid PR identity")
	}
	return nil
}

func validGitSHA(s string) bool { return validHex(s, 40) || validHex(s, 64) }

func validHex(s string, length int) bool {
	if len(s) != length {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}
