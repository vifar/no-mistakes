//go:build windows

package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecLintCheck_WindowsShimPreservesLiteralArguments(t *testing.T) {
	t.Parallel()
	for _, ext := range []string{".cmd", ".BAT"} {
		t.Run(ext, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			binary := filepath.Join(dir, "tool with spaces"+ext)
			if err := os.WriteFile(binary, []byte("@echo off\r\necho first=[%~1]\r\necho second=[%~2]\r\nexit /b 0\r\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			output, exit, err := execLintCheck(&pipeline.StepContext{}, lintCheckTimeout)(context.Background(), dir, binary, []string{"--check", `src\a file.js`})
			if err != nil || exit != 0 || strings.ReplaceAll(output, "\r\n", "\n") != "first=[--check]\nsecond=[src\\a file.js]\n" {
				t.Fatalf("output = %q, exit = %d, error = %v; want literal arguments", output, exit, err)
			}
		})
	}
}

func TestLintToolCheck_WindowsShimRefusesUnsafePathsWithoutExecution(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		binary string
		file   string
	}{
		{"file", "eslint.cmd", "a&whoami&b.js"},
		{"binary", "eslint&injected.cmd", "plain.js"},
		{"batch", "eslint.bat", "a%PATH%b.js"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			binary := filepath.Join(dir, tc.binary)
			if err := os.WriteFile(binary, []byte("@echo off\r\necho launched>\"%~dp0launched\"\r\nexit /b 0\r\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			sctx := &pipeline.StepContext{Ctx: context.Background(), WorkDir: dir}
			plan := lintToolPlan{Tool: knownLintTools[1], Dir: ".", Binary: binary, Files: []string{tc.file}}
			findings, err := reconcileLintToolFindings(sctx, Findings{Items: []Finding{{ID: "lint-missing-eslint"}}}, []lintToolPlan{plan}, execLintCheck(sctx, lintCheckTimeout))
			if err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != 1 {
				t.Fatalf("findings = %+v, want one unsafe-verification warning", findings.Items)
			}
			got := findings.Items[0]
			if got.ID != "lint-eslint-check" || got.Category != types.FindingCategoryLint || got.Action != types.ActionAskUser || got.Severity != types.FindingSeverityWarning || !strings.Contains(got.Description, "shell metacharacters") || !strings.Contains(got.Description, "run the check by hand") {
				t.Fatalf("finding = %+v, want a lint warning requesting manual verification", got)
			}
			if _, err := os.Stat(filepath.Join(dir, "launched")); !os.IsNotExist(err) {
				t.Fatalf("launch marker error = %v, want the shim never to execute", err)
			}
		})
	}
}
