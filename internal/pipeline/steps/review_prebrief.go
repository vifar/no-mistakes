package steps

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/jev"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// This file builds the opt-in Jev review pre-brief (issue #1055, global
// config jev.review_assist). One batched TypeSafe System One evaluation runs
// before a review turn and contributes ADVISORY input to the review prompt: a
// ranked list of surrounding-context files to read first. It exists to
// shorten the cold reviewer's unguided search, which is where a cold launch
// spends most of its tokens; it is deliberately not a coverage mechanism.
//
// The invariants that keep this inside VISION R4 and the issue's contract:
//
//   - Complete-change coverage is untouched. The reviewed_paths contract, the
//     prompt's full-pass obligations, and the reviewable set are exactly what
//     they are with the assist off. The pre-brief can only ever ADD reading
//     suggestions; no Jev answer can remove a file, an obligation, or a
//     prompt clause.
//   - One owner per judgment (VISION L31). Jev owns one question nothing
//     else owns: "which surrounding files are most relevant as context"
//     (retrieval ranking; the reviewer remains free to read anything). Every
//     verdict - findings, severity, coverage, risk - stays with the
//     session-free reviewer. Per-file scrutiny scores and candidate-defect
//     leads were considered and rejected: both stack a second mechanism on
//     the reviewer's own verdict question, and acting on them to do less
//     work is the fix-delta-only rereview the maintainer already rejected.
//   - Fail closed. A disabled flag, a missing TYPESAFE_API_KEY, a network or
//     API error, or an undecodable answer each produce an empty pre-brief,
//     which is byte-identical to today's prompt. The assist can never block
//     or weaken a review.
//   - The reviewer stays a fresh, session-free invocation; the pre-brief is
//     prompt text, not a resumed session, and the fixer session is never
//     involved.
//
// Jev answers are typed numbers, not generated text, so nothing the service
// returns can inject prose into the prompt; the section is formatted from
// paths and scores alone.

// jevDiffMaxBytes clips the unified diff inside the Jev state. Jev's state
// budget is 32k tokens and code tokenizes denser than prose, so the digest,
// candidate paths, and questions together must stay well under it; an
// oversized request fails closed to a pre-brief-less review anyway.
const jevDiffMaxBytes = 32 * 1024

// jevStatMaxBytes clips the diff --stat summary in the Jev state.
const jevStatMaxBytes = 4 * 1024

// jevMaxIdentifiers caps how many changed-code identifiers with at least one
// use site feed the candidate context files.
const jevMaxIdentifiers = 8

// jevMaxGreps bounds the git grep runs spent looking for those identifiers,
// since a name the change just introduced often has no use site yet.
const jevMaxGreps = 16

// jevMinIdentifierLen skips names too short to search for: a one- to
// three-letter name (a loop index, a minified symbol) matches as a whole word
// almost everywhere, so it cannot point at a use site.
const jevMinIdentifierLen = 4

// jevMaxCandidates caps the candidate files sent for ranking.
const jevMaxCandidates = 40

// jevMaxListed caps how many ranked files the pre-brief lists.
const jevMaxListed = 10

// jevMaxExcerptTotalBytes caps the summed candidate excerpts in one Jev
// request. Per-candidate bytes come from the operator's
// jev.candidate_excerpt_bytes, but 40 candidates at a generous per-file
// budget would otherwise dwarf the 32k-token state budget the diff digest is
// clipped for; excerpts past this ceiling are dropped from the
// lowest-coupling candidates first, so the request stays bounded no matter
// how many candidates the change yields.
const jevMaxExcerptTotalBytes = 16 * 1024

// jevRelevanceThreshold is the probability mass a candidate must carry at
// "relevant" or "essential" (levels 2 and 3 of jevRelevanceLevels) combined
// to be listed. The score Jev returns is probability-weighted across all four
// levels, so a threshold on the score itself demands near-certainty at level
// 2: a file Jev is quite sure is relevant (60-70% of mass at level 2, the
// rest at level 1) scores 1.6-1.8 and would never list. Mass at or above
// level 2 is the rubric's actual meaning of "worth reading".
const jevRelevanceThreshold = 0.5

// jevConfidenceThreshold drops a ranked candidate whose score confidence is
// too low to act on; failing toward not listing is the pre-assist behavior.
const jevConfidenceThreshold = 0.3

// jevRelevanceLevels is the ordered rubric for per-candidate relevance
// scores. Level wording follows the TypeSafe score guidance: concrete
// situations that stand on their own.
var jevRelevanceLevels = []string{
	"Unrelated to the change: reviewing the change would not benefit from reading this file",
	"Weakly related: shares a package or naming with the change but no evident coupling to its behavior",
	"Relevant context: references or couples to the changed symbols or behavior, so reading it would inform the review",
	"Essential context: the change cannot be judged correctly without understanding this file",
}

// jevClient is the slice of the TypeSafe client the pre-brief needs, so tests
// can inject a fake.
type jevClient interface {
	Evaluate(ctx context.Context, state any, questions map[string]jev.Question) (*jev.Response, error)
}

// jevCandidate is one surrounding-context file offered for ranking. By
// default it is a path only: no content of an unchanged file leaves the
// machine. The opt-in jev.candidate_excerpt_bytes attaches a bounded leading
// slice of the file as excerpt (omitted from the state when empty, so the
// default request is byte-identical to path-only). coupling is the code's
// use-site score (0 for a sibling); it is unexported, so it never reaches
// the Jev state, and only orders the listing.
type jevCandidate struct {
	Path     string `json:"path"`
	Excerpt  string `json:"excerpt,omitempty"`
	coupling float64
}

// jevChangeState is the state one pre-brief evaluation runs over.
type jevChangeState struct {
	Change     jevChangeDigest `json:"change"`
	Candidates []jevCandidate  `json:"candidates"`
}

type jevChangeDigest struct {
	Branch     string `json:"branch"`
	BaseCommit string `json:"base_commit"`
	DiffStat   string `json:"diff_stat"`
	Diff       string `json:"diff"`
}

// reviewPrebriefSection builds the advisory pre-brief for one review turn, or
// "" when the assist is disabled or unavailable. It never returns an error:
// every failure degrades to the same cold, complete review the turn would run
// without the assist, with one log line naming the degradation.
func (s *ReviewStep) reviewPrebriefSection(ctx context.Context, sctx *pipeline.StepContext, baseSHA string, changed, reviewable []string) string {
	if sctx.Config == nil || !sctx.Config.Jev.ReviewAssist {
		return ""
	}
	logf := func(format string, args ...any) {
		if sctx.Log != nil {
			sctx.Log(fmt.Sprintf(format, args...))
		}
	}
	client := s.jev
	if client == nil {
		if c := jev.NewClientFromEnv(); c != nil {
			client = c
		}
	}
	if client == nil {
		logf("jev review assist is enabled but %s is not set; reviewing without a pre-brief", jev.EnvKey)
		return ""
	}

	state, candidates := buildJevReviewState(ctx, sctx, baseSHA, changed, reviewable)
	if excerptBytes := sctx.Config.Jev.CandidateExcerptBytes; excerptBytes > 0 {
		attachJevExcerpts(sctx.WorkDir, candidates, excerptBytes)
	}
	if state == nil {
		logf("jev pre-brief skipped: could not read the change digest; reviewing without a pre-brief")
		return ""
	}
	if len(candidates) == 0 {
		logf("jev pre-brief skipped: no surrounding-context candidates to rank; reviewing without a pre-brief")
		return ""
	}
	resp, err := client.Evaluate(ctx, state, buildJevQuestions(candidates))
	if err != nil {
		logf("jev pre-brief unavailable (%v); reviewing without a pre-brief", err)
		return ""
	}
	section, listed := formatJevPrebrief(resp, candidates)
	logf("jev pre-brief: %d of %d context candidates listed (model %s, %d input tokens)", listed, len(candidates), resp.Model, resp.Usage.InputTokens)
	return section
}

// buildJevReviewState assembles the code-filtered Jev state: the change
// digest (stat plus clipped diff of the reviewable paths only, so content
// ignore_patterns excludes from the review never crowds the clipped digest)
// and the candidate surrounding-context files. Nil when the diff itself
// cannot be read, which the caller treats as fail-closed.
func buildJevReviewState(ctx context.Context, sctx *pipeline.StepContext, baseSHA string, changed, reviewable []string) (*jevChangeState, []jevCandidate) {
	rev := baseSHA + ".." + sctx.Run.HeadSHA
	if sctx.Fixing {
		rev = baseSHA
	}
	return buildJevStateFromRev(ctx, sctx.WorkDir, sctx.Run.Branch, baseSHA, rev, changed, reviewable, sctx.Config.IgnorePatterns)
}

// buildJevStateFromRev is the sctx-free core behind buildJevReviewState, so
// the offline replay harness can assemble the same production state from an
// explicit revision range. The range mirrors the review turn's changed-files
// diff, limited to the reviewable paths: a rereview diffs the worktree
// against the base (fix commits plus uncommitted fixer work) while an
// initial review diffs base..head.
func buildJevStateFromRev(ctx context.Context, workDir, branch, baseSHA, rev string, changed, reviewable []string, ignorePatterns []string) (*jevChangeState, []jevCandidate) {
	diffArgs := func(opts ...string) []string {
		args := append([]string{"diff", "--no-renames"}, opts...)
		args = append(args, rev, "--")
		for _, p := range reviewable {
			args = append(args, ":(literal)"+p)
		}
		return args
	}
	stat, err := git.Run(ctx, workDir, diffArgs("--stat")...)
	if err != nil {
		return nil, nil
	}
	diff, err := git.Run(ctx, workDir, diffArgs()...)
	if err != nil {
		return nil, nil
	}
	digest := jevChangeDigest{
		Branch:     branch,
		BaseCommit: baseSHA,
		DiffStat:   clipMiddle(stat, jevStatMaxBytes),
		Diff:       clipMiddle(diff, jevDiffMaxBytes),
	}
	candidates := jevContextCandidates(ctx, workDir, diff, changed, reviewable, ignorePatterns)
	return &jevChangeState{Change: digest, Candidates: candidates}, candidates
}

// buildJevQuestions packs one relevance Score per candidate into a single
// batched request. A candidate carrying an excerpt is judged from its path
// and that excerpt; a path-only candidate is judged from its path alone.
func buildJevQuestions(candidates []jevCandidate) map[string]jev.Question {
	questions := make(map[string]jev.Question, len(candidates))
	for i := range candidates {
		id := fmt.Sprintf("ctx_%d", i)
		basis := "Judge from its path and the change"
		if candidates[i].Excerpt != "" {
			basis = "Judge from its path and its content excerpt, alongside the change"
		}
		questions[id] = jev.Question{
			Type: "score",
			Instructions: fmt.Sprintf(
				"You are ranking supporting files for a code review. The change under review is in `change`. How relevant is the file `candidates[%d]` as surrounding context for reviewing that change? %s: candidates are files that use names the change defines, then files in the same directories as the changed files.", i, basis),
			Criteria: jevRelevanceLevels,
		}
	}
	return questions
}

// formatJevPrebrief renders the advisory prompt section from typed answers.
// "" when nothing clears the thresholds, so an uneventful pre-screen adds no
// prompt noise. Also returns the listed-candidate count for the log line.
//
// Jev's score alone decides which candidates are listed. The order blends in
// the code's use-site evidence, which Jev never sees: each candidate's
// coupling, scaled to the strongest one, adds up to one rubric level. A file
// that uses a changed name therefore precedes a sibling Jev scored the same,
// but never one Jev scored a full level higher.
// attachJevExcerpts fills each candidate's excerpt from the file in the
// review worktree, bounded by maxBytes per file and jevMaxExcerptTotalBytes
// across the request. A non-positive maxBytes leaves every candidate
// path-only. Files that cannot be read, that look binary, or that match
// ignore_patterns never reach this point as candidates at all (the builder
// excludes them); a read that fails here degrades to no excerpt for that
// file, never to an error. The per-request ceiling drops excerpts from the
// lowest-coupling candidates first, so a large candidate list cannot grow
// the request without bound.
func attachJevExcerpts(workDir string, candidates []jevCandidate, maxBytes int) {
	if maxBytes <= 0 {
		return
	}
	for i := range candidates {
		candidates[i].Excerpt = readJevExcerpt(workDir, candidates[i].Path, maxBytes)
	}
	enforceJevExcerptBudget(candidates)
}

// jevExcerptProbeBytes is the leading span scanned for a NUL byte, matching
// git's binary detection window.
const jevExcerptProbeBytes = 8000

// readJevExcerpt returns at most maxBytes of the file's leading content, cut
// at a line boundary and UTF-8 safe, or "" when the file should contribute
// no excerpt: unreadable, binary, outside the worktree, or not a regular
// file (a tracked symlink's git content is its link text, so following it
// would ship the target's bytes instead).
func readJevExcerpt(workDir, rel string, maxBytes int) string {
	if rel == "" || filepath.IsAbs(rel) {
		return ""
	}
	full := filepath.Join(workDir, filepath.FromSlash(rel))
	if outside, err := filepath.Rel(workDir, full); err != nil || outside == ".." || strings.HasPrefix(outside, ".."+string(filepath.Separator)) {
		return ""
	}
	if info, err := os.Lstat(full); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	f, err := os.Open(full)
	if err != nil {
		return ""
	}
	defer f.Close()
	// Only the leading slice is ever needed: enough for the excerpt plus
	// the binary probe, so a large tracked sibling never loads whole.
	content, err := io.ReadAll(io.LimitReader(f, int64(max(maxBytes, jevExcerptProbeBytes))+1))
	if err != nil || len(content) == 0 {
		return ""
	}
	// Binary files get no excerpt: a NUL byte in the leading probe is the
	// same signal git grep -I uses to treat a file as binary.
	probe := content
	if len(probe) > jevExcerptProbeBytes {
		probe = probe[:jevExcerptProbeBytes]
	}
	if strings.IndexByte(string(probe), 0) >= 0 {
		return ""
	}
	if len(content) <= maxBytes {
		return string(trimJevExcerptTail(content))
	}
	if cut := strings.LastIndexByte(string(content[:maxBytes]), '\n'); cut >= 0 {
		return string(content[:cut+1])
	}
	return string(trimJevExcerptTail(content[:maxBytes]))
}

// trimJevExcerptTail backs off over a trailing incomplete UTF-8 sequence so
// a hard byte cut never ends mid-rune. A cut at a line boundary needs no
// trimming (newline is ASCII), so this only runs on whole-file and
// single-line excerpts.
func trimJevExcerptTail(b []byte) []byte {
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		if r != utf8.RuneError || size > 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

// enforceJevExcerptBudget clears excerpts from the lowest-coupling
// candidates first until the summed excerpt bytes fit
// jevMaxExcerptTotalBytes. Ties break by path so the outcome is stable.
func enforceJevExcerptBudget(candidates []jevCandidate) {
	total := 0
	for _, c := range candidates {
		total += len(c.Excerpt)
	}
	if total <= jevMaxExcerptTotalBytes {
		return
	}
	order := make([]int, len(candidates))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := candidates[order[i]], candidates[order[j]]
		if a.coupling != b.coupling {
			return a.coupling < b.coupling
		}
		return a.Path < b.Path
	})
	for _, i := range order {
		if total <= jevMaxExcerptTotalBytes {
			break
		}
		total -= len(candidates[i].Excerpt)
		candidates[i].Excerpt = ""
	}
}

// JevReplayCandidate is the exported view of one ranked candidate for the
// offline replay harness: the path Jev saw, the code's use-site coupling the
// listing order blends in, and the excerpt the request carried (empty for
// path-only runs).
type JevReplayCandidate struct {
	Path     string
	Coupling float64
	Excerpt  string
}

// BuildJevReplayState assembles the production Jev state for an explicit
// base..head range in workDir, with excerpts attached per excerptBytes, for
// the offline replay harness. ignorePatterns filters exactly as the review
// turn does. It returns the state as sent to Evaluate plus the ordered
// candidates (nil when the diff itself cannot be read).
func BuildJevReplayState(ctx context.Context, workDir, branch, baseSHA, headSHA string, excerptBytes int, ignorePatterns []string) (any, []JevReplayCandidate, error) {
	raw, err := git.RunRaw(ctx, workDir, "diff", "--no-renames", "--name-only", "-z", baseSHA+".."+headSHA, "--")
	if err != nil {
		return nil, nil, fmt.Errorf("jev replay: read changed files: %w", err)
	}
	changed := changedPathList(string(raw))
	reviewable := reviewablePaths(changed, ignorePatterns)
	if len(reviewable) == 0 {
		return nil, nil, fmt.Errorf("jev replay: no reviewable files in %s..%s", baseSHA, headSHA)
	}
	state, candidates := buildJevStateFromRev(ctx, workDir, branch, baseSHA, baseSHA+".."+headSHA, changed, reviewable, ignorePatterns)
	if state == nil {
		return nil, nil, fmt.Errorf("jev replay: could not read the change digest")
	}
	attachJevExcerpts(workDir, candidates, excerptBytes)
	replay := make([]JevReplayCandidate, len(candidates))
	for i, c := range candidates {
		replay[i] = JevReplayCandidate{Path: c.Path, Coupling: c.coupling, Excerpt: c.Excerpt}
	}
	return state, replay, nil
}

// JevReplayQuestions packs the production relevance questions for replay
// candidates, so a live replay call asks exactly what the review turn asks.
func JevReplayQuestions(candidates []JevReplayCandidate) map[string]jev.Question {
	return buildJevQuestions(replayCandidates(candidates))
}

// RankJevPrebrief applies the production listing rule to a recorded response
// and returns the listed paths in display order. The replay harness joins
// this order against its relevance labels to measure hit rate.
func RankJevPrebrief(resp *jev.Response, candidates []JevReplayCandidate) []string {
	internal := replayCandidates(candidates)
	listing := rankJevPrebrief(resp, internal)
	paths := make([]string, len(listing))
	for i, item := range listing {
		paths[i] = item.path
	}
	return paths
}

// replayCandidates converts replay candidates to the internal shape the
// production ranker and question packer run on.
func replayCandidates(candidates []JevReplayCandidate) []jevCandidate {
	internal := make([]jevCandidate, len(candidates))
	for i, c := range candidates {
		internal[i] = jevCandidate{Path: c.Path, Excerpt: c.Excerpt, coupling: c.Coupling}
	}
	return internal
}

type jevRanked struct {
	path    string
	jev     float64
	blended float64
}

// rankJevPrebrief applies the listing rule: Jev's score alone decides which
// candidates are listed, and the code's use-site evidence lifts a candidate
// by up to one rubric level in the order. The cap keeps the files Jev scored
// highest, and the blend only reorders them.
func rankJevPrebrief(resp *jev.Response, candidates []jevCandidate) []jevRanked {
	strongest := 0.0
	for _, c := range candidates {
		strongest = max(strongest, c.coupling)
	}
	var listing []jevRanked
	for i, c := range candidates {
		answer, ok := resp.Answers[fmt.Sprintf("ctx_%d", i)]
		if !ok || answer.Type != "score" {
			continue
		}
		if answer.Probabilities["2"]+answer.Probabilities["3"] < jevRelevanceThreshold || answer.Confidence < jevConfidenceThreshold {
			continue
		}
		blended := answer.Score
		if strongest > 0 {
			blended += c.coupling / strongest
		}
		listing = append(listing, jevRanked{path: c.Path, jev: answer.Score, blended: blended})
	}
	if len(listing) == 0 {
		return nil
	}
	sort.SliceStable(listing, func(i, j int) bool { return listing[i].jev > listing[j].jev })
	if len(listing) > jevMaxListed {
		listing = listing[:jevMaxListed]
	}
	sort.SliceStable(listing, func(i, j int) bool { return listing[i].blended > listing[j].blended })
	return listing
}

func formatJevPrebrief(resp *jev.Response, candidates []jevCandidate) (string, int) {
	listing := rankJevPrebrief(resp, candidates)
	if len(listing) == 0 {
		return "", 0
	}
	var b strings.Builder
	b.WriteString("\n\nPre-brief (advisory output of a fast pre-screen model; claims, not evidence):\n")
	b.WriteString("Every obligation above is unchanged: read and judge every changed file yourself, and treat each statement below as a hint to verify, never as a finding.\n")
	b.WriteString("- Surrounding context ranked most relevant to this change; consider reading it first, and explore beyond it as the change requires:\n")
	for _, item := range listing {
		b.WriteString("  - " + item.path + "\n")
	}
	return b.String(), len(listing)
}

// definitionPattern extracts the name a definition line introduces, across
// common languages, so its use sites can be found with git grep. It is
// anchored at the start of the line (after indentation and common modifiers)
// so prose - a comment saying "we let the user" - never yields a name.
// Definitions are the stable handle a change offers; call-site shaped tokens
// are far noisier.
var definitionPattern = regexp.MustCompile(`^\s*(?:(?:export|default|pub|public|private|protected|static|async|abstract|final)\s+)*(?:func|def|function|fn|sub|type|class|struct|enum|interface|trait|record|const|var|let)\s+(?:\([^)]*\)\s*)?([A-Za-z_][A-Za-z0-9_]*)`)

// jevIdentifiers returns the distinct defined names in the diff, taken in
// turn from each changed code file so the first file in path order cannot
// claim every search slot. Within a file, names keep their diff order and
// come from three line shapes: the hunk header's trailing context and the
// unchanged lines before a hunk's first change (the enclosing definition a
// behavior-only edit modifies), and added or removed definition lines (what
// the change introduces or replaces). Unchanged lines after a hunk's first
// change are skipped: they are neighbours of the change, not part of it.
// Prose files are skipped, and names shorter than jevMinIdentifierLen are
// dropped.
func jevIdentifiers(diff string) []string {
	seen := map[string]bool{}
	var byFile [][]string
	prose, changedInHunk := false, false
	add := func(text string) {
		if len(byFile) > 0 {
			byFile[len(byFile)-1] = appendDefinition(byFile[len(byFile)-1], seen, text)
		}
	}
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			// The header line ends with the post-image path.
			prose = isProsePath(line)
			byFile = append(byFile, nil)
		case prose:
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "@@"):
			changedInHunk = false
			if _, rest, ok := strings.Cut(line, "@@"); ok {
				// rest follows the first @@; the trailing context follows the
				// second one.
				if _, context, ok := strings.Cut(rest, "@@"); ok {
					add(context)
				}
			}
		case strings.HasPrefix(line, "+"), strings.HasPrefix(line, "-"):
			changedInHunk = true
			add(line[1:])
		case strings.HasPrefix(line, " ") && !changedInHunk:
			add(line[1:])
		}
	}
	var ids []string
	for i, more := 0, true; more; i++ {
		more = false
		for _, names := range byFile {
			if i < len(names) {
				ids = append(ids, names[i])
				more = true
			}
		}
	}
	return ids
}

// isProsePath reports whether a path names a documentation or prose file,
// whose lines are sentences rather than definitions.
func isProsePath(p string) bool {
	switch strings.ToLower(path.Ext(strings.Trim(p, `"`))) {
	case ".md", ".mdx", ".markdown", ".rst", ".txt", ".adoc":
		return true
	}
	return false
}

// appendDefinition appends the name definitionPattern finds in text.
func appendDefinition(ids []string, seen map[string]bool, text string) []string {
	match := definitionPattern.FindStringSubmatch(text)
	if match == nil {
		return ids
	}
	name := match[1]
	if len(name) < jevMinIdentifierLen || seen[name] {
		return ids
	}
	seen[name] = true
	return append(ids, name)
}

// jevContextCandidates builds the candidate surrounding-context set without
// any model: use sites of the identifiers the change introduces or modifies,
// then same-directory siblings of the reviewable files. Changed files are
// never candidates - the reviewer reads them regardless - nor are paths
// matching ignore_patterns, and the set is capped at jevMaxCandidates.
//
// Use sites are ranked by how specific their matches are: each identifier
// contributes 1/N to every file it appears in, N being how many files it
// appears in. A file referencing a rare changed name outranks one that only
// shares a ubiquitous name, so the cap keeps the tightest coupling rather
// than whatever sorts first by path.
func jevContextCandidates(ctx context.Context, workDir, diff string, changed, reviewable, ignorePatterns []string) []jevCandidate {
	changedSet := make(map[string]bool, len(changed))
	for _, p := range changed {
		changedSet[p] = true
	}
	// git grep -l prints each matching path once, so the output is bounded by
	// the number of tracked files however common the name is. git grep exits
	// 1 when nothing matches; that is not an error for an advisory list. A
	// name without an unchanged, non-ignored use site does not count toward
	// jevMaxIdentifiers.
	score := map[string]float64{}
	used, greps := 0, 0
	for _, id := range jevIdentifiers(diff) {
		if used >= jevMaxIdentifiers || greps >= jevMaxGreps {
			break
		}
		greps++
		files := nulSeparated(jevGrep(ctx, workDir, "-l", "-z", "-w", "-F", "-I", "-e", id))
		found := false
		for _, f := range reviewablePaths(files, ignorePatterns) {
			if !changedSet[f] {
				score[f] += 1 / float64(len(files))
				found = true
			}
		}
		if found {
			used++
		}
	}
	order := make([]string, 0, len(score))
	for f := range score {
		order = append(order, f)
	}
	sort.Slice(order, func(i, j int) bool {
		if score[order[i]] != score[order[j]] {
			return score[order[i]] > score[order[j]]
		}
		return order[i] < order[j]
	})
	if len(order) > jevMaxCandidates {
		order = order[:jevMaxCandidates]
	}
	candidates := make([]jevCandidate, len(order))
	index := make(map[string]int, len(order))
	for i, f := range order {
		candidates[i] = jevCandidate{Path: f, coupling: score[f]}
		index[f] = i
	}

	// Same-directory siblings, path-only, in one pass over one ls-files read.
	if len(candidates) < jevMaxCandidates {
		if tracked, err := git.RunRaw(ctx, workDir, "ls-files", "-z"); err == nil {
			dirs := make(map[string]bool, len(reviewable))
			for _, p := range reviewable {
				dirs[path.Dir(p)] = true
			}
			for _, f := range reviewablePaths(nulSeparated(tracked), ignorePatterns) {
				if len(candidates) >= jevMaxCandidates {
					break
				}
				if _, seen := index[f]; seen || changedSet[f] || !dirs[path.Dir(f)] {
					continue
				}
				index[f] = len(candidates)
				candidates = append(candidates, jevCandidate{Path: f})
			}
		}
	}
	return candidates
}

// jevGrep runs git grep, treating "no match" (exit 1) and every other
// failure as no output.
func jevGrep(ctx context.Context, workDir string, args ...string) []byte {
	out, err := git.RunRaw(ctx, workDir, append([]string{"grep"}, args...)...)
	if err != nil {
		return nil
	}
	return out
}

// nulSeparated splits NUL-terminated git output.
func nulSeparated(out []byte) []string {
	if len(out) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

// clipMiddle keeps the head and tail of text within maxBytes, marking the
// omission, so a clipped digest still shows both the change's start and its
// end.
func clipMiddle(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	head := maxBytes / 2
	tail := maxBytes - head
	return text[:head] + fmt.Sprintf("\n... [%d bytes omitted] ...\n", len(text)-maxBytes) + text[len(text)-tail:]
}
