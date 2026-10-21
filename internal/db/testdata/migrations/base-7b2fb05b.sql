CREATE TABLE status_validation_epochs (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 epoch INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE status_validation_steps (
 step_id TEXT PRIMARY KEY REFERENCES step_results(id) ON DELETE CASCADE,
 head_sha TEXT NOT NULL, epoch INTEGER NOT NULL, config_digest TEXT NOT NULL
);
CREATE TABLE controlled_shutdown_runs (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 instance TEXT NOT NULL, lifecycle TEXT NOT NULL, reason TEXT NOT NULL
);
CREATE TABLE document_passes (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    from_head_sha TEXT NOT NULL,
    to_head_sha TEXT,
    commit_count INTEGER NOT NULL DEFAULT 0,
    findings_json TEXT,
    carried_review_from TEXT
);
CREATE UNIQUE INDEX document_pass_pending ON document_passes(run_id) WHERE to_head_sha IS NULL;
CREATE TABLE pipeline_commit_heads (
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    head_sha TEXT NOT NULL,
    parent_sha TEXT NOT NULL,
    PRIMARY KEY (run_id, head_sha)
);
CREATE TABLE review_comparisons (
    run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    source_repo TEXT NOT NULL, source_branch TEXT NOT NULL,
    target_branch TEXT NOT NULL, target_sha TEXT NOT NULL,
    head_sha TEXT NOT NULL, merge_base_sha TEXT NOT NULL,
    diff_digest TEXT NOT NULL, generation INTEGER NOT NULL,
    approved INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE repos (
    id             TEXT PRIMARY KEY,
    working_path   TEXT NOT NULL UNIQUE,
    upstream_url   TEXT NOT NULL,
    fork_url       TEXT,
    default_branch TEXT NOT NULL DEFAULT 'main',
    created_at     INTEGER NOT NULL
);
CREATE TABLE runs (
    id                   TEXT PRIMARY KEY,
    repo_id              TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch               TEXT NOT NULL,
    ending_event TEXT,
    head_sha                TEXT NOT NULL,
    base_sha                TEXT NOT NULL,
    worktree_dir            TEXT,
    submitted_head_sha      TEXT,
    no_mistakes_version     TEXT,
    no_mistakes_build_sha   TEXT,
    review_approved_head_sha TEXT,
    status                  TEXT NOT NULL DEFAULT 'pending',
    pr_url                  TEXT,
    pr_state                TEXT,
    pr_state_observed_at    INTEGER,
    ci_ready_at             INTEGER,
    ci_ready_no_ci          INTEGER NOT NULL DEFAULT 0,
    last_pushed_sha         TEXT,
    push_target_kind        TEXT,
    push_target_fingerprint TEXT,
    push_ref                TEXT,
    last_pushed_at          INTEGER,
    push_generation         INTEGER,
    push_active             INTEGER NOT NULL DEFAULT 0,
    terminal_head_verified_at INTEGER,
    gates_json              TEXT,
    error                   TEXT,
    awaiting_agent_since INTEGER,
    parked_ms            INTEGER,
    launch_nonce         TEXT,
    launch_validation_generation TEXT,
    launch_intent_digest TEXT,
    launch_receipt_claimed_at INTEGER,
    pr_base_branch       TEXT,
    pr_base_branch_requested INTEGER NOT NULL DEFAULT 0,
    omit_intent          INTEGER NOT NULL DEFAULT 0,
    external_ci_owner    TEXT NOT NULL DEFAULT '',
    pi_profile           TEXT,
    verification_plan    TEXT,
    leftover_validation_commits INTEGER NOT NULL DEFAULT 0,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
, shipping_options TEXT, intent TEXT, intent_source TEXT, intent_session_id TEXT, intent_score REAL, ci_rerun_state TEXT, custody_returned_at INTEGER, closing_issue_refs TEXT, closing_issue_refs_locked_at INTEGER, ci_test_restart_pending INTEGER NOT NULL DEFAULT 0, queued_at INTEGER, queue_spec TEXT, check_scope_json TEXT, target_advance_rewinds INTEGER NOT NULL DEFAULT 0, pending_target_rewind_id TEXT, retarget_attempted INTEGER, launch_owner TEXT);
CREATE TABLE commit_status_attempts (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 sha TEXT NOT NULL, host TEXT NOT NULL, repo TEXT NOT NULL,
 target TEXT NOT NULL, ref TEXT NOT NULL, retired INTEGER NOT NULL DEFAULT 0,
 validated INTEGER NOT NULL DEFAULT 0, ci_eligible INTEGER NOT NULL DEFAULT 0
, adopted_only INTEGER NOT NULL DEFAULT 0);
CREATE TABLE commit_status_publications (
 run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 sha TEXT NOT NULL, host TEXT NOT NULL, repo TEXT NOT NULL,
 last_state TEXT NOT NULL DEFAULT '', retired INTEGER NOT NULL DEFAULT 0, validated INTEGER NOT NULL DEFAULT 0, ci_eligible INTEGER NOT NULL DEFAULT 0, adopted_only INTEGER NOT NULL DEFAULT 0, strict_certificate TEXT, preserve_success INTEGER,
 PRIMARY KEY (host, repo, sha, run_id)
);
CREATE TABLE step_results (
    id               TEXT PRIMARY KEY,
    run_id           TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name        TEXT NOT NULL,
    step_order       INTEGER NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending',
    exit_code        INTEGER,
    duration_ms      INTEGER,
    log_path         TEXT,
    findings_json    TEXT,
    error            TEXT,
    started_at       INTEGER,
    round_started_at INTEGER,
    completed_at     INTEGER,
    last_activity_at INTEGER,
    last_activity    TEXT,
    agent_pid        INTEGER,
    auto_fix_limit              INTEGER,
    ci_fix_attempts             INTEGER NOT NULL DEFAULT 0,
    override_reason             TEXT
, skip_reason TEXT, approval_reason TEXT, accepted_gaps_json TEXT, pending_response TEXT, gate_nonce TEXT, gate_round_id TEXT);
CREATE TABLE run_pr_contexts (
    run_id          TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    pr_url          TEXT,
    source_repo     TEXT,
    source_branch   TEXT,
    forge_head_sha  TEXT,
    local_head_sha  TEXT NOT NULL,
    target_branch   TEXT NOT NULL,
    target_sha      TEXT NOT NULL,
    merge_base_sha  TEXT NOT NULL,
    diff_digest     TEXT NOT NULL,
    observed_at     INTEGER NOT NULL,
    generation      INTEGER NOT NULL CHECK (generation > 0)
, ci_qualified INTEGER NOT NULL DEFAULT 0, retained_target_sha TEXT);
CREATE TABLE step_rounds (
    id                   TEXT PRIMARY KEY,
    step_result_id       TEXT NOT NULL REFERENCES step_results(id) ON DELETE CASCADE,
    round                INTEGER NOT NULL,
    trigger_type         TEXT NOT NULL,
    findings_json        TEXT,
    reviewed_head_sha    TEXT,
    starting_head_sha    TEXT,
    trusted_config_sha   TEXT,
    global_config_yaml   BLOB,
    repo_config_yaml     BLOB,
    user_findings_json   TEXT,
    selected_finding_ids TEXT,
    selection_source     TEXT,
    fix_summary          TEXT,
    repair_published     INTEGER NOT NULL DEFAULT 0,
    duration_ms          INTEGER NOT NULL,
    created_at           INTEGER NOT NULL
);
CREATE INDEX idx_step_rounds_step_result_round ON step_rounds (step_result_id, round);
CREATE INDEX idx_step_results_run_order ON step_results (run_id, step_order, id);
CREATE TABLE agent_invocations (
    id                    TEXT PRIMARY KEY,
    run_id                TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_name             TEXT NOT NULL,
    round                 INTEGER NOT NULL,
    purpose               TEXT NOT NULL,
    agent                 TEXT NOT NULL,
    model                 TEXT,
    model_provider        TEXT,
    session_mode          TEXT NOT NULL,
    session_key           TEXT,
    fallback_reason       TEXT,
    started_at            INTEGER NOT NULL,
    completed_at          INTEGER NOT NULL,
    duration_ms           INTEGER NOT NULL,
    subprocess_wait_ms    INTEGER,
    exit_status           TEXT NOT NULL,
    failure_category      TEXT,
    input_tokens          INTEGER,
    output_tokens         INTEGER,
    cache_read_tokens     INTEGER,
    cache_creation_tokens INTEGER,
    fresh_input_tokens    INTEGER,
    reasoning_tokens      INTEGER,
    delta_input_tokens    INTEGER,
    delta_output_tokens   INTEGER,
    delta_cache_read_tokens INTEGER,
    model_roundtrips      INTEGER,
    tool_calls            INTEGER,
    tool_wait_calls       INTEGER,
    tool_test_lint_calls  INTEGER,
    tool_edit_calls       INTEGER,
    tool_read_calls       INTEGER,
    tool_git_calls        INTEGER,
    tool_other_calls      INTEGER,
    workload_files        INTEGER,
    workload_lines        INTEGER,
    finding_count         INTEGER
);
CREATE INDEX idx_agent_invocations_run_started_id
    ON agent_invocations (run_id, started_at, id);
CREATE TABLE recovery_archives (
    id                 TEXT PRIMARY KEY,
    owner_run_id       TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    repo_id            TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    run_id             TEXT NOT NULL,
    branch             TEXT NOT NULL,
    required_head_sha  TEXT NOT NULL,
    preserved_head_sha TEXT NOT NULL,
    archive_ref        TEXT NOT NULL,
    created_at         INTEGER NOT NULL,
    UNIQUE (owner_run_id, archive_ref)
);
CREATE INDEX idx_recovery_archives_owner_created_id
    ON recovery_archives (owner_run_id, created_at, id);
CREATE TABLE run_agent_sessions (
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    agent      TEXT NOT NULL,
    session_id TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (run_id, role)
);
CREATE TABLE run_media_attachments (
    run_id    TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    path      TEXT NOT NULL,
    digest    TEXT NOT NULL,
    url       TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (run_id, path)
);
CREATE TABLE intent_cache (
    cache_key   TEXT PRIMARY KEY,
    summary     TEXT NOT NULL,
    agent_name  TEXT NOT NULL,
    session_id  TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);
CREATE TABLE review_questions (
    repo_id      TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch       TEXT NOT NULL,
    question_id  TEXT NOT NULL,
    run_id       TEXT NOT NULL,
    ask_ordinal  INTEGER NOT NULL,
    question     TEXT NOT NULL,
    options_json TEXT,
    file         TEXT,
    line         INTEGER,
    answer       TEXT NOT NULL,
    answered_by  TEXT,
    answered_at  TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (repo_id, branch, question_id, run_id, ask_ordinal)
);
CREATE TABLE uncertified_pipeline_ranges (
    repo_id       TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    branch        TEXT NOT NULL,
    from_sha      TEXT NOT NULL,
    to_sha        TEXT NOT NULL,
    source_run_id TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (repo_id, branch)
);
CREATE TABLE owner_admission_events (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 payload TEXT NOT NULL
);
CREATE TABLE push_notifications (
 notification_id TEXT PRIMARY KEY,
 run_id TEXT REFERENCES runs(id),
 diagnostic_run_id TEXT REFERENCES runs(id)
);
CREATE TRIGGER runs_verification_plan_immutable BEFORE UPDATE OF verification_plan ON runs WHEN NEW.verification_plan IS NOT OLD.verification_plan BEGIN SELECT RAISE(ABORT, 'run verification plan is immutable'); END;
CREATE TRIGGER runs_pi_profile_immutable BEFORE UPDATE OF pi_profile ON runs WHEN NEW.pi_profile IS NOT OLD.pi_profile BEGIN SELECT RAISE(ABORT, 'run Pi profile is immutable'); END;
CREATE UNIQUE INDEX idx_runs_repo_branch_launch_nonce ON runs (repo_id, branch, launch_nonce) WHERE launch_nonce IS NOT NULL;
CREATE TABLE run_target_rewinds (
        id TEXT PRIMARY KEY,
        run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
        old_target_sha TEXT NOT NULL, target_sha TEXT NOT NULL,
        local_head_sha TEXT NOT NULL, source_sha TEXT,
        step TEXT NOT NULL, generation INTEGER NOT NULL, at INTEGER NOT NULL, forge_head_sha TEXT,
        UNIQUE(run_id,generation)
    );
CREATE TRIGGER clear_terminal_target_rewind
        AFTER UPDATE OF status ON runs
        WHEN NEW.status NOT IN ('pending','running','queued')
        BEGIN UPDATE runs SET pending_target_rewind_id=NULL WHERE id=NEW.id; END;
CREATE TABLE target_rewind_history (
        rewind_id TEXT PRIMARY KEY REFERENCES run_target_rewinds(id) ON DELETE CASCADE,
        comparison_json TEXT NOT NULL, review_head_sha TEXT,
        steps_json TEXT NOT NULL, rounds_json TEXT NOT NULL,
        boundary_step TEXT, boundary_json TEXT
    );
CREATE TABLE run_rewind_pr_observations (
        run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
        pr_url TEXT NOT NULL, head_sha TEXT NOT NULL
    );
PRAGMA user_version=7;
