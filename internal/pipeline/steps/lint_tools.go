package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// lintTool is a JavaScript lint or format tool the Lint step can resolve
// from the package that configures it. In a monorepo the tool is often
// pinned by a sub-package (controller/package.json) and installed only in
// that package's node_modules, so `pnpm exec prettier` from the repository
// root cannot find it (issue #212). Resolving it here, the way Node does,
// lets the agent run the repository's own binary from the right directory
// and lets the step check a "tool is missing" claim instead of trusting it.
type lintTool struct {
	Name        string
	Bin         string
	Packages    []string
	PackageKeys []string
	ConfigFiles []string
	// CheckArgs is a read-only invocation; the changed files follow it.
	CheckArgs  []string
	Extensions []string
}

var knownLintTools = []lintTool{
	{
		Name: "prettier", Bin: "prettier",
		Packages: []string{"prettier"}, PackageKeys: []string{"prettier"},
		ConfigFiles: []string{".prettierrc", ".prettierrc.json", ".prettierrc.yaml", ".prettierrc.yml", ".prettierrc.json5", ".prettierrc.js", ".prettierrc.cjs", ".prettierrc.mjs", ".prettierrc.ts", ".prettierrc.toml", "prettier.config.js", "prettier.config.cjs", "prettier.config.mjs", "prettier.config.ts"},
		CheckArgs:   []string{"--check", "--ignore-unknown"},
	},
	{
		Name: "eslint", Bin: "eslint",
		Packages: []string{"eslint"}, PackageKeys: []string{"eslintConfig"},
		ConfigFiles: []string{"eslint.config.js", "eslint.config.mjs", "eslint.config.cjs", "eslint.config.ts", "eslint.config.mts", "eslint.config.cts", ".eslintrc", ".eslintrc.js", ".eslintrc.cjs", ".eslintrc.json", ".eslintrc.yaml", ".eslintrc.yml"},
		Extensions:  []string{".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts"},
	},
	{
		Name: "biome", Bin: "biome",
		Packages:    []string{"@biomejs/biome"},
		ConfigFiles: []string{"biome.json", "biome.jsonc"},
		CheckArgs:   []string{"check", "--no-errors-on-unmatched", "--files-ignore-unknown=true"},
	},
	{
		Name: "stylelint", Bin: "stylelint",
		Packages: []string{"stylelint"}, PackageKeys: []string{"stylelint"},
		ConfigFiles: []string{".stylelintrc", ".stylelintrc.json", ".stylelintrc.yaml", ".stylelintrc.yml", ".stylelintrc.js", ".stylelintrc.cjs", ".stylelintrc.mjs", "stylelint.config.js", "stylelint.config.cjs", "stylelint.config.mjs"},
		CheckArgs:   []string{"--allow-empty-input"},
		Extensions:  []string{".css", ".scss", ".sass", ".less"},
	},
}

// lintMissingIDPrefix is the finding id prefix of a lint tool that is not
// available in the run's environment. The step's own ids add "@<dir>" for a
// tool configured below the repository root, so each package's finding is
// distinct and names where availability must be re-checked.
const lintMissingIDPrefix = "lint-missing-"

// lintPlanID is the id of a step-generated finding for one tool in one
// package directory.
func lintPlanID(base, dir string) string {
	if dir == "." {
		return base
	}
	return base + "@" + dir
}

// lintToolPlan is one tool resolved for the changed files of one package.
// Paths are slash-separated and relative to the repository root, except
// Files, which are relative to Dir so the tool can run from Dir.
type lintToolPlan struct {
	Tool         lintTool
	Dir          string
	ConfiguredBy string
	// Binary is the resolved executable: a repository-relative
	// node_modules/.bin path, or an absolute PATH entry. Empty when the tool
	// is not installed anywhere the configuring package can reach.
	Binary string
	Files  []string
}

func (p lintToolPlan) missing() bool { return p.Binary == "" }

// binaryFromDir is the binary as it is invoked from the package directory.
func (p lintToolPlan) binaryFromDir() string {
	if p.Binary == "" || filepath.IsAbs(p.Binary) {
		return p.Binary
	}
	rel, err := filepath.Rel(filepath.FromSlash(p.Dir), filepath.FromSlash(p.Binary))
	if err != nil {
		return p.Binary
	}
	rel = filepath.ToSlash(rel)
	if !strings.HasPrefix(rel, ".") {
		rel = "./" + rel
	}
	return rel
}

// resolveLintTools assigns each changed file, for each known tool, to the
// nearest ancestor directory (up to the repository root) whose package.json
// or tool config file configures that tool, then resolves the tool's binary
// from that directory's node_modules/.bin upward to the root, falling back to
// lookPath. The search reads only the changed files' ancestor directories, so
// it never walks the tree.
func resolveLintTools(workDir string, changed []string, lookPath func(string) string) []lintToolPlan {
	type key struct{ tool, dir string }
	cache := map[string]map[string]string{}
	configured := func(dir string) map[string]string {
		if found, ok := cache[dir]; ok {
			return found
		}
		found := directoryLintTools(filepath.Join(workDir, filepath.FromSlash(dir)), dir)
		cache[dir] = found
		return found
	}
	plans := map[key]*lintToolPlan{}
	for _, file := range changed {
		file = path.Clean(filepath.ToSlash(file))
		if info, err := os.Stat(filepath.Join(workDir, filepath.FromSlash(file))); err != nil || info.IsDir() {
			continue
		}
		for _, tool := range knownLintTools {
			for dir := path.Dir(file); ; dir = path.Dir(dir) {
				if by, ok := configured(dir)[tool.Name]; ok {
					k := key{tool.Name, dir}
					plan := plans[k]
					if plan == nil {
						plan = &lintToolPlan{Tool: tool, Dir: dir, ConfiguredBy: by, Binary: resolveLintBinary(workDir, dir, tool.Bin, lookPath)}
						plans[k] = plan
					}
					rel := file
					if dir != "." {
						rel = strings.TrimPrefix(file, dir+"/")
					}
					plan.Files = append(plan.Files, rel)
					break
				}
				if dir == "." {
					break
				}
			}
		}
	}
	out := make([]lintToolPlan, 0, len(plans))
	for _, plan := range plans {
		out = append(out, *plan)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tool.Name != out[j].Tool.Name {
			return out[i].Tool.Name < out[j].Tool.Name
		}
		return out[i].Dir < out[j].Dir
	})
	return out
}

// directoryLintTools reports which known tools the directory configures,
// mapped to the repository-relative file that configures each.
func directoryLintTools(abs, rel string) map[string]string {
	found := map[string]string{}
	join := func(name string) string {
		if rel == "." {
			return name
		}
		return rel + "/" + name
	}
	var manifest map[string]json.RawMessage
	if raw, err := os.ReadFile(filepath.Join(abs, "package.json")); err == nil {
		_ = json.Unmarshal(raw, &manifest)
	}
	deps := map[string]bool{}
	for _, field := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
		var names map[string]json.RawMessage
		if raw, ok := manifest[field]; ok && json.Unmarshal(raw, &names) == nil {
			for name := range names {
				deps[name] = true
			}
		}
	}
	for _, tool := range knownLintTools {
		for _, name := range tool.Packages {
			if deps[name] {
				found[tool.Name] = join("package.json")
			}
		}
		for _, k := range tool.PackageKeys {
			if _, ok := manifest[k]; ok {
				found[tool.Name] = join("package.json")
			}
		}
		if _, ok := found[tool.Name]; ok {
			continue
		}
		for _, name := range tool.ConfigFiles {
			if info, err := os.Stat(filepath.Join(abs, name)); err == nil && !info.IsDir() {
				found[tool.Name] = join(name)
				break
			}
		}
	}
	return found
}

func resolveLintBinary(workDir, dir, bin string, lookPath func(string) string) string {
	names := []string{bin}
	if runtime.GOOS == "windows" {
		// Package managers install a .cmd shim beside the POSIX script.
		names = []string{bin + ".cmd", bin}
	}
	for d := dir; ; d = path.Dir(d) {
		for _, name := range names {
			candidate := path.Join(d, "node_modules", ".bin", name)
			if info, err := os.Stat(filepath.Join(workDir, filepath.FromSlash(candidate))); err == nil && !info.IsDir() {
				return candidate
			}
		}
		if d == "." {
			break
		}
	}
	if lookPath != nil {
		return lookPath(bin)
	}
	return ""
}

// lintToolsForStep resolves the known tools for the change under review.
func lintToolsForStep(sctx *pipeline.StepContext, baseSHA string) []lintToolPlan {
	out, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-only", "-z", "--no-renames", "--diff-filter=d", baseSHA)
	if err != nil {
		sctx.Log(fmt.Sprintf("warning: could not list changed files for lint tool resolution: %v", err))
		return nil
	}
	if out == "" {
		return nil
	}
	return resolveLintTools(sctx.WorkDir, changedPathList(out), stepLookPath(sctx))
}

// stepLookPath resolves a program on the step environment's PATH.
func stepLookPath(sctx *pipeline.StepContext) func(string) string {
	env := stepEnvironment(sctx)
	if env == nil {
		env = os.Environ()
	}
	return func(name string) string { return findInCustomPath(sctx.WorkDir, env, name) }
}

// lintToolsPromptSection tells the agent which repository-local binary to run
// from which directory. The worktree steering forbids hunting the machine for
// a tool, so without these paths an agent at the repository root reports a
// sub-package's tool as missing.
func lintToolsPromptSection(plans []lintToolPlan) string {
	if len(plans) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nResolved lint tools (no-mistakes resolved each tool from the package that configures it; run each from its package directory, not the repository root, over its listed files, whose paths are relative to that directory):")
	for _, plan := range plans {
		files := sanitizePromptText(strings.Join(plan.Files, ", "))
		if plan.missing() {
			fmt.Fprintf(&b, "\n- %s (configured by %s): not installed in %s/node_modules/.bin or any parent package, and not on PATH; files: %s. If these files need it, report it with id %q and action \"ask-user\".", plan.Tool.Name, sanitizePromptText(plan.ConfiguredBy), sanitizePromptText(plan.Dir), files, sanitizePromptText(lintPlanID(lintMissingIDPrefix+plan.Tool.Name, plan.Dir)))
			continue
		}
		fmt.Fprintf(&b, "\n- %s (configured by %s): run from `%s` with `%s`; files: %s", plan.Tool.Name, sanitizePromptText(plan.ConfiguredBy), sanitizePromptText(plan.Dir), sanitizePromptText(plan.binaryFromDir()), files)
	}
	b.WriteString("\n- A tool listed with a binary is installed: never report it as missing or unavailable. If another tool you need is unavailable, report it with id \"" + lintMissingIDPrefix + "<tool>\" and action \"ask-user\".\n")
	return b.String()
}

// isLintEnvironmentFinding reports a finding about the run's environment (a
// lint tool that is unavailable), as opposed to the code.
func isLintEnvironmentFinding(f Finding) bool {
	return f.Category == types.FindingCategoryLintEnvironment
}

// lintMissingToolClaim reports the tool a finding claims is unavailable,
// and the package directory its id names ("." when it names none).
func lintMissingToolClaim(f Finding) (tool, dir string, ok bool) {
	raw := strings.TrimSpace(f.ID)
	id := strings.ToLower(raw)
	if strings.HasPrefix(id, lintMissingIDPrefix) {
		tool, dir, _ = strings.Cut(raw[len(lintMissingIDPrefix):], "@")
		if dir == "" {
			dir = "."
		}
		return strings.ToLower(tool), dir, true
	}
	return "", "", false
}

// lintCheckRunner runs a resolved tool's read-only check from dir. It returns
// the combined output and exit code. A normal non-zero exit is returned without
// an error; startup, cancellation, timeout, and output-collection failures return
// an error with exit code -1.
type lintCheckRunner func(ctx context.Context, dir, binary string, args []string) (string, int, error)

const lintCheckTimeout = 5 * time.Minute

func lintWindowsShimCommandLine(binary string, args []string) (string, error) {
	quoted := append([]string{binary}, args...)
	for i, arg := range quoted {
		if strings.ContainsAny(arg, "&|<>^%!\"()\r\n") {
			return "", fmt.Errorf("could not safely verify %q on Windows because file path %q contains shell metacharacters; run the check by hand", binary, arg)
		}
		quoted[i] = `"` + arg + `"`
	}
	return `cmd.exe /d /s /c "` + strings.Join(quoted, " ") + `"`, nil
}

func execLintCheck(sctx *pipeline.StepContext, timeout time.Duration) lintCheckRunner {
	return func(ctx context.Context, dir, binary string, args []string) (string, int, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd, err := lintCheckCommand(ctx, binary, args)
		if err != nil {
			return "", -1, err
		}
		cmd.Dir = dir
		cmd.Env = stepEnvironment(sctx)
		shellenv.ConfigureShellCommand(cmd)
		output, err := shellenv.CombinedOutputShellCommand(cmd)
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return string(output), -1, fmt.Errorf("timed out after %s: %w", timeout, ctx.Err())
			}
			return string(output), -1, ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return string(output), exitErr.ExitCode(), nil
		}
		if err != nil {
			return string(output), -1, err
		}
		return string(output), 0, nil
	}
}

// reconcileLintToolFindings checks every missing-tool claim against the
// resolved tools. A claim about a tool the configuring package has installed
// is replaced by that tool's own check, run from its package directory: a
// passing check drops the claim, a failing one becomes an auto-fix finding.
// A claim about a tool that really is unavailable becomes one environment
// finding with a stable id, so strict shipping can route it (see
// db.StepResult.StrictApprovalRefusal). Every other finding is untouched.
func reconcileLintToolFindings(sctx *pipeline.StepContext, findings Findings, plans []lintToolPlan, run lintCheckRunner) (Findings, error) {
	if err := sctx.Ctx.Err(); err != nil {
		return Findings{}, err
	}
	byTool := map[string][]lintToolPlan{}
	for _, plan := range plans {
		byTool[plan.Tool.Name] = append(byTool[plan.Tool.Name], plan)
	}
	out := Findings{Items: []Finding{}, Summary: findings.Summary}
	// Only the step's own findings are deduplicated: each is keyed by tool
	// and package, so two claims about one tool yield one check. The
	// agent's findings pass through as reported; duplicate ids among them
	// are the executor's to normalize, never a reason to drop one.
	checked := map[string]bool{}
	for _, item := range findings.Items {
		tool, _, ok := lintMissingToolClaim(item)
		if !ok {
			out.Items = append(out.Items, item)
			continue
		}
		toolPlans := byTool[tool]
		if len(toolPlans) == 0 {
			// Not a tool the step resolves: keep the agent's report, typed as
			// an environment finding.
			item.Category = types.FindingCategoryLintEnvironment
			if item.Action == "" || item.Action == types.ActionAutoFix {
				item.Action = types.ActionAskUser
			}
			out.Items = append(out.Items, item)
			continue
		}
		for _, plan := range toolPlans {
			key := plan.Tool.Name + "@" + plan.Dir
			if checked[key] {
				continue
			}
			checked[key] = true
			out.Items = append(out.Items, checkLintToolPlan(sctx, plan, run)...)
			if err := sctx.Ctx.Err(); err != nil {
				return Findings{}, err
			}
		}
	}
	return out, sctx.Ctx.Err()
}

func checkLintToolPlan(sctx *pipeline.StepContext, plan lintToolPlan, run lintCheckRunner) []Finding {
	if plan.missing() {
		return []Finding{lintMissingToolFinding(plan, "")}
	}
	files := plan.checkFiles()
	if len(files) == 0 {
		files = plan.Files
	}
	binary := plan.Binary
	if !filepath.IsAbs(binary) {
		binary = filepath.Join(sctx.WorkDir, filepath.FromSlash(binary))
	}
	args := append(append([]string{}, plan.Tool.CheckArgs...), files...)
	output, exitCode, err := run(sctx.Ctx, filepath.Join(sctx.WorkDir, filepath.FromSlash(plan.Dir)), binary, args)
	command := fmt.Sprintf("cd %s && %s %s", plan.Dir, plan.binaryFromDir(), strings.Join(plan.Tool.CheckArgs, " "))
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return []Finding{lintMissingToolFinding(plan, fmt.Sprintf("%s could not be found at %s: %v", plan.Tool.Name, plan.Binary, err))}
		}
		return []Finding{{
			ID:          lintPlanID("lint-"+plan.Tool.Name+"-check", plan.Dir),
			Severity:    types.FindingSeverityWarning,
			Action:      types.ActionAskUser,
			Category:    types.FindingCategoryLint,
			Description: fmt.Sprintf("`%s` over the changed files could not be verified: %v\n%s", command, err, tailText(output, 4000)),
		}}
	}
	if exitCode == 0 {
		sctx.Log(fmt.Sprintf("%s is installed at %s and its check passed from %s over %d changed files; dropping the missing-tool claim", plan.Tool.Name, plan.Binary, plan.Dir, len(files)))
		return nil
	}
	return []Finding{{
		ID:          lintPlanID("lint-"+plan.Tool.Name+"-check", plan.Dir),
		Severity:    types.FindingSeverityWarning,
		Action:      types.ActionAutoFix,
		Category:    types.FindingCategoryLint,
		Description: fmt.Sprintf("`%s` over the changed files exited %d:\n%s", command, exitCode, tailText(output, 4000)),
	}}
}

func lintMissingToolFinding(plan lintToolPlan, detail string) Finding {
	if detail == "" {
		detail = fmt.Sprintf("%s configures %s, but it is not installed in %s/node_modules/.bin or any parent package and is not on PATH", plan.ConfiguredBy, plan.Tool.Name, plan.Dir)
	}
	return Finding{
		ID:          lintPlanID(lintMissingIDPrefix+plan.Tool.Name, plan.Dir),
		Severity:    types.FindingSeverityWarning,
		Action:      types.ActionAskUser,
		Category:    types.FindingCategoryLintEnvironment,
		Description: detail + ". Make it available in the run worktree (for example install the repository's dependencies), then respond with fix to re-check the environment.",
	}
}

func (p lintToolPlan) checkFiles() []string {
	if p.Tool.Extensions == nil {
		return append([]string{}, p.Files...)
	}
	var files []string
	for _, file := range p.Files {
		ext := strings.ToLower(path.Ext(file))
		for _, want := range p.Tool.Extensions {
			if ext == want {
				files = append(files, file)
				break
			}
		}
	}
	return files
}

func tailText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return "..." + text[len(text)-limit:]
}

// lintEnvironmentRecheck handles a fix round whose every selected finding is
// an environment finding. Code changes cannot fix an unavailable tool, so no
// agent runs: the step re-checks each finding's tool in the package directory
// the finding names. The outcome is nil (run the normal lint pass) once one
// of them is available there, or the same findings again so the gate
// re-parks without failing the run. Another package having the tool does not
// count: the package that lacked it has gained nothing.
func lintEnvironmentRecheck(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if !sctx.Fixing || sctx.PreviousFindings == "" {
		return nil, nil
	}
	previous, err := types.ParseFindingsJSON(sctx.PreviousFindings)
	if err != nil || len(previous.Items) == 0 {
		return nil, nil
	}
	for _, item := range previous.Items {
		if !isLintEnvironmentFinding(item) {
			return nil, nil
		}
	}
	lookPath := stepLookPath(sctx)
	for _, item := range previous.Items {
		tool, dir, ok := lintMissingToolClaim(item)
		if !ok || tool == "" {
			continue
		}
		bin := tool
		for _, known := range knownLintTools {
			if known.Name == tool {
				bin = known.Bin
			}
		}
		if resolveLintBinary(sctx.WorkDir, path.Clean(filepath.ToSlash(dir)), bin, lookPath) != "" {
			sctx.Log(fmt.Sprintf("lint tool %s is now available for %s; re-running the lint pass", tool, dir))
			return nil, nil
		}
	}
	sctx.Log("every selected lint finding is an unavailable tool and none is available yet; re-parking without an agent pass")
	payload, err := types.MarshalFindingsJSON(previous)
	if err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: payload, FixSummary: NoChangesAppliedSummary}, nil
}
