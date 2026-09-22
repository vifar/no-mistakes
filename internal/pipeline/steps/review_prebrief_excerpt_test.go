package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func writeJevFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAttachJevExcerpts_DisabledLeavesPathOnly pins the default: with a
// non-positive budget no excerpt is attached, and the marshalled state
// carries no excerpt key at all, so the request is byte-identical to
// path-only.
func TestAttachJevExcerpts_DisabledLeavesPathOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeJevFile(t, dir, "a.go", "package a\n")
	candidates := []jevCandidate{{Path: "a.go", coupling: 1}}
	attachJevExcerpts(dir, candidates, 0)
	if candidates[0].Excerpt != "" {
		t.Fatalf("excerpt = %q with the budget disabled", candidates[0].Excerpt)
	}
	encoded, err := json.Marshal(jevChangeState{Candidates: candidates})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "excerpt") {
		t.Fatalf("path-only state carries an excerpt key:\n%s", encoded)
	}
}

// TestReadJevExcerpt_WholeFileAndCap pins the per-file budget: a small file
// arrives whole, and a larger one is cut at a line boundary within the
// budget, never mid-line.
func TestReadJevExcerpt_WholeFileAndCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeJevFile(t, dir, "small.go", "package small\n")
	if got := readJevExcerpt(dir, "small.go", 1024); got != "package small\n" {
		t.Fatalf("small file excerpt = %q, want the whole file", got)
	}
	lines := []string{"line-one\n", "line-two\n", "line-three\n", "line-four\n"}
	writeJevFile(t, dir, "big.go", strings.Join(lines, ""))
	// 20 bytes hold two full lines; the third would overflow.
	got := readJevExcerpt(dir, "big.go", 20)
	if got != "line-one\nline-two\n" {
		t.Fatalf("capped excerpt = %q, want the line-boundary cut", got)
	}
	if len(got) > 20 {
		t.Fatalf("excerpt is %d bytes, over the 20-byte budget", len(got))
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("excerpt does not end at a line boundary: %q", got)
	}
}

// TestReadJevExcerpt_UTF8Safe pins that a budget cutting through a multibyte
// rune (with no earlier newline to cut at) still yields valid UTF-8 within
// the budget.
func TestReadJevExcerpt_UTF8Safe(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// One long line: every rune is two bytes, so an odd budget splits one.
	writeJevFile(t, dir, "uni.go", strings.Repeat("é", 100)+"\n")
	got := readJevExcerpt(dir, "uni.go", 101)
	if !utf8.ValidString(got) {
		t.Fatalf("excerpt is not valid UTF-8: %q", got)
	}
	if len(got) > 101 {
		t.Fatalf("excerpt is %d bytes, over the 101-byte budget", len(got))
	}
	if len(got) == 0 {
		t.Fatal("excerpt is empty, want the runes that fit")
	}
	// A line-boundary cut of valid content stays valid without trimming.
	writeJevFile(t, dir, "mixed.go", "package mixed // héllo\nfunc Mixed() {}\n")
	got = readJevExcerpt(dir, "mixed.go", 24)
	if !utf8.ValidString(got) {
		t.Fatalf("line-cut excerpt is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("excerpt does not end at a line boundary: %q", got)
	}
}

// TestReadJevExcerpt_BinarySkipped pins that files with NUL bytes get no
// excerpt, the same binary signal git grep -I uses.
func TestReadJevExcerpt_BinarySkipped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(p, []byte{'G', 'I', 'F', '8', 0, 'x', 'y'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readJevExcerpt(dir, "blob.bin", 1024); got != "" {
		t.Fatalf("binary excerpt = %q, want none", got)
	}
}

// TestReadJevExcerpt_LargeFileYieldsBoundedPrefix pins that a candidate far
// larger than both the budget and the binary probe window still yields the
// same line-cut leading excerpt as a small one.
func TestReadJevExcerpt_LargeFileYieldsBoundedPrefix(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := []byte(strings.Repeat("line\n", 4000))
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readJevExcerpt(dir, "big.txt", 12); got != "line\nline\n" {
		t.Fatalf("excerpt = %q, want two lines", got)
	}
}

// TestReadJevExcerpt_MissingAndOutside pins fail-soft reads: a candidate
// that cannot be read, or that escapes the worktree, contributes no excerpt
// and no error.
func TestReadJevExcerpt_MissingAndOutside(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got := readJevExcerpt(dir, "nope.go", 1024); got != "" {
		t.Fatalf("missing file excerpt = %q, want none", got)
	}
	if got := readJevExcerpt(dir, "../escape.go", 1024); got != "" {
		t.Fatalf("escaping path excerpt = %q, want none", got)
	}
	if got := readJevExcerpt(dir, "/abs.go", 1024); got != "" {
		t.Fatalf("absolute path excerpt = %q, want none", got)
	}
}

// TestReadJevExcerpt_SymlinkNotFollowed pins that a tracked symlink (which
// git ls-files lists as a candidate) contributes no excerpt: following it
// would ship the link target's bytes, which may live outside the worktree.
func TestReadJevExcerpt_SymlinkNotFollowed(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("hostname-secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "latest")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if got := readJevExcerpt(dir, "latest", 1024); got != "" {
		t.Fatalf("symlink excerpt = %q, want none", got)
	}
}

// TestEnforceJevExcerptBudget_DropsLowestCouplingFirst pins the per-request
// ceiling: excerpts past it are cleared from the lowest-coupling candidates
// first, with a path tie-break, and the survivors keep their excerpts.
func TestEnforceJevExcerptBudget_DropsLowestCouplingFirst(t *testing.T) {
	t.Parallel()
	mk := func(path string, coupling float64) jevCandidate {
		return jevCandidate{Path: path, coupling: coupling, Excerpt: strings.Repeat("x\n", 4000)}
	}
	candidates := []jevCandidate{
		mk("sibling-b.go", 0),
		mk("sibling-a.go", 0),
		mk("use.go", 0.5),
		mk("top.go", 2),
	}
	enforceJevExcerptBudget(candidates)
	byPath := map[string]jevCandidate{}
	total := 0
	for _, c := range candidates {
		byPath[c.Path] = c
		total += len(c.Excerpt)
	}
	if total > jevMaxExcerptTotalBytes {
		t.Fatalf("excerpts total %d bytes, over the %d-byte ceiling", total, jevMaxExcerptTotalBytes)
	}
	if byPath["sibling-a.go"].Excerpt != "" || byPath["sibling-b.go"].Excerpt != "" {
		t.Fatal("the ceiling did not drop the zero-coupling siblings first")
	}
	if byPath["top.go"].Excerpt == "" {
		t.Fatal("the ceiling dropped the strongest-coupling excerpt")
	}
	if got := len(byPath["use.go"].Excerpt); got != 8000 {
		t.Fatalf("survivor excerpt is %d bytes, want the untouched 8000", got)
	}
}

// TestReviewStep_JevExcerptEnabledReachesState pins the enabled path end to
// end: with the assist and a byte budget set, the fake client sees excerpts
// in the state, and its questions judge from path and excerpt.
func TestReviewStep_JevExcerptEnabledReachesState(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupJevRepo(t)
	fake := &fakeJevClient{answer: highScoreEverything}
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return cleanReviewResult(t, dir, baseSHA), nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Jev.ReviewAssist = true
	sctx.Config.Jev.CandidateExcerptBytes = 512

	if _, err := (&ReviewStep{jev: fake}).Execute(sctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fake.state == nil {
		t.Fatal("jev was not consulted")
	}
	excerpts := 0
	for _, c := range fake.state.Candidates {
		if c.Excerpt != "" {
			excerpts++
			if len(c.Excerpt) > 512 {
				t.Fatalf("excerpt of %s is %d bytes, over the 512-byte budget", c.Path, len(c.Excerpt))
			}
		}
	}
	if excerpts == 0 {
		t.Fatal("no candidate carries an excerpt with the budget enabled")
	}
	found := false
	for _, c := range fake.state.Candidates {
		if c.Path == "widget/user.go" {
			found = true
			if !strings.Contains(c.Excerpt, "RenderWidget") {
				t.Fatalf("use-site excerpt misses the coupled symbol:\n%s", c.Excerpt)
			}
		}
	}
	if !found {
		t.Fatal("widget/user.go not among the candidates")
	}
	for id, q := range fake.lastReq {
		text, _ := q.Instructions.(string)
		if !strings.Contains(text, "content excerpt") {
			t.Errorf("question %s does not judge from the excerpt: %q", id, text)
		}
	}
}

// TestReviewStep_JevExcerptDisabledStaysPathOnly pins that enabling the
// assist alone never attaches content: the state the fake sees marshals
// without any excerpt key.
func TestReviewStep_JevExcerptDisabledStaysPathOnly(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupJevRepo(t)
	fake := &fakeJevClient{answer: highScoreEverything}
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return cleanReviewResult(t, dir, baseSHA), nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Jev.ReviewAssist = true

	if _, err := (&ReviewStep{jev: fake}).Execute(sctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fake.state == nil {
		t.Fatal("jev was not consulted")
	}
	for _, c := range fake.state.Candidates {
		if c.Excerpt != "" {
			t.Fatalf("candidate %s carries an excerpt with the budget at 0", c.Path)
		}
	}
	encoded, err := json.Marshal(fake.state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "excerpt") {
		t.Fatalf("assist-only state carries an excerpt key:\n%s", encoded)
	}
	for id, q := range fake.lastReq {
		text, _ := q.Instructions.(string)
		if strings.Contains(text, "excerpt") {
			t.Errorf("path-only question %s mentions excerpts: %q", id, text)
		}
	}
}

// TestReviewStep_JevExcerptRespectsIgnorePatterns pins that excerpts never
// smuggle an ignored file back in: with the budget enabled, ignored paths
// stay out of the candidates and none of their content reaches the state.
func TestReviewStep_JevExcerptRespectsIgnorePatterns(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupJevRepo(t)
	fake := &fakeJevClient{answer: highScoreEverything}
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return cleanReviewResult(t, dir, baseSHA), nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Jev.ReviewAssist = true
	sctx.Config.Jev.CandidateExcerptBytes = 4096
	sctx.Config.IgnorePatterns = []string{"widget/*_test.go", "widget/*_pb.go"}

	if _, err := (&ReviewStep{jev: fake}).Execute(sctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if fake.state == nil {
		t.Fatal("jev was not consulted")
	}
	for _, c := range fake.state.Candidates {
		if c.Path == "widget/widget_test.go" || c.Path == "widget/widget_pb.go" {
			t.Fatalf("ignored file %s is a candidate", c.Path)
		}
	}
	encoded, err := json.Marshal(fake.state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "widget_test.go") || strings.Contains(string(encoded), "widget_pb.go") {
		t.Fatalf("state references an ignored file:\n%s", encoded)
	}
}
