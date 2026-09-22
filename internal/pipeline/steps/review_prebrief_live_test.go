package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/jev"
)

// TestReviewStep_LiveTypeSafePrebrief drives the production pre-brief path
// (ReviewStep with jev.review_assist, real TypeSafe client, no fake) against
// the live API on the fixture change in setupJevRepo. The reviewer is a mock
// so this measures Jev billed tokens and wall time, not a full agent review.
// Skipped unless NO_MISTAKES_JEV_LIVE=1 and TYPESAFE_API_KEY are set.
func TestReviewStep_LiveTypeSafePrebrief(t *testing.T) {
	if os.Getenv("NO_MISTAKES_JEV_LIVE") != "1" {
		t.Skip("set NO_MISTAKES_JEV_LIVE=1 and TYPESAFE_API_KEY to run the live TypeSafe pre-brief scenario")
	}
	if os.Getenv(jev.EnvKey) == "" {
		t.Skip(jev.EnvKey + " is unset; live TypeSafe pre-brief scenario skipped")
	}

	dir, baseSHA, headSHA := setupJevRepo(t)
	var logs []string
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return cleanReviewResult(t, dir, baseSHA), nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Jev.ReviewAssist = true
	sctx.Log = func(s string) { logs = append(logs, s); t.Log(s) }

	start := time.Now()
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	wall := time.Since(start)

	joined := strings.Join(logs, "\n")
	if strings.Contains(joined, "not set") || strings.Contains(joined, "unavailable") {
		t.Fatalf("live pre-brief degraded instead of calling TypeSafe:\n%s", joined)
	}

	// Production log line: "jev pre-brief: L of C context candidates listed (model M, N input tokens)"
	re := regexp.MustCompile(`jev pre-brief: (\d+) of (\d+) context candidates listed \(model ([^,]+), (\d+) input tokens\)`)
	var match []string
	for _, line := range logs {
		if m := re.FindStringSubmatch(line); m != nil {
			match = m
			break
		}
	}
	if match == nil {
		t.Fatalf("no production jev pre-brief success line in logs:\n%s", joined)
	}
	listed, _ := strconv.Atoi(match[1])
	candidates, _ := strconv.Atoi(match[2])
	model := match[3]
	inputTokens, _ := strconv.Atoi(match[4])
	if !strings.HasPrefix(model, "jev-") {
		t.Fatalf("reported model %q, want a jev- versioned id", model)
	}
	if candidates <= 0 {
		t.Fatal("live pre-brief listed against zero candidates")
	}
	if inputTokens <= 0 {
		t.Fatalf("live pre-brief input_tokens = %d, want billed input", inputTokens)
	}

	// Jev may legitimately list nothing, so the live check is that the reviewer
	// prompt agrees with the logged count: a pre-brief section exactly when
	// something was listed, naming that many paths.
	prompt := reviewPromptOf(t, ag)
	hasPrebrief := strings.Contains(prompt, "Pre-brief (advisory")
	if hasPrebrief != (listed > 0) {
		t.Fatalf("logged %d listed candidates but prompt_has_prebrief=%v:\n%s", listed, hasPrebrief, prompt)
	}
	if listed > 0 {
		_, section, _ := strings.Cut(prompt, "Pre-brief (advisory")
		if got := strings.Count(section, "\n  - "); got != listed {
			t.Fatalf("pre-brief section lists %d paths, log reported %d:\n%s", got, listed, section)
		}
	}

	t.Logf("live pre-brief: listed=%d candidates=%d model=%s input_tokens=%d wall_ms=%d prompt_has_prebrief=%v",
		listed, candidates, model, inputTokens, wall.Milliseconds(), hasPrebrief)

	if dir := os.Getenv("NO_MISTAKES_JEV_LIVE_EVIDENCE"); dir != "" {
		payload := map[string]any{
			"kind":                "review-prebrief",
			"reported_model":      model,
			"listed":              listed,
			"candidates":          candidates,
			"input_tokens":        inputTokens,
			"wall_ms":             wall.Milliseconds(),
			"prompt_has_prebrief": hasPrebrief,
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create live evidence dir: %v", err)
		}
		encoded, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			t.Fatalf("encode live evidence: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "live-prebrief.json"), append(encoded, '\n'), 0o644); err != nil {
			t.Fatalf("write live evidence: %v", err)
		}
	}
}

// capturingJevClient delegates to the real TypeSafe client while keeping the
// state it sent, so a live test can prove the excerpts rode the actual
// request rather than only asserting the call succeeded.
type capturingJevClient struct {
	inner jevClient
	state *jevChangeState
}

func (c *capturingJevClient) Evaluate(ctx context.Context, state any, questions map[string]jev.Question) (*jev.Response, error) {
	if s, ok := state.(*jevChangeState); ok {
		c.state = s
	}
	return c.inner.Evaluate(ctx, state, questions)
}

// TestReviewStep_LiveTypeSafePrebriefWithExcerpts drives the production
// pre-brief path with jev.candidate_excerpt_bytes set against the live API
// on the fixture change in setupJevRepo. It proves an excerpts-on request is
// accepted and answered, and that the state sent carried bounded excerpts.
// Skipped unless NO_MISTAKES_JEV_LIVE=1 and TYPESAFE_API_KEY are set.
func TestReviewStep_LiveTypeSafePrebriefWithExcerpts(t *testing.T) {
	if os.Getenv("NO_MISTAKES_JEV_LIVE") != "1" {
		t.Skip("set NO_MISTAKES_JEV_LIVE=1 and TYPESAFE_API_KEY to run the live TypeSafe pre-brief scenario")
	}
	if os.Getenv(jev.EnvKey) == "" {
		t.Skip(jev.EnvKey + " is unset; live TypeSafe pre-brief scenario skipped")
	}
	inner := jev.NewClientFromEnv()
	if inner == nil {
		t.Fatalf("%s is set but no client was built", jev.EnvKey)
	}
	captured := &capturingJevClient{inner: inner}

	dir, baseSHA, headSHA := setupJevRepo(t)
	var logs []string
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return cleanReviewResult(t, dir, baseSHA), nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Jev.ReviewAssist = true
	sctx.Config.Jev.CandidateExcerptBytes = 512
	sctx.Log = func(s string) { logs = append(logs, s); t.Log(s) }

	start := time.Now()
	if _, err := (&ReviewStep{jev: captured}).Execute(sctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	wall := time.Since(start)

	joined := strings.Join(logs, "\n")
	if strings.Contains(joined, "not set") || strings.Contains(joined, "unavailable") {
		t.Fatalf("live excerpts-on pre-brief degraded instead of calling TypeSafe:\n%s", joined)
	}
	if captured.state == nil {
		t.Fatal("live call ran without a captured state")
	}
	withExcerpt, excerptBytes := 0, 0
	for _, c := range captured.state.Candidates {
		if c.Excerpt == "" {
			continue
		}
		withExcerpt++
		excerptBytes += len(c.Excerpt)
		if len(c.Excerpt) > 512 {
			t.Fatalf("excerpt of %s is %d bytes, over the 512-byte budget", c.Path, len(c.Excerpt))
		}
	}
	if withExcerpt == 0 {
		t.Fatal("no candidate carried an excerpt on the live excerpts-on call")
	}
	if excerptBytes > jevMaxExcerptTotalBytes {
		t.Fatalf("excerpts total %d bytes, over the %d-byte per-request ceiling", excerptBytes, jevMaxExcerptTotalBytes)
	}

	re := regexp.MustCompile(`jev pre-brief: (\d+) of (\d+) context candidates listed \(model ([^,]+), (\d+) input tokens\)`)
	var match []string
	for _, line := range logs {
		if m := re.FindStringSubmatch(line); m != nil {
			match = m
			break
		}
	}
	if match == nil {
		t.Fatalf("no production jev pre-brief success line in logs:\n%s", joined)
	}
	listed, _ := strconv.Atoi(match[1])
	candidates, _ := strconv.Atoi(match[2])
	model := match[3]
	inputTokens, _ := strconv.Atoi(match[4])
	t.Logf("live excerpts-on pre-brief: listed=%d candidates=%d with_excerpt=%d excerpt_bytes=%d model=%s input_tokens=%d wall_ms=%d",
		listed, candidates, withExcerpt, excerptBytes, model, inputTokens, wall.Milliseconds())

	if dir := os.Getenv("NO_MISTAKES_JEV_LIVE_EVIDENCE"); dir != "" {
		payload := map[string]any{
			"kind":                "review-prebrief-excerpts",
			"reported_model":      model,
			"listed":              listed,
			"candidates":          candidates,
			"with_excerpt":        withExcerpt,
			"excerpt_bytes":       excerptBytes,
			"excerpt_budget":      512,
			"input_tokens":        inputTokens,
			"wall_ms":             wall.Milliseconds(),
			"prompt_has_prebrief": strings.Contains(reviewPromptOf(t, ag), "Pre-brief (advisory"),
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create live evidence dir: %v", err)
		}
		encoded, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			t.Fatalf("encode live evidence: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "live-prebrief-excerpts.json"), append(encoded, '\n'), 0o644); err != nil {
			t.Fatalf("write live evidence: %v", err)
		}
	}
}
