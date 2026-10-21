//go:build e2e && !windows

package e2e

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These journeys use short real-time budgets, the real daemon and its SQLite
// state, and the owned stateful forge. The oracle is issue #87's contract:
// only consecutive otherwise-ready observations without proof spend the bound.
// Restoring proof copies the fixture's original receipt; it never invents one.
func TestCIReceiptRecoveryJourneys(t *testing.T) {
	for _, mode := range []string{"receipt-recovers", "pending-resets", "unreadable-resets", "base-advance", "other-comparison-error", "absent-checks"} {
		t.Run(mode, func(t *testing.T) {
			budget := "8s"
			if mode == "pending-resets" || mode == "unreadable-resets" {
				budget = "40s"
			}
			ciTimeout := "unlimited"
			if mode == "base-advance" {
				ciTimeout = "2m"
			}
			j := newScopeJourney(t, SetupOpts{Agent: "claude", GlobalConfigExtra: "ci_timeout: " + ciTimeout + "\nci_readiness_receipt_timeout: " + budget + "\n"})
			pending := forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{runningCheck("build")}}
			green := forgeState{Protection: protectionJSON("build@15368"), Checks: []forgeCheck{greenCheck("build")}}
			if mode == "absent-checks" {
				pending.Protection = nil
			}
			j.setForge(pending)
			j.init()
			branch := "feature/" + mode
			j.push(branch)
			run := j.expectWaiting(branch, "CI checks running, waiting for results...")
			database, err := sql.Open("sqlite", filepath.Join(j.h.NMHome, "state.sqlite")+"?_pragma=busy_timeout(5000)")
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			database.SetMaxOpenConns(1)
			execSQL := func(query string, args ...any) {
				t.Helper()
				if _, err := database.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			execSQL("CREATE TEMP TABLE receipt_backup AS SELECT * FROM run_pr_contexts WHERE run_id = ?", run.ID)
			var saved int
			if err := database.QueryRow("SELECT count(*) FROM receipt_backup").Scan(&saved); err != nil || saved != 1 {
				t.Fatalf("fixture must own exactly one original receipt: count=%d err=%v", saved, err)
			}
			execSQL("DELETE FROM run_pr_contexts WHERE run_id = ?", run.ID)
			missing := "CI readiness lacks a current PR comparison receipt"
			waitCount := func(message string, count int) {
				t.Helper()
				deadline := time.Now().Add(90 * time.Second)
				for time.Now().Before(deadline) {
					if strings.Count(j.ciLog(run.ID), message) >= count {
						return
					}
					if info := j.h.RunInfo(run.ID); info.Status.Terminal() {
						t.Fatalf("premature terminal run: %s %s\n%s", info.Status, deref(info.Error), j.ciLog(run.ID))
					}
					time.Sleep(100 * time.Millisecond)
				}
				t.Fatalf("missing observation %q #%d:\n%s", message, count, j.ciLog(run.ID))
			}
			assertMonitoring := func(ready bool) {
				t.Helper()
				info := j.h.RunInfo(run.ID)
				ci, _ := findStep(info.Steps, types.StepCI)
				if info.Status.Terminal() || ci.Status != types.StepStatusRunning || info.CIReady != ready || deref(ci.FindingsJSON) != "" {
					t.Fatalf("unexpected monitoring state: status=%s ci=%s ready=%v findings=%s err=%s\n%s", info.Status, ci.Status, info.CIReady, deref(ci.FindingsJSON), deref(info.Error), j.ciLog(run.ID))
				}
				out, err := j.h.Run("axi", "status", "--run", run.ID)
				if err != nil {
					t.Fatalf("status: %v\n%s", err, out)
				}
				t.Logf("Live monitoring observation, expected ready=%v:\n%s", ready, out)
			}

			if mode == "receipt-recovers" {
				// Multiple pending polls outlive the configured proof bound.
				before := j.rollupReads()
				deadline := time.Now().Add(90 * time.Second)
				for j.rollupReads() <= before && time.Now().Before(deadline) {
					time.Sleep(100 * time.Millisecond)
				}
				if j.rollupReads() <= before {
					t.Fatal("pending checks were never polled again")
				}
				assertMonitoring(false)
			}
			if mode == "absent-checks" {
				// Issue #122's absent-CI wait must not become issue #87's
				// otherwise-ready missing-proof failure.
				j.setForge(forgeState{})
				waitCount("no CI checks reported yet", 1)
				time.Sleep(10 * time.Second)
				assertMonitoring(false)
				if strings.Contains(j.ciLog(run.ID), missing) {
					t.Fatal("absent checks were classified as otherwise ready")
				}
				t.Logf("Absent-checks live CI log:\n%s", j.ciLog(run.ID))
				return
			}
			j.setForge(green)
			waitCount(missing, 1)

			switch mode {
			case "receipt-recovers":
				execSQL("INSERT INTO run_pr_contexts SELECT * FROM receipt_backup")
				j.expectReady(branch, 90*time.Second)
				time.Sleep(10 * time.Second)
				assertMonitoring(true)
			case "pending-resets", "unreadable-resets":
				if mode == "pending-resets" {
					j.setForge(pending)
				} else {
					if err := os.WriteFile(j.forgePath, []byte("not valid forge JSON"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				// Outlive the old proof deadline, with an intervening pending or
				// unreadable observation. Reusing the first anchor would fail.
				time.Sleep(45 * time.Second)
				assertMonitoring(false)
				if mode == "unreadable-resets" {
					waitCount("could not read the PR target branch", 1)
				}
				before := strings.Count(j.ciLog(run.ID), missing)
				j.setForge(green)
				waitCount(missing, before+1)
				assertMonitoring(false)
				j.waitCILog(run.ID, missing, 10*time.Second)
				deadline := time.Now().Add(80 * time.Second)
				for !j.h.RunInfo(run.ID).Status.Terminal() && time.Now().Before(deadline) {
					time.Sleep(100 * time.Millisecond)
				}
				info := j.h.RunInfo(run.ID)
				if info.Status != types.RunFailed || !strings.Contains(deref(info.Error), "ci-readiness-receipt-missing") {
					t.Fatalf("reset budget did not expire with its named error: %s %s", info.Status, deref(info.Error))
				}
			case "base-advance":
				j.h.CommitChange("main", "base-advanced.txt", "new base\n", "advance fixture base")
				if out, err := j.h.runGit(t.Context(), j.h.WorkDir, "push", "origin", "main"); err != nil {
					t.Fatalf("fixture base push: %v\n%s", err, out)
				}
				deadline := time.Now().Add(25 * time.Second)
				for !j.h.RunInfo(run.ID).Status.Terminal() && time.Now().Before(deadline) {
					time.Sleep(100 * time.Millisecond)
				}
				info := j.h.RunInfo(run.ID)
				if info.Status != types.RunFailed || !strings.Contains(deref(info.Error), "ci-readiness-receipt-missing") {
					t.Fatalf("base advancement prolonged the proof budget: %s %s\n%s", info.Status, deref(info.Error), j.ciLog(run.ID))
				}
			case "other-comparison-error":
				execSQL("INSERT INTO run_pr_contexts SELECT * FROM receipt_backup")
				execSQL("UPDATE run_pr_contexts SET diff_digest = ? WHERE run_id = ?", strings.Repeat("0", 64), run.ID)
				waitCount("diff differs from PR comparison receipt", 1)
				time.Sleep(10 * time.Second)
				assertMonitoring(false)
				// Restore only the original fixture proof, then observe recovery.
				execSQL("DELETE FROM run_pr_contexts WHERE run_id = ?", run.ID)
				execSQL("INSERT INTO run_pr_contexts SELECT * FROM receipt_backup")
				j.expectReady(branch, 90*time.Second)
			}
			out, err := j.h.Run("axi", "status", "--run", run.ID)
			if err != nil {
				t.Fatalf("status: %v\n%s", err, out)
			}
			t.Logf("Final real CLI status:\n%s\nCI log:\n%s", out, j.ciLog(run.ID))
		})
	}
}

// Exercise operator configuration through the real doctor's public output.
// Invalid budgets must be refused rather than silently disabling the bound.
func TestCIReceiptTimeoutDoctorConfig(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	path := filepath.Join(h.NMHome, "config.yaml")
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		value   string
		invalid bool
	}{
		{"", false}, {"1h", false}, {"0s", true}, {"-1m", true}, {"unlimited", true}, {"bad", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			text := string(base)
			if tc.value != "" {
				text += "ci_readiness_receipt_timeout: " + tc.value + "\n"
			}
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			out, err := h.Run("doctor")
			if err != nil {
				t.Fatalf("doctor: %v\n%s", err, out)
			}
			want := "claude is runnable"
			if tc.invalid {
				want = "unavailable: load config (parse ci_readiness_receipt_timeout"
			}
			if !strings.Contains(out, want) {
				t.Fatalf("doctor did not enforce the positive-only budget contract for %q:\n%s", tc.value, out)
			}
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "gate validation") {
					t.Logf("Real CLI doctor, budget=%q: %s", tc.value, line)
				}
			}
		})
	}
}
