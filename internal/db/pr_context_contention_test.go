package db

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
	"modernc.org/sqlite"
)

// This driver only observes the real driver's rows. Closing QueryRow's receipt
// rows is the barrier: both SELECTs have established the transaction's snapshot,
// and the production INSERT has not run. No production test hook is needed.
type receiptBarrierDriver struct{ afterRead func() }

func (d receiptBarrierDriver) Open(name string) (driver.Conn, error) {
	c, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return &receiptBarrierConn{Conn: c, afterRead: d.afterRead}, nil
}

type receiptBarrierConn struct {
	driver.Conn
	afterRead func()
}

func (c *receiptBarrierConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err == nil && strings.Contains(query, "SELECT pr_url, source_repo, source_branch, forge_head_sha,") {
		return &receiptBarrierRows{Rows: rows, afterRead: c.afterRead}, nil
	}
	return rows, err
}

type receiptBarrierRows struct {
	driver.Rows
	afterRead func()
}

func (r *receiptBarrierRows) Close() error {
	err := r.Rows.Close()
	r.afterRead()
	return err
}

type receiptContentionResult struct {
	Attempts   int
	Generation int64
	Changed    bool
	Error      string
	Code       int
}

// A separate process executes the unmodified public writer through the real
// modernc driver. Its parent owns the competing connection and commits before
// acknowledging each snapshot barrier. All database files are under TempDir.
func TestPRContextContentionProcess(t *testing.T) {
	mode := os.Getenv("NM_TEST_RECEIPT_CONTENTION")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	input := bufio.NewReader(os.Stdin)
	attempts := 0
	sql.Register("receipt-barrier", receiptBarrierDriver{afterRead: func() {
		attempts++
		if mode == "recover" && attempts > 1 {
			return
		}
		fmt.Fprintln(os.Stdout, "SNAPSHOT")
		if line, err := input.ReadString('\n'); err != nil || line != "COMMITTED\n" {
			panic(fmt.Sprintf("competing commit acknowledgement: %q, %v", line, err))
		}
	}})
	raw, err := sql.Open("receipt-barrier", os.Getenv("NM_TEST_RECEIPT_DB")+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	defer raw.Close()
	d := &DB{sql: raw}
	candidate := testPRContextCandidate()
	candidate.TargetSHA = strings.Repeat("e", 40)
	got, err := d.BindRunPRContext(os.Getenv("NM_TEST_RECEIPT_RUN"), candidate, types.StepReview)
	result := receiptContentionResult{Attempts: attempts, Generation: got.Generation, Changed: got.Changed}
	if err != nil {
		result.Error = err.Error()
		var sqliteErr *sqlite.Error
		if errors.As(err, &sqliteErr) {
			result.Code = sqliteErr.Code()
		}
	}
	b, _ := json.Marshal(result)
	fmt.Fprintln(os.Stdout, "RESULT "+string(b))
}

func TestPRContextContention(t *testing.T) {
	for _, mode := range []string{"recover", "exhaust"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.sqlite")
			d, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			var journal string
			var timeout int
			if err := d.sql.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
				t.Fatalf("journal mode = %s, %v", journal, err)
			}
			if err := d.sql.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != 5000 {
				t.Fatalf("busy timeout = %d, %v", timeout, err)
			}
			runID := testPRContextRun(t, d)
			candidate := testPRContextCandidate()
			if _, err := d.BindRunPRContext(runID, candidate, types.StepReview); err != nil {
				t.Fatal(err)
			}
			step, err := d.InsertStepResult(runID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.sql.Exec("UPDATE step_results SET status=? WHERE id=?", types.StepStatusCompleted, step.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunReviewApprovedHeadSHA(runID, candidate.LocalHeadSHA); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPRContextContentionProcess$")
			cmd.Env = append(os.Environ(), "NM_TEST_RECEIPT_CONTENTION="+mode, "NM_TEST_RECEIPT_DB="+path, "NM_TEST_RECEIPT_RUN="+runID)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Always reap the helper, including on a failed barrier or assertion.
			defer func() { cancel(); stdin.Close(); cmd.Wait() }()
			scanner := bufio.NewScanner(stdout)
			barriers := 0
			var result *receiptContentionResult
			for scanner.Scan() {
				line := scanner.Text()
				if line == "SNAPSHOT" {
					barriers++
					if mode == "recover" {
						other := candidate
						other.TargetSHA = strings.Repeat("f", 40)
						got, err := d.AdvanceRunPRContext(runID, other)
						if err != nil || got.Generation != 2 {
							t.Fatalf("competing committed receipt = %+v, %v", got, err)
						}
					} else if _, err := d.sql.Exec("UPDATE runs SET updated_at=updated_at+1 WHERE id=?", runID); err != nil {
						t.Fatal(err)
					}
					if _, err := io.WriteString(stdin, "COMMITTED\n"); err != nil {
						t.Fatal(err)
					}
				} else if strings.HasPrefix(line, "RESULT ") {
					if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "RESULT ")), &result); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := scanner.Err(); err != nil || result == nil {
				t.Fatalf("helper result = %+v, scan=%v stderr=%s", result, err, stderr.String())
			}
			t.Logf("two-process committed-snapshot interleaving: barriers=%d result=%+v", barriers, *result)
			receipt, err := d.GetRunPRContext(runID)
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.GetRun(runID)
			if err != nil {
				t.Fatal(err)
			}
			review, err := d.GetStepResult(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "recover" {
				// The competitor committed generation 2. A fresh read must produce 3,
				// never replay a stale generation 2 or leave approved review authority.
				if result.Error != "" || !result.Changed || result.Generation != 3 || result.Attempts != 2 || barriers != 1 {
					t.Fatalf("writer must recover with a fresh transaction: %+v, barriers=%d", *result, barriers)
				}
				candidate.TargetSHA = strings.Repeat("e", 40)
				if receipt.Generation != 3 || receipt.PRContextCandidate != candidate || run.ReviewApprovedHeadSHA != nil || review.Status != types.StepStatusPending {
					t.Fatalf("receipt and authority must commit together: receipt=%+v run=%+v review=%+v", receipt, run, review)
				}
			} else {
				// The shared time budget bounds full transactions. An
				// exhausted stale snapshot must preserve the old receipt + authority.
				if !strings.Contains(result.Error, "SQLITE_WRITE_CONTENTION:") || result.Code != 517 || result.Changed || result.Generation != 0 || result.Attempts < 4 || barriers != result.Attempts {
					t.Fatalf("writer must fail closed after the shared retry budget: %+v, barriers=%d", *result, barriers)
				}
				if receipt.Generation != 1 || receipt.PRContextCandidate != candidate || run.ReviewApprovedHeadSHA == nil || review.Status != types.StepStatusCompleted {
					t.Fatalf("exhaustion changed receipt or authority: receipt=%+v run=%+v review=%+v", receipt, run, review)
				}
			}
		})
	}
}
