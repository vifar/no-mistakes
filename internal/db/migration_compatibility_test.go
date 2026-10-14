package db

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/verificationplan"
	_ "modernc.org/sqlite"
)

// Source snapshots are exact `git show <revision>:internal/db/schema.go`
// outputs. Keep these hashes in sync only when the pinned source revisions
// intentionally change: main-faa586d8 = faa586d839243dc290980e83e402d87105358ed5
// (SHA-256 3eac4fa984543f2262d295538bd6d7273d39f1370d3d0ff54688547d3c663c55),
// topic-fc890914 = fc890914 (SHA-256
// 000179d714dce4a58a1d90628ff2a66eb4f391216f68f1ee836720d747090121).
func TestMigrationCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fixture  string
		source   string
		wantHash string
	}{
		{"main", "main-faa586d8.go.txt", "faa586d839243dc290980e83e402d87105358ed5", "3eac4fa984543f2262d295538bd6d7273d39f1370d3d0ff54688547d3c663c55"},
		{"topic", "topic-fc890914.go.txt", "fc890914", "000179d714dce4a58a1d90628ff2a66eb4f391216f68f1ee836720d747090121"},
		{"fresh", "", "merged db.Open", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "compat.sqlite")
			var wantRun *Run
			var wantContext *PRContext
			var wantComparison *ReviewComparison
			if tc.fixture == "" {
				d, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				wantRun, wantContext, wantComparison = seedFreshCompatibilityDB(t, d)
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				snapshot, err := readMigrationSnapshot(t, tc.fixture, tc.source, tc.wantHash)
				if err != nil {
					t.Fatal(err)
				}
				if snapshot.source != tc.source {
					t.Fatalf("fixture source = %q, want %q", snapshot.source, tc.source)
				}
				if err := createHistoricalDB(path, snapshot); err != nil {
					t.Fatal(err)
				}
				wantRun, wantContext, wantComparison = seedHistoricalCompatibilityDB(t, path, tc.name)
			}

			d, err := Open(path)
			if err != nil {
				t.Fatalf("merged Open: %v", err)
			}
			assertCompatibilitySentinels(t, d, wantRun, wantContext, wantComparison)
			beforeCounts := compatibilityTableCounts(t, d)
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}

			d, err = Open(path)
			if err != nil {
				t.Fatalf("second merged Open: %v", err)
			}
			defer d.Close()
			assertCompatibilitySentinels(t, d, wantRun, wantContext, wantComparison)
			if got := compatibilityTableCounts(t, d); !reflect.DeepEqual(got, beforeCounts) {
				t.Fatalf("table counts changed after second Open: before=%v after=%v", beforeCounts, got)
			}
			var integrity string
			if err := d.sql.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
				t.Fatalf("integrity_check = %q, %v", integrity, err)
			}
			if _, err := d.sql.Exec(`UPDATE runs SET verification_plan='{}' WHERE id=?`, wantRun.ID); err == nil {
				t.Fatal("verification_plan mutation was accepted after reopen")
			}
			if _, err := d.sql.Exec(`UPDATE runs SET pi_profile='{}' WHERE id=?`, wantRun.ID); err == nil {
				t.Fatal("pi_profile mutation was accepted after reopen")
			}
		})
	}

	t.Run("migration errors are fail-closed except duplicate columns", func(t *testing.T) {
		db, err := openMigrationSQLite(filepath.Join(t.TempDir(), "errors.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := applyMigrationStatements(db, []string{
			`CREATE TABLE ordered (id INTEGER PRIMARY KEY)`,
			`ALTER TABLE ordered ADD COLUMN value TEXT`,
			`ALTER TABLE ordered ADD COLUMN value TEXT`,
		}); err != nil {
			t.Fatalf("duplicate-column migration should be tolerated: %v", err)
		}
		err = applyMigrationStatements(db, []string{
			`CREATE TABLE applied_before_error (id INTEGER)`,
			`ALTER TABLE absent_table ADD COLUMN value TEXT`,
			`CREATE TABLE must_not_run (id INTEGER)`,
		})
		if err == nil || !strings.Contains(err.Error(), "no such table") {
			t.Fatalf("non-duplicate migration error = %v, want fail-closed SQLite error", err)
		}
		for _, table := range []string{"applied_before_error", "must_not_run"} {
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 1
			if table == "must_not_run" {
				want = 0
			}
			if count != want {
				t.Fatalf("table %q presence = %d, want %d", table, count, want)
			}
		}
	})

	if dir := os.Getenv("NM_COMPAT_SOURCE_DBS"); dir != "" {
		for _, source := range []string{"faa586d8", "fc890914"} {
			t.Run("compiled source copy "+source, func(t *testing.T) {
				path := filepath.Join(dir, source+".sqlite")
				copyPath := filepath.Join(t.TempDir(), "source-copy.sqlite")
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(copyPath, b, 0600); err != nil {
					t.Fatal(err)
				}
				d, err := Open(copyPath)
				if err != nil {
					t.Fatalf("merged Open of source copy: %v", err)
				}
				defer d.Close()
				assertCompiledSourceRowsPreserved(t, path, d.sql, source)
				var integrity string
				if err := d.sql.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
					t.Fatalf("copied source integrity_check = %q, %v", integrity, err)
				}
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}
				d, err = Open(copyPath)
				if err != nil {
					t.Fatalf("second merged Open of source copy: %v", err)
				}
				defer d.Close()
				assertCompiledSourceRowsPreserved(t, path, d.sql, source)
				if source == "fc890914" {
					got, err := d.GetRun("run_topic")
					if err != nil || got.ExternalCIOwner != "controller-ship-pr" || !got.PRBaseBranchRequested {
						t.Fatalf("topic run owner/request = %#v, %v", got, err)
					}
					ctx, err := d.GetRunPRContext("run_topic")
					want := &PRContext{PRContextCandidate: PRContextCandidate{PRURL: "https://example.test/topic/pull/17", SourceRepo: "source-repo-sentinel", SourceBranch: "source-branch-sentinel", ForgeHeadSHA: "forge-head-sentinel", LocalHeadSHA: "local-head-sentinel", TargetBranch: "target-branch-sentinel", TargetSHA: "target-sha-sentinel", MergeBaseSHA: "merge-base-sentinel", DiffDigest: "diff-digest-sentinel"}, ObservedAt: 1700000004, Generation: 17}
					if err != nil || !reflect.DeepEqual(ctx, want) {
						t.Fatalf("topic PR context = %#v, want %#v, err %v", ctx, want, err)
					}
				}
			})
		}
	}
}

// Capture source tables and rows through a read-only connection before Open
// migrates the copy. Compare only columns that existed in the source schema.
func assertCompiledSourceRowsPreserved(t *testing.T, sourcePath string, merged *sql.DB, revision string) {
	t.Helper()
	source, err := sql.Open("sqlite", "file:"+sourcePath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	tables, err := source.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	type tableRows struct {
		name    string
		columns []string
		rows    [][]any
	}
	var captured []tableRows
	for tables.Next() {
		var table string
		if err := tables.Scan(&table); err != nil {
			t.Fatal(err)
		}
		columns, err := source.Query(`PRAGMA table_info("` + strings.ReplaceAll(table, `"`, `""`) + `")`)
		if err != nil {
			t.Fatal(err)
		}
		var cols []string
		for columns.Next() {
			var cid, notnull, pk int
			var name, typ string
			var def any
			if err := columns.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, name)
		}
		if err := columns.Err(); err != nil {
			t.Fatal(err)
		}
		columns.Close()
		quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
		selectCols := make([]string, len(cols))
		for i, c := range cols {
			selectCols[i] = `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
		}
		rows, err := source.Query(`SELECT ` + strings.Join(selectCols, ",") + ` FROM ` + quoted + ` ORDER BY rowid`)
		if err != nil {
			t.Fatalf("%s read source table %s: %v", revision, table, err)
		}
		tr := tableRows{name: table, columns: cols, rows: make([][]any, 0)}
		for rows.Next() {
			vals := make([]any, len(cols))
			dest := make([]any, len(cols))
			for i := range vals {
				dest[i] = &vals[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			tr.rows = append(tr.rows, vals)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		captured = append(captured, tr)
	}
	if err := tables.Err(); err != nil {
		t.Fatal(err)
	}
	tables.Close()
	for _, tr := range captured {
		selectCols := make([]string, len(tr.columns))
		for i, c := range tr.columns {
			selectCols[i] = `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
		}
		quoted := `"` + strings.ReplaceAll(tr.name, `"`, `""`) + `"`
		rows, err := merged.Query(`SELECT ` + strings.Join(selectCols, ",") + ` FROM ` + quoted + ` ORDER BY rowid`)
		if err != nil {
			t.Fatalf("%s read merged table %s: %v", revision, tr.name, err)
		}
		got := make([][]any, 0)
		for rows.Next() {
			vals := make([]any, len(tr.columns))
			dest := make([]any, len(vals))
			for i := range vals {
				dest[i] = &vals[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			got = append(got, vals)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if !reflect.DeepEqual(got, tr.rows) {
			t.Fatalf("%s table %s changed: columns=%v source rows=%#v merged rows=%#v", revision, tr.name, tr.columns, tr.rows, got)
		}
	}
}

type migrationSnapshot struct {
	source     string
	schemaSQL  string
	statements []string
}

func readMigrationSnapshot(t *testing.T, name, source, wantHash string) (migrationSnapshot, error) {
	t.Helper()
	path := filepath.Join("testdata", "migrations", name)
	data, err := os.ReadFile(path)
	if err != nil {
		return migrationSnapshot{}, err
	}
	if got := fmt.Sprintf("%x", sha256Sum(data)); got != wantHash {
		return migrationSnapshot{}, fmt.Errorf("%s SHA-256 = %s, want %s", name, got, wantHash)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		return migrationSnapshot{}, fmt.Errorf("parse %s: %w", name, err)
	}
	out := migrationSnapshot{source: source}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			switch item := spec.(type) {
			case *ast.ValueSpec:
				for i, ident := range item.Names {
					if ident.Name == "schemaSQL" && i < len(item.Values) {
						out.schemaSQL, err = stringLiteral(item.Values[i])
						if err != nil {
							return out, err
						}
					}
					if ident.Name == "migrationStatements" && i < len(item.Values) {
						array, ok := item.Values[i].(*ast.CompositeLit)
						if !ok {
							return out, fmt.Errorf("migrationStatements is not a composite literal")
						}
						for _, elt := range array.Elts {
							statement, err := stringLiteral(elt)
							if err != nil {
								return out, fmt.Errorf("migration statement: %w", err)
							}
							out.statements = append(out.statements, statement)
						}
					}
				}
			}
		}
	}
	if out.schemaSQL == "" || len(out.statements) == 0 {
		return out, fmt.Errorf("%s is missing schemaSQL or migrationStatements", name)
	}
	return out, nil
}

func stringLiteral(expr ast.Expr) (string, error) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", fmt.Errorf("expected string literal at token position %d", expr.Pos())
	}
	return strconv.Unquote(lit.Value)
}

func createHistoricalDB(path string, snapshot migrationSnapshot) error {
	db, err := openMigrationSQLite(path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(snapshot.schemaSQL); err != nil {
		return fmt.Errorf("historical schema: %w", err)
	}
	if err := applyMigrationStatements(db, snapshot.statements); err != nil {
		return fmt.Errorf("historical migration sequence: %w", err)
	}
	return nil
}

func openMigrationSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func applyMigrationStatements(db *sql.DB, statements []string) error {
	for i, statement := range statements {
		if _, err := db.Exec(statement); err != nil && !isDuplicateColumnErr(err) {
			return fmt.Errorf("statement %d: %w", i, err)
		}
	}
	return nil
}

func seedHistoricalCompatibilityDB(t *testing.T, path, side string) (*Run, *PRContext, *ReviewComparison) {
	t.Helper()
	db, err := openMigrationSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO repos (id,working_path,upstream_url,fork_url,default_branch,created_at) VALUES (?, ?, ?, ?, 'main', 101)`, "repo-"+side, filepath.Join(t.TempDir(), side), "https://example.test/upstream.git", "https://example.test/fork.git"); err != nil {
		t.Fatal(err)
	}
	plan := `{"id":"plan-` + side + `","path":"plan.md","source_path":"/private/plan.md","sha256":"` + strings.Repeat("a", 64) + `","captured_at":102}`
	profile := `{"model":"openai-codex/gpt-5.4","effort":"high"}`
	insertHistoricalRun(t, db, side, plan, profile, side == "main")
	if _, err := db.Exec(`INSERT INTO step_results (id,run_id,step_name,step_order,status,findings_json,error,started_at,completed_at) VALUES (?, ?, 'review', 2, 'completed', ?, ?, 103, 104)`, "step-"+side, "run-"+side, `[{"id":"sentinel-`+side+`"}]`, "step-error-"+side); err != nil {
		t.Fatal(err)
	}
	if side == "main" {
		_, err = db.Exec(`INSERT INTO review_comparisons (run_id,source_repo,source_branch,target_branch,target_sha,head_sha,merge_base_sha,diff_digest,generation,approved) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 7, 1)`, "run-"+side, "owner/repo", "feature-main", "main", strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40), strings.Repeat("4", 64))
	} else {
		_, err = db.Exec(`INSERT INTO run_pr_contexts (run_id,pr_url,source_repo,source_branch,forge_head_sha,local_head_sha,target_branch,target_sha,merge_base_sha,diff_digest,observed_at,generation) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 105, 8)`, "run-"+side, "https://example.test/pr/8", "owner/repo", "feature-topic", strings.Repeat("5", 40), strings.Repeat("6", 40), "main", strings.Repeat("7", 40), strings.Repeat("8", 40), strings.Repeat("9", 64))
	}
	if err != nil {
		t.Fatal(err)
	}
	return historicalExpectedRun(side), historicalExpectedContext(side), historicalExpectedComparison(side)
}

func insertHistoricalRun(t *testing.T, db *sql.DB, side, plan, profile string, withShipping bool) {
	t.Helper()
	// These columns exist in both snapshots; migrations add later columns.
	columns := `id,repo_id,branch,head_sha,base_sha,worktree_dir,submitted_head_sha,no_mistakes_version,no_mistakes_build_sha,review_approved_head_sha,status,pr_url,pr_state,pr_state_observed_at,ci_ready_at,ci_ready_no_ci,last_pushed_sha,push_target_kind,push_target_fingerprint,push_ref,last_pushed_at,push_generation,push_active,terminal_head_verified_at,gates_json,error,awaiting_agent_since,parked_ms,launch_nonce,launch_validation_generation,launch_intent_digest,launch_receipt_claimed_at,pr_base_branch,omit_intent,pi_profile,verification_plan,created_at,updated_at`
	values := []any{"run-" + side, "repo-" + side, "branch-" + side, "head-" + side, "base-" + side, "/worktree/" + side, "submitted-" + side, "version-" + side, "build-" + side, "approved-" + side, "running", "https://example.test/pr/" + side, "open", 106, 107, 1, "pushed-" + side, "github", "fingerprint-" + side, "refs/heads/" + side, int64(108), int64(9), 1, int64(109), `["review","test"]`, `run-error-` + side, int64(110), int64(111), "nonce-" + side, "validation-" + side, "digest-" + side, int64(112), "release", 1, profile, plan, int64(113), int64(114)}
	if withShipping {
		columns += `,shipping_options`
		values = append(values, `{"mode":"full","target_branch":"main"}`)
	}
	query := `INSERT INTO runs (` + columns + `) VALUES (` + sqlPlaceholders(len(values)) + `)`
	// Current historical versions differ in whether several requested/owner fields
	// are in schemaSQL or migrations. Set those only after column introspection.
	if _, err := db.Exec(query, values...); err != nil {
		t.Fatalf("insert historical run: %v", err)
	}
	for column, value := range map[string]any{"pr_base_branch_requested": 1, "external_ci_owner": "controller-ship-pr"} {
		if historicalHasColumn(t, db, "runs", column) {
			if _, err := db.Exec(`UPDATE runs SET `+column+`=? WHERE id=?`, value, "run-"+side); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func sqlPlaceholders(count int) string { return strings.TrimSuffix(strings.Repeat("?,", count), ",") }

func historicalHasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}

func historicalExpectedRun(side string) *Run {
	var shipping *types.ShippingOptions
	if side == "main" {
		shipping = &types.ShippingOptions{Mode: types.ShippingFull, TargetBranch: "main"}
	}
	profile := agentcfg.PiProfile{Model: "openai-codex/gpt-5.4", Effort: agentcfg.EffortHigh}
	plan := verificationplan.Snapshot{ID: "plan-" + side, Path: "plan.md", SourcePath: "/private/plan.md", SHA256: strings.Repeat("a", 64), CapturedAt: 102}
	requested, externalOwner := true, "controller-ship-pr"
	if side == "main" {
		// This source predates both fields. Merged Open supplies their legacy defaults.
		requested, externalOwner = false, ""
	}
	return &Run{ID: "run-" + side, RepoID: "repo-" + side, Branch: "branch-" + side, HeadSHA: "head-" + side, BaseSHA: "base-" + side, WorktreeDir: strptr("/worktree/" + side), SubmittedHeadSHA: strptr("submitted-" + side), NoMistakesVersion: strptr("version-" + side), NoMistakesBuildSHA: strptr("build-" + side), ReviewApprovedHeadSHA: strptr("approved-" + side), Status: types.RunRunning, PRURL: strptr("https://example.test/pr/" + side), PRState: strptr("open"), PRStateObservedAt: int64ptr(106), CIReadyAt: int64ptr(107), CIReadyNoCI: true, LastPushedSHA: strptr("pushed-" + side), PushTargetKind: strptr("github"), PushTargetFingerprint: strptr("fingerprint-" + side), PushRef: strptr("refs/heads/" + side), LastPushedAt: int64ptr(108), PushGeneration: int64ptr(9), PushActive: true, TerminalHeadVerifiedAt: int64ptr(109), Error: strptr("run-error-" + side), AwaitingAgentSince: int64ptr(110), ParkedMS: 111, LaunchNonce: strptr("nonce-" + side), LaunchValidationGeneration: strptr("validation-" + side), LaunchIntentDigest: strptr("digest-" + side), LaunchReceiptClaimedAt: int64ptr(112), PRBaseBranch: strptr("release"), PRBaseBranchRequested: requested, OmitIntent: true, ExternalCIOwner: externalOwner, PiProfile: &profile, VerificationPlan: &plan, Shipping: shipping, CreatedAt: 113, UpdatedAt: 114}
}

func historicalExpectedContext(side string) *PRContext {
	if side != "topic" {
		return nil
	}
	return &PRContext{PRContextCandidate: PRContextCandidate{PRURL: "https://example.test/pr/8", SourceRepo: "owner/repo", SourceBranch: "feature-topic", ForgeHeadSHA: strings.Repeat("5", 40), LocalHeadSHA: strings.Repeat("6", 40), TargetBranch: "main", TargetSHA: strings.Repeat("7", 40), MergeBaseSHA: strings.Repeat("8", 40), DiffDigest: strings.Repeat("9", 64)}, ObservedAt: 105, Generation: 8}
}
func historicalExpectedComparison(side string) *ReviewComparison {
	if side != "main" {
		return nil
	}
	return &ReviewComparison{SourceRepo: "owner/repo", SourceBranch: "feature-main", TargetBranch: "main", TargetSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40), MergeBaseSHA: strings.Repeat("3", 40), DiffDigest: strings.Repeat("4", 64), Generation: 7, Approved: true}
}

func seedFreshCompatibilityDB(t *testing.T, d *DB) (*Run, *PRContext, *ReviewComparison) {
	t.Helper()
	repo, err := d.InsertRepo(t.TempDir(), "https://example.test/fresh.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	profile := &agentcfg.PiProfile{Model: "openai-codex/gpt-5.4", Effort: agentcfg.EffortHigh}
	plan := &verificationplan.Snapshot{ID: "plan-fresh", Path: "plan.md", SourcePath: "/private/plan.md", SHA256: strings.Repeat("a", 64), CapturedAt: 102}
	run, err := d.InsertRunWithAllOptions(repo.ID, "fresh", "head-fresh", "base-fresh", nil, "", "", "", "release", true, true, "controller-ship-pr", plan, profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`INSERT INTO step_results (id,run_id,step_name,step_order,status,findings_json,error,started_at,completed_at) VALUES ('step-fresh', ?, 'review', 2, 'completed', '[{"id":"sentinel-fresh"}]', 'step-error-fresh', 103, 104)`, run.ID); err != nil {
		t.Fatal(err)
	}
	// Fresh uses the same distinct API surfaces with valid full-length comparison values.
	ctx := historicalExpectedContext("topic")
	ctxCandidate := ctx.PRContextCandidate
	if _, err := d.BindRunPRContext(run.ID, ctxCandidate, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	comparison := historicalExpectedComparison("main")
	if _, err := d.CaptureReviewComparison(run.ID, *comparison); err != nil {
		t.Fatal(err)
	}
	gotRun, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotContext, err := d.GetRunPRContext(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotComparison, err := d.GetReviewComparison(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return gotRun, gotContext, gotComparison
}

func assertCompatibilitySentinels(t *testing.T, d *DB, wantRun *Run, wantContext *PRContext, wantComparison *ReviewComparison) {
	t.Helper()
	got, err := d.GetRun(wantRun.ID)
	if err != nil || !reflect.DeepEqual(got, wantRun) {
		t.Fatalf("GetRun sentinel mismatch:\n got: %#v\nwant: %#v\nerr: %v", got, wantRun, err)
	}
	ctx, err := d.GetRunPRContext(wantRun.ID)
	if err != nil || !reflect.DeepEqual(ctx, wantContext) {
		t.Fatalf("GetRunPRContext = %#v, want %#v, err %v", ctx, wantContext, err)
	}
	comparison, err := d.GetReviewComparison(wantRun.ID)
	if err != nil || !reflect.DeepEqual(comparison, wantComparison) {
		t.Fatalf("GetReviewComparison = %#v, want %#v, err %v", comparison, wantComparison, err)
	}
	var stepID, stepName, status, findings, stepError string
	var stepOrder, startedAt, completedAt int64
	if err := d.sql.QueryRow(`SELECT id,step_name,step_order,status,findings_json,error,started_at,completed_at FROM step_results WHERE run_id=?`, wantRun.ID).Scan(&stepID, &stepName, &stepOrder, &status, &findings, &stepError, &startedAt, &completedAt); err != nil {
		t.Fatalf("read preserved step row: %v", err)
	}
	side := strings.TrimPrefix(wantRun.ID, "run-")
	if side == wantRun.ID {
		side = "fresh"
	}
	wantStepID := "step-" + side
	wantFindings := `[{"id":"sentinel-` + side + `"}]`
	if stepID != wantStepID || stepName != "review" || stepOrder != 2 || status != "completed" || findings != wantFindings || stepError != "step-error-"+side || startedAt != 103 || completedAt != 104 {
		t.Fatalf("step row changed: id=%q name=%q order=%d status=%q findings=%q error=%q started=%d completed=%d", stepID, stepName, stepOrder, status, findings, stepError, startedAt, completedAt)
	}
}

func compatibilityTableCounts(t *testing.T, d *DB) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"repos", "runs", "step_results", "review_comparisons", "run_pr_contexts"} {
		if !hasColumn(t, d, table, "id") && table != "review_comparisons" && table != "run_pr_contexts" {
			continue
		}
		var count int
		if err := d.sql.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			// A historical fixture does not create the opposing side's table until merged Open.
			if strings.Contains(err.Error(), "no such table") {
				out[table] = 0
				continue
			}
			t.Fatal(err)
		}
		out[table] = count
	}
	return out
}

func sha256Sum(data []byte) [32]byte { return sha256.Sum256(data) }
func strptr(v string) *string        { return &v }
func int64ptr(v int64) *int64        { return &v }
