package steps

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func writeLintFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.Contains(name, "node_modules/.bin/") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
}

func noLookPath(string) string { return "" }

func TestLintWindowsShimCommandLine_RefusesShellMetacharacters(t *testing.T) {
	t.Parallel()
	for _, unsafe := range []string{"a&whoami&b.js", "a|b.js", "a<b.js", "a>b.js", "a^b.js", "a%b.js", "a!b.js", "a\"b.js", "a(b.js", "a)b.js", "a\nb.js", "a\rb.js"} {
		t.Run(unsafe, func(t *testing.T) {
			t.Parallel()
			for _, args := range [][]string{
				{unsafe, "--check", "plain.js"},
				{"eslint.cmd", "--check", unsafe},
				{"eslint.cmd", unsafe, "plain.js"},
			} {
				line, err := lintWindowsShimCommandLine(args[0], args[1:])
				if err == nil || line != "" || !strings.Contains(err.Error(), "shell metacharacters") || !strings.Contains(err.Error(), "run the check by hand") {
					t.Fatalf("arguments %q: line = %q, error = %v; want refusal without a command line", args, line, err)
				}
			}
		})
	}
}

func TestLintWindowsShimCommandLine_QuotesLiteralArguments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		binary string
		args   []string
		want   string
	}{
		{"eslint.cmd", []string{"plain.js"}, `cmd.exe /d /s /c ""eslint.cmd" "plain.js""`},
		{`C:\repo with spaces\eslint.cmd`, []string{"--check", `src\a file.js`}, `cmd.exe /d /s /c ""C:\repo with spaces\eslint.cmd" "--check" "src\a file.js""`},
		{"stylelint.bat", []string{"", "日本語.css"}, `cmd.exe /d /s /c ""stylelint.bat" "" "日本語.css""`},
	} {
		line, err := lintWindowsShimCommandLine(tc.binary, tc.args)
		if err != nil || line != tc.want {
			t.Fatalf("binary %q, arguments %q: line = %q, error = %v; want %q", tc.binary, tc.args, line, err, tc.want)
		}
	}
}

func TestExecLintCheck_CancellationReleasesInheritedOutputPipes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a Unix shell")
	}
	t.Parallel()
	dir := t.TempDir()
	writeLintFixture(t, dir, map[string]string{
		"node_modules/.bin/biome": "#!/bin/sh\nsleep 60 &\nsleep 60\n",
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	output, exitCode, err := execLintCheck(&pipeline.StepContext{}, lintCheckTimeout)(ctx, dir, filepath.Join(dir, "node_modules", ".bin", "biome"), nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancelled check took %s, want under 5s; exit=%d error=%v output=%q", elapsed, exitCode, err, output)
	}
	if err == nil && exitCode == 0 {
		t.Fatalf("cancelled check succeeded; output=%q", output)
	}
}

func TestResolveLintTools_SubPackagePrettierResolvesFromThatPackage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeLintFixture(t, root, map[string]string{
		"package.json":                          `{"name":"root","private":true}`,
		"controller/package.json":               `{"devDependencies":{"prettier":"^3.9.4"}}`,
		"controller/node_modules/.bin/prettier": "#!/bin/sh\n",
		"controller/src/a.ts":                   "export const a = 1\n",
		"README.md":                             "# root\n",
	})

	got := resolveLintTools(root, []string{"controller/src/a.ts", "README.md"}, noLookPath)
	if len(got) != 1 {
		t.Fatalf("plans = %+v, want one prettier plan", got)
	}
	plan := got[0]
	if plan.Tool.Name != "prettier" || plan.Dir != "controller" || plan.ConfiguredBy != "controller/package.json" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.Binary != "controller/node_modules/.bin/prettier" || plan.missing() {
		t.Fatalf("binary = %q, want the sub-package's own prettier", plan.Binary)
	}
	if plan.binaryFromDir() != "./node_modules/.bin/prettier" {
		t.Fatalf("binaryFromDir = %q", plan.binaryFromDir())
	}
	if !reflect.DeepEqual(plan.Files, []string{"src/a.ts"}) {
		t.Fatalf("files = %v, want paths relative to controller/", plan.Files)
	}
}

func TestResolveLintTools_TwoPackagesEachRunTheirOwnTool(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeLintFixture(t, root, map[string]string{
		"package.json":                   `{"name":"root"}`,
		"web/package.json":               `{"devDependencies":{"eslint":"^9"}}`,
		"web/node_modules/.bin/eslint":   "#!/bin/sh\n",
		"web/app.tsx":                    "x\n",
		"api/.prettierrc":                "{}\n",
		"api/package.json":               `{"devDependencies":{"prettier":"^3"}}`,
		"api/node_modules/.bin/prettier": "#!/bin/sh\n",
		"api/server.ts":                  "x\n",
	})

	got := resolveLintTools(root, []string{"web/app.tsx", "api/server.ts"}, noLookPath)
	if len(got) != 2 {
		t.Fatalf("plans = %+v, want eslint in web and prettier in api", got)
	}
	if got[0].Tool.Name != "eslint" || got[0].Dir != "web" || got[0].Binary != "web/node_modules/.bin/eslint" || !reflect.DeepEqual(got[0].Files, []string{"app.tsx"}) {
		t.Fatalf("eslint plan = %+v", got[0])
	}
	if got[1].Tool.Name != "prettier" || got[1].Dir != "api" || got[1].Binary != "api/node_modules/.bin/prettier" || !reflect.DeepEqual(got[1].Files, []string{"server.ts"}) {
		t.Fatalf("prettier plan = %+v", got[1])
	}
}

func TestResolveLintTools_RootFallbackAndHoistedBinary(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeLintFixture(t, root, map[string]string{
		"package.json":               `{"devDependencies":{"prettier":"^3"}}`,
		"node_modules/.bin/prettier": "#!/bin/sh\n",
		"pkg/package.json":           `{"devDependencies":{"eslint":"^9"}}`,
		"node_modules/.bin/eslint":   "#!/bin/sh\n",
		"pkg/a.js":                   "x\n",
	})

	got := resolveLintTools(root, []string{"pkg/a.js"}, noLookPath)
	if len(got) != 2 {
		t.Fatalf("plans = %+v", got)
	}
	// eslint is configured by pkg/ and hoisted to the root's node_modules.
	if got[0].Tool.Name != "eslint" || got[0].Dir != "pkg" || got[0].Binary != "node_modules/.bin/eslint" || got[0].binaryFromDir() != "../node_modules/.bin/eslint" {
		t.Fatalf("eslint plan = %+v", got[0])
	}
	// prettier is configured only by the root, so it runs from the root.
	if got[1].Tool.Name != "prettier" || got[1].Dir != "." || !reflect.DeepEqual(got[1].Files, []string{"pkg/a.js"}) {
		t.Fatalf("prettier plan = %+v", got[1])
	}
}

func TestResolveLintTools_GenuinelyMissingTool(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeLintFixture(t, root, map[string]string{
		"controller/package.json": `{"devDependencies":{"prettier":"^3"}}`,
		"controller/a.ts":         "x\n",
	})
	got := resolveLintTools(root, []string{"controller/a.ts"}, noLookPath)
	if len(got) != 1 || !got[0].missing() {
		t.Fatalf("plans = %+v, want one missing prettier", got)
	}
	got = resolveLintTools(root, []string{"controller/a.ts"}, func(name string) string { return "/opt/bin/" + name })
	if len(got) != 1 || got[0].Binary != "/opt/bin/prettier" {
		t.Fatalf("plans = %+v, want the PATH fallback", got)
	}
}

// lintMonorepo commits a pnpm-style monorepo fixture on the feature branch:
// Prettier is pinned by controller/ only, and installed (untracked, ignored)
// in controller/node_modules. The fake prettier records its directory and
// arguments.
func lintMonorepo(t *testing.T, installPrettier bool) (dir, baseSHA, headSHA, record string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell binaries")
	}
	dir, baseSHA, _ = setupGitRepo(t)
	writeLintFixture(t, dir, map[string]string{
		".gitignore":              "node_modules/\n",
		"package.json":            `{"name":"root","private":true}`,
		"controller/package.json": `{"name":"controller","devDependencies":{"prettier":"^3.9.4"}}`,
		"controller/src/a.ts":     "export const a = 1\n",
	})
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "monorepo")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	record = filepath.Join(t.TempDir(), "prettier.log")
	if installPrettier {
		writeLintFixture(t, dir, map[string]string{
			"controller/node_modules/.bin/prettier": "#!/bin/sh\npwd > \"" + record + "\"\necho \"$@\" >> \"" + record + "\"\necho 'All matched files use Prettier code style!'\n",
		})
	}
	return dir, baseSHA, headSHA, record
}

// hermeticPath is a PATH with git and sh only, so a prettier installed on the
// host cannot satisfy a missing-tool test.
func hermeticPath(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	for _, name := range []string{"git", "sh"} {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	return "PATH=" + bin
}

const falseMissingPrettier = `{"findings":[{"id":"lint-missing-prettier","severity":"warning","action":"ask-user","description":"controller/package.json configures Prettier, but ` + "`pnpm exec prettier --check`" + ` failed because Prettier is unavailable. Provide it on PATH or at a repository-local path."}],"summary":"prettier unavailable"}`

func TestLintStep_SubPackagePrettierIsRunFromItsPackageNotReportedMissing(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA, record := lintMonorepo(t, true)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(falseMissingPrettier)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = []string{hermeticPath(t)}

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "prettier (configured by controller/package.json): run from `controller` with `./node_modules/.bin/prettier`; files: package.json, src/a.ts") {
		t.Fatalf("lint prompt does not name the sub-package's prettier:\n%s", prompt)
	}
	if outcome.NeedsApproval || strings.Contains(outcome.Findings, "lint-missing-prettier") {
		t.Fatalf("outcome = %+v, want the false missing-tool claim dropped", outcome)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("prettier was not run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	wantDir, _ := filepath.EvalSymlinks(filepath.Join(dir, "controller"))
	gotDir, _ := filepath.EvalSymlinks(lines[0])
	if gotDir != wantDir || lines[1] != "--check --ignore-unknown package.json src/a.ts" {
		t.Fatalf("prettier ran in %q with %q, want %q with --check over the changed files under controller/", lines[0], lines[1], wantDir)
	}
}

func TestLintStep_HousekeepingFalseMissingClaimIsChecked(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA, _ := lintMonorepo(t, true)
	sctx := newHousekeepingContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = []string{hermeticPath(t)}
	var stash Findings
	if err := json.Unmarshal([]byte(falseMissingPrettier), &stash); err != nil {
		t.Fatal(err)
	}
	stash.Items[0].Category = types.FindingCategoryLint
	stashJSON, _ := types.MarshalFindingsJSON(stash)
	sctx.Shared.SetHousekeepingLint(pipeline.HousekeepingLintResult{FindingsJSON: stashJSON, Summary: stash.Summary})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || strings.Contains(outcome.Findings, "lint-missing-prettier") {
		t.Fatalf("outcome = %+v, want the housekeeping false claim dropped", outcome)
	}
}

func TestLintStep_RuleFindingWithMissingToolLanguageStaysBlocked(t *testing.T) {
	t.Parallel()
	for _, housekeeping := range []bool{false, true} {
		name := "agent"
		if housekeeping {
			name = "housekeeping"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, _ := setupGitRepo(t)
			writeLintFixture(t, dir, map[string]string{
				".gitignore":       "node_modules/\n",
				"web/package.json": `{"devDependencies":{"eslint":"^9"}}`,
				"web/app.js":       "const x = 1\n",
			})
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "-m", "eslint package")
			headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
			writeLintFixture(t, dir, map[string]string{"web/node_modules/.bin/eslint": "#!/bin/sh\nexit 0\n"})
			want := Finding{
				ID: "eslint-missing-rule", Severity: types.FindingSeverityWarning,
				Action: types.ActionAskUser, Category: types.FindingCategoryLint,
				Description: "eslint rule not found",
			}
			payload, err := types.MarshalFindingsJSON(Findings{Items: []Finding{want}, Summary: "missing required rule"})
			if err != nil {
				t.Fatal(err)
			}
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(payload)}, nil
			}}
			sctx := newHousekeepingContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = []string{hermeticPath(t)}
			if housekeeping {
				sctx.Shared.SetHousekeepingLint(pipeline.HousekeepingLintResult{FindingsJSON: payload})
			}

			outcome, err := (&LintStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			got, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.NeedsApproval || !reflect.DeepEqual(got.Items, []Finding{want}) {
				t.Fatalf("outcome = %+v, findings = %+v, want the unchanged rule finding to park lint", outcome, got.Items)
			}
		})
	}
}

func TestLintStep_GenuinelyMissingToolStillReported(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA, _ := lintMonorepo(t, false)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(falseMissingPrettier)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = []string{hermeticPath(t)}

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ag.calls[0].Prompt, "prettier (configured by controller/package.json): not installed") {
		t.Fatalf("prompt does not report the missing tool:\n%s", ag.calls[0].Prompt)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || len(findings.Items) != 1 {
		t.Fatalf("outcome = %+v, want one missing-tool finding", outcome)
	}
	got := findings.Items[0]
	if got.ID != "lint-missing-prettier@controller" || got.Category != types.FindingCategoryLintEnvironment || got.Action != types.ActionAskUser {
		t.Fatalf("finding = %+v, want an ask-user environment finding", got)
	}
}

func TestLintStep_FailingCheckOfPresentToolBecomesAutoFixFinding(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA, _ := lintMonorepo(t, false)
	writeLintFixture(t, dir, map[string]string{
		"controller/node_modules/.bin/prettier": "#!/bin/sh\necho '[warn] src/a.ts'\nexit 1\n",
	})
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(falseMissingPrettier)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = []string{hermeticPath(t)}

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, _ := types.ParseFindingsJSON(outcome.Findings)
	if len(findings.Items) != 1 || findings.Items[0].ID != "lint-prettier-check@controller" || findings.Items[0].Action != types.ActionAutoFix || !strings.Contains(findings.Items[0].Description, "[warn] src/a.ts") {
		t.Fatalf("findings = %+v, want prettier's own failure", findings.Items)
	}
}

func TestLintStep_EnvironmentOnlyFixRoundRechecksWithoutAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA, _ := lintMonorepo(t, false)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"lint clean"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = []string{hermeticPath(t)}
	previous, _ := types.MarshalFindingsJSON(Findings{Items: []Finding{{
		ID: "lint-missing-prettier@controller", Severity: "warning", Action: types.ActionAskUser,
		Category: types.FindingCategoryLintEnvironment, Description: "prettier is not installed",
	}}})
	sctx.Fixing = true
	sctx.PreviousFindings = previous

	// Still missing: the gate re-parks on the same finding, no agent runs and
	// the round does not fail.
	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("environment-only fix round failed: %v", err)
	}
	if calls != 0 || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "lint-missing-prettier") {
		t.Fatalf("calls=%d outcome=%+v, want a re-park without an agent pass", calls, outcome)
	}

	// The environment is fixed: the next fix round runs the lint pass and
	// the finding clears.
	writeLintFixture(t, dir, map[string]string{"controller/node_modules/.bin/prettier": "#!/bin/sh\nexit 0\n"})
	outcome, err = (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || outcome.NeedsApproval || strings.Contains(outcome.Findings, "lint-missing-prettier") {
		t.Fatalf("calls=%d outcome=%+v, want the lint pass to run and pass", calls, outcome)
	}
}

func TestStrictApproval_LintEnvironmentFindingPointsAtFixButCodeFindingStaysBlocked(t *testing.T) {
	t.Parallel()
	row := func(items ...Finding) *db.StepResult {
		raw, _ := types.MarshalFindingsJSON(Findings{Items: items})
		return &db.StepResult{StepName: types.StepLint, FindingsJSON: &raw}
	}
	env := Finding{ID: "lint-missing-prettier", Severity: "warning", Action: types.ActionAskUser, Category: types.FindingCategoryLintEnvironment, Description: "missing"}
	code := Finding{ID: "lint-1", Severity: "warning", Action: types.ActionAutoFix, Category: types.FindingCategoryLint, Description: "unformatted"}

	if got := row(env).StrictApprovalRefusal(""); got != db.StrictLintEnvironmentRefusal || !strings.Contains(got, "--action fix") {
		t.Fatalf("environment refusal = %q, want the fix way forward", got)
	}
	for _, r := range []*db.StepResult{row(code), row(env, code)} {
		if got := r.StrictApprovalRefusal(""); !strings.Contains(got, "cannot approve unresolved actionable findings") {
			t.Fatalf("code refusal = %q, want real findings still blocked", got)
		}
	}
}

func TestHousekeepingLintDutyNamesResolvedSubPackageTool(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA, _ := lintMonorepo(t, true)
	sctx := newHousekeepingContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = []string{hermeticPath(t)}
	section := lintDutySection(sctx, baseSHA, true)
	if !strings.Contains(section, "Combined lint duty") || !strings.Contains(section, "run from `controller` with `./node_modules/.bin/prettier`") {
		t.Fatalf("housekeeping lint duty does not name the sub-package's prettier:\n%s", section)
	}
	if lintDutySection(sctx, baseSHA, false) != "" {
		t.Fatal("a configured lint command must not add the combined lint duty")
	}
}

func TestResolveLintTools_ResolvesEveryChangedFileSoConfiguredTypesAreNotLost(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeLintFixture(t, root, map[string]string{
		"web/package.json":      `{"devDependencies":{"eslint":"^9"}}`,
		"web/eslint.config.mjs": "export default []\n",
		"web/App.vue":           "<template/>\n",
	})
	got := resolveLintTools(root, []string{"web/App.vue"}, noLookPath)
	if len(got) != 1 || got[0].Tool.Name != "eslint" || got[0].Dir != "web" || !reflect.DeepEqual(got[0].Files, []string{"App.vue"}) {
		t.Fatalf("plans = %+v, want eslint planned for the .vue file its configuration may support", got)
	}
	if files := got[0].checkFiles(); len(files) != 0 {
		t.Fatalf("native check files = %v, want none for a non-JS extension", files)
	}
}

func TestLintPromptListsEveryPlannedFile(t *testing.T) {
	t.Parallel()
	var files []string
	for i := 0; i < 30; i++ {
		files = append(files, "f"+string(rune('a'+i%26))+strings.Repeat("x", i)+".ts")
	}
	section := lintToolsPromptSection([]lintToolPlan{{Tool: knownLintTools[0], Dir: "pkg", ConfiguredBy: "pkg/package.json", Binary: "pkg/node_modules/.bin/prettier", Files: files}})
	if !strings.Contains(section, files[29]) || strings.Contains(section, "more)") {
		t.Fatalf("prompt truncated the file list:\n%s", section)
	}
}

func TestLintStep_AgentFindingsSharingAnIDAreBothKept(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA, _ := lintMonorepo(t, true)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"l1","severity":"info","action":"no-op","description":"note"},{"id":"l1","severity":"warning","action":"ask-user","description":"real lint error"}],"summary":"two findings"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = []string{hermeticPath(t)}

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "real lint error") {
		t.Fatalf("outcome = %+v, want the blocking finding kept", outcome)
	}
}

func TestLintStep_MissingPackageIsNotMaskedByAnotherPackagesTool(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	writeLintFixture(t, dir, map[string]string{
		".gitignore":     "node_modules/\n",
		"a/package.json": `{"devDependencies":{"prettier":"^3"}}`,
		"a/x.ts":         "x\n",
		"b/package.json": `{"devDependencies":{"prettier":"^3"}}`,
		"b/y.ts":         "y\n",
	})
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "two packages")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	writeLintFixture(t, dir, map[string]string{"a/node_modules/.bin/prettier": "#!/bin/sh\nexit 0\n"})
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"lint-missing-prettier","severity":"warning","action":"ask-user","description":"prettier is unavailable"}],"summary":"missing"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = []string{hermeticPath(t)}

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, _ := types.ParseFindingsJSON(outcome.Findings)
	if len(findings.Items) != 1 || findings.Items[0].ID != "lint-missing-prettier@b" {
		t.Fatalf("findings = %+v, want only b's prettier reported missing", findings.Items)
	}

	sctx.Fixing = true
	sctx.PreviousFindings = outcome.Findings
	outcome, err = (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "lint-missing-prettier@b") {
		t.Fatalf("calls=%d outcome=%+v, want a re-park without an agent pass while b still lacks prettier", calls, outcome)
	}
}

func TestLintStep_PresentToolChecksConfiguredFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell binaries")
	}
	t.Parallel()
	for _, path := range []string{"agent", "housekeeping"} {
		for _, exit := range []string{"0", "1"} {
			t.Run(path+"/exit-"+exit, func(t *testing.T) {
				t.Parallel()
				dir, baseSHA, _ := setupGitRepo(t)
				writeLintFixture(t, dir, map[string]string{
					".gitignore":       "node_modules/\n",
					"web/package.json": `{"devDependencies":{"eslint":"^9"}}`,
					"web/App.vue":      "<template/>\n",
				})
				gitCmd(t, dir, "add", "-A")
				gitCmd(t, dir, "commit", "-m", "vue")
				headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
				writeLintFixture(t, dir, map[string]string{
					"web/node_modules/.bin/eslint": "#!/bin/sh\nprintf '%s\\n' \"$@\" > node_modules/.bin/checked-files\necho 'eslint checked configured files'\nexit " + exit + "\n",
				})
				payload := `{"findings":[{"id":"lint-missing-eslint","severity":"warning","action":"ask-user","description":"eslint is unavailable"}],"summary":"missing"}`
				ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(payload)}, nil
				}}
				sctx := newHousekeepingContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
				sctx.Env = []string{hermeticPath(t)}
				if path == "housekeeping" {
					sctx.Shared.SetHousekeepingLint(pipeline.HousekeepingLintResult{FindingsJSON: payload})
				}

				outcome, err := (&LintStep{}).Execute(sctx)
				if err != nil {
					t.Fatal(err)
				}
				checked, err := os.ReadFile(filepath.Join(dir, "web", "node_modules", ".bin", "checked-files"))
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(strings.Fields(string(checked)), []string{"App.vue", "package.json"}) {
					t.Fatalf("checked files = %q, want all configured files", checked)
				}
				findings, err := types.ParseFindingsJSON(outcome.Findings)
				if err != nil {
					t.Fatal(err)
				}
				if exit == "0" {
					if outcome.NeedsApproval || len(findings.Items) != 0 {
						t.Fatalf("outcome = %+v, want the verified false claim dropped", outcome)
					}
				} else {
					if !outcome.NeedsApproval || len(findings.Items) != 1 {
						t.Fatalf("outcome = %+v, want the failed check to park", outcome)
					}
					got := findings.Items[0]
					if got.ID != "lint-eslint-check@web" || got.Category != types.FindingCategoryLint || got.Action != types.ActionAutoFix || !strings.Contains(got.Description, "eslint checked configured files") {
						t.Fatalf("finding = %+v, want the tool's auto-fix check finding", got)
					}
				}
			})
		}
	}
}

func TestLintToolCheck_TimeoutRetainsDiagnosticsWithoutReportingMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell binaries")
	}
	t.Parallel()
	dir := t.TempDir()
	writeLintFixture(t, dir, map[string]string{
		"web/package.json":             `{"devDependencies":{"eslint":"^9"}}`,
		"web/App.vue":                  "<template/>\n",
		"web/node_modules/.bin/eslint": "#!/bin/sh\necho 'checking App.vue'\nsleep 60\n",
	})
	sctx := &pipeline.StepContext{Ctx: context.Background(), WorkDir: dir}
	plans := resolveLintTools(dir, []string{"web/App.vue"}, noLookPath)
	findings, err := reconcileLintToolFindings(sctx, Findings{Items: []Finding{{ID: "lint-missing-eslint"}}}, plans, execLintCheck(sctx, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %+v, want one failed verification", findings.Items)
	}
	got := findings.Items[0]
	if got.ID != "lint-eslint-check@web" || got.Category != types.FindingCategoryLint || got.Action != types.ActionAskUser || got.Severity != types.FindingSeverityWarning {
		t.Fatalf("finding = %+v, want an ask-user lint warning", got)
	}
	for _, detail := range []string{"cd web && ./node_modules/.bin/eslint", "timed out after 1s", "checking App.vue"} {
		if !strings.Contains(got.Description, detail) {
			t.Fatalf("description = %q, want %q", got.Description, detail)
		}
	}
}

func TestLintToolCheck_OnlyAbsentExecutablesAreEnvironmentFindings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		err         error
		environment bool
	}{
		{"not-on-path", exec.ErrNotFound, true},
		{"not-on-disk", &os.PathError{Op: "fork/exec", Path: "eslint", Err: fs.ErrNotExist}, true},
		{"permission", &os.PathError{Op: "fork/exec", Path: "eslint", Err: fs.ErrPermission}, false},
		{"output-failure", errors.New("could not collect output"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx := &pipeline.StepContext{Ctx: context.Background(), WorkDir: t.TempDir()}
			plan := lintToolPlan{Tool: knownLintTools[1], Dir: "web", Binary: "web/node_modules/.bin/eslint", Files: []string{"App.vue"}}
			findings, err := reconcileLintToolFindings(sctx, Findings{Items: []Finding{{ID: "lint-missing-eslint"}}}, []lintToolPlan{plan}, func(context.Context, string, string, []string) (string, int, error) {
				return strings.Repeat("x", 5000) + "\nlast diagnostic", -1, tc.err
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != 1 {
				t.Fatalf("findings = %+v, want one warning", findings.Items)
			}
			got := findings.Items[0]
			if got.Action != types.ActionAskUser || got.Severity != types.FindingSeverityWarning || !strings.Contains(got.Description, tc.err.Error()) {
				t.Fatalf("finding = %+v, want an ask-user warning naming the error", got)
			}
			if tc.environment {
				if got.ID != "lint-missing-eslint@web" || got.Category != types.FindingCategoryLintEnvironment {
					t.Fatalf("finding = %+v, want a missing-tool environment finding", got)
				}
			} else if got.ID != "lint-eslint-check@web" || got.Category != types.FindingCategoryLint || !strings.Contains(got.Description, "cd web && ./node_modules/.bin/eslint") || !strings.HasSuffix(got.Description, "last diagnostic") || strings.Contains(got.Description, strings.Repeat("x", 4001)) {
				t.Fatalf("finding = %+v, want a lint check warning with bounded output tail", got)
			}
		})
	}
}

func TestLintStep_CancelledVerificationReturnsAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell binaries")
	}
	t.Parallel()
	for _, path := range []string{"agent", "housekeeping"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA, _ := lintMonorepo(t, false)
			writeLintFixture(t, dir, map[string]string{
				"controller/node_modules/.bin/prettier": "#!/bin/sh\necho started > node_modules/.bin/started\nsleep 60\n",
			})
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(falseMissingPrettier)}, nil
			}}
			sctx := newHousekeepingContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
			if path == "housekeeping" {
				sctx.Shared.SetHousekeepingLint(pipeline.HousekeepingLintResult{FindingsJSON: falseMissingPrettier})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sctx.Ctx = ctx
			go func() {
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if _, err := os.Stat(filepath.Join(dir, "controller", "node_modules", ".bin", "started")); err == nil {
							cancel()
							return
						}
					}
				}
			}()
			outcome, err := (&LintStep{}).Execute(sctx)
			if !errors.Is(err, context.Canceled) || outcome != nil {
				t.Fatalf("outcome = %+v, error = %v, want cancellation without findings", outcome, err)
			}
		})
	}
}
