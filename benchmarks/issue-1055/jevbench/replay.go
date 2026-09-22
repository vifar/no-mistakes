// Replay mode measures the Jev pre-brief's candidate ranking against a
// tracked relevance label set, without needing review agents.
//
// Labels (benchmarks/issue-1125/candidates.jsonl) mark every candidate of the
// issue-1125 corpus change (merged PR #1120, the 40 candidates that run
// scored) relevant or not, with a one-line reason each.
//
// A replay takes either a recorded response or a live call:
//
//	go run ./benchmarks/issue-1055/jevbench -replay -repo <repo> \
//	  -base <sha> -head <sha> -change <name> -labels <candidates.jsonl> \
//	  -response <recorded.json> -out <dir>
//
// With -response, the harness reads the recorded candidates plus either full
// per-question answers (score-ranked through the production listing rule) or
// a bare recorded listed set (the issue-1125 run persisted no per-question
// scores, so its replay measures the listed set only).
//
// Without -response, the harness rebuilds the production state from the repo
// (same digest, same candidates, excerpts per -excerpt-bytes) and calls the
// live TypeSafe API, then persists the full response beside the summary so
// later replays are score-ranked. The live arm requires NO_MISTAKES_JEV_LIVE=1
// and TYPESAFE_API_KEY, mirroring the gated live tests; without both it
// refuses instead of touching the network.
//
// Metrics, computed over the ranked order (at most jevMaxListed entries, the
// production cap):
//
//   - hit@10: labeled-relevant candidates in the first 10, with recall
//     against all labeled-relevant candidates.
//   - non-empty pre-brief rate: 1 when anything listed, else 0 (one run).
//   - listed precision: relevant listed over listed total.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/jev"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
)

// candidateLabel is one row of the tracked relevance label set.
type candidateLabel struct {
	Path     string  `json:"path"`
	Coupling float64 `json:"coupling"`
	Relevant bool    `json:"relevant"`
	Reason   string  `json:"reason"`
}

// recordedResponse is a persisted pre-brief evaluation: the ordered
// candidates, the full per-question answers when kept, and the listed set.
// The issue-1125 excerpts-off record carries an empty answers map because
// that run persisted only the outcome, not the scores.
type recordedResponse struct {
	Change         string `json:"change"`
	Mode           string `json:"mode"`
	BaseSHA        string `json:"base_sha"`
	HeadSHA        string `json:"head_sha"`
	JevModel       string `json:"jev_model"`
	JevInputTokens int    `json:"jev_input_tokens"`
	Candidates     []struct {
		Path     string  `json:"path"`
		Coupling float64 `json:"coupling"`
		Excerpt  string  `json:"excerpt,omitempty"`
	} `json:"candidates"`
	Answers map[string]jev.Answer `json:"answers"`
	Listed  []string              `json:"listed"`
}

// replaySummary is the replay's evidence row.
type replaySummary struct {
	Change           string   `json:"change"`
	Mode             string   `json:"mode"`
	BaseSHA          string   `json:"base_sha"`
	HeadSHA          string   `json:"head_sha"`
	Candidates       int      `json:"candidates"`
	LabeledRelevant  int      `json:"labeled_relevant"`
	RankedOrder      []string `json:"ranked_order"`
	HitAt10          int      `json:"hit_at_10"`
	HitAt10Recall    float64  `json:"hit_at_10_recall"`
	Listed           int      `json:"listed"`
	ListedRelevant   int      `json:"listed_relevant"`
	ListedPrecision  float64  `json:"listed_precision"`
	NonEmptyPrebrief int      `json:"non_empty_prebrief"`
	JevModel         string   `json:"jev_model,omitempty"`
	JevInputTokens   int      `json:"jev_input_tokens,omitempty"`
	ScoreRanked      bool     `json:"score_ranked"`
}

func runReplay(repo, base, head, change, outDir, labelsPath, responsePath string, excerptBytes int) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if labelsPath == "" {
		return errors.New("-labels is required in replay mode")
	}
	labels, err := readLabels(labelsPath)
	if err != nil {
		return err
	}
	relevant := map[string]bool{}
	labeledRelevant := 0
	for _, l := range labels {
		relevant[l.Path] = l.Relevant
		if l.Relevant {
			labeledRelevant++
		}
	}

	var order []string
	var candidateCount int
	var mode, model string
	var inputTokens int
	var scoreRanked bool
	if responsePath != "" {
		order, candidateCount, mode, model, inputTokens, scoreRanked, err = replayRecorded(responsePath, base, head, relevant)
		if err != nil {
			return err
		}
	} else {
		var summary *replaySummary
		order, summary, err = replayLive(repo, base, head, change, outDir, excerptBytes, relevant, labeledRelevant)
		if err != nil {
			return err
		}
		return writeReplaySummary(outDir, change, summary)
	}

	summary := summarizeReplay(change, mode, base, head, order, candidateCount, relevant, labeledRelevant)
	summary.JevModel = model
	summary.JevInputTokens = inputTokens
	summary.ScoreRanked = scoreRanked
	return writeReplaySummary(outDir, change, summary)
}

func readLabels(path string) ([]candidateLabel, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var labels []candidateLabel
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var l candidateLabel
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if l.Path == "" || l.Reason == "" {
			return nil, fmt.Errorf("parse %s: label without path or reason: %s", path, line)
		}
		if seen[l.Path] {
			return nil, fmt.Errorf("parse %s: duplicate label for %s", path, l.Path)
		}
		seen[l.Path] = true
		labels = append(labels, l)
	}
	if len(labels) == 0 {
		return nil, fmt.Errorf("no labels in %s", path)
	}
	return labels, nil
}

// checkCandidateLabels requires the candidate set and the label set to be the
// same set: every candidate labeled exactly once and every label a candidate.
// A label without a candidate would inflate the recall denominator and a
// duplicate candidate would count twice in the ranked order, so either
// mismatch refuses the replay instead of publishing a skewed metric.
func checkCandidateLabels(source string, candidates []string, relevant map[string]bool) error {
	seen := make(map[string]bool, len(candidates))
	for _, p := range candidates {
		if _, ok := relevant[p]; !ok {
			return fmt.Errorf("%s: candidate %s has no label", source, p)
		}
		if seen[p] {
			return fmt.Errorf("%s: duplicate candidate %s", source, p)
		}
		seen[p] = true
	}
	for p := range relevant {
		if !seen[p] {
			return fmt.Errorf("%s: label %s is not a candidate", source, p)
		}
	}
	return nil
}

// checkListed requires a bare recorded listed set to be distinct candidates:
// the production listing is always a subset of the candidates, so anything
// else is a malformed recording rather than a ranking to measure.
func checkListed(source string, listed, candidates []string) error {
	candidate := make(map[string]bool, len(candidates))
	for _, p := range candidates {
		candidate[p] = true
	}
	seen := make(map[string]bool, len(listed))
	for _, p := range listed {
		if !candidate[p] {
			return fmt.Errorf("%s: listed %s is not a candidate", source, p)
		}
		if seen[p] {
			return fmt.Errorf("%s: duplicate listed %s", source, p)
		}
		seen[p] = true
	}
	return nil
}

// replayRecorded ranks a recorded response: full answers run through the
// production listing rule, while a bare listed set (the issue-1125 record)
// is measured as-is.
func replayRecorded(responsePath, base, head string, relevant map[string]bool) (order []string, candidateCount int, mode, model string, inputTokens int, scoreRanked bool, err error) {
	data, err := os.ReadFile(responsePath)
	if err != nil {
		return nil, 0, "", "", 0, false, err
	}
	var rec recordedResponse
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, 0, "", "", 0, false, fmt.Errorf("parse %s: %w", responsePath, err)
	}
	if rec.BaseSHA != base || rec.HeadSHA != head {
		return nil, 0, "", "", 0, false, fmt.Errorf("%s records %s..%s, want %s..%s", responsePath, rec.BaseSHA, rec.HeadSHA, base, head)
	}
	candidates := make([]steps.JevReplayCandidate, len(rec.Candidates))
	paths := make([]string, len(rec.Candidates))
	for i, c := range rec.Candidates {
		candidates[i] = steps.JevReplayCandidate{Path: c.Path, Coupling: c.Coupling, Excerpt: c.Excerpt}
		paths[i] = c.Path
	}
	if err := checkCandidateLabels(responsePath, paths, relevant); err != nil {
		return nil, 0, "", "", 0, false, err
	}
	if len(rec.Answers) > 0 {
		resp := &jev.Response{Model: rec.JevModel, Answers: rec.Answers, Usage: jev.Usage{InputTokens: rec.JevInputTokens}}
		return steps.RankJevPrebrief(resp, candidates), len(candidates), rec.Mode, rec.JevModel, rec.JevInputTokens, true, nil
	}
	if err := checkListed(responsePath, rec.Listed, paths); err != nil {
		return nil, 0, "", "", 0, false, err
	}
	return rec.Listed, len(candidates), rec.Mode, rec.JevModel, rec.JevInputTokens, false, nil
}

// replayLive rebuilds the production state from the repo, calls the live
// TypeSafe API once, persists the full response for later score-ranked
// replays, and returns the production listing order with its summary.
func replayLive(repo, base, head, change, outDir string, excerptBytes int, relevant map[string]bool, labeledRelevant int) ([]string, *replaySummary, error) {
	if os.Getenv("NO_MISTAKES_JEV_LIVE") != "1" || os.Getenv(jev.EnvKey) == "" {
		return nil, nil, fmt.Errorf("live replay needs NO_MISTAKES_JEV_LIVE=1 and %s; pass -response to replay a recorded response instead", jev.EnvKey)
	}
	client := jev.NewClientFromEnv()
	if client == nil {
		return nil, nil, fmt.Errorf("%s is set but no client was built", jev.EnvKey)
	}
	fixture, err := os.MkdirTemp("", "jevbench-replay-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(fixture)
	fixtureDir := filepath.Join(fixture, "wt")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "--detach", fixtureDir, head).CombinedOutput(); err != nil {
		return nil, nil, fmt.Errorf("create fixture worktree: %v: %s", err, out)
	}
	defer func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", fixtureDir).Run()
	}()

	// Production reads ignore_patterns from the pushed head, which is what
	// the fixture worktree holds, so the replay excludes the same candidates.
	repoCfg, err := config.LoadRepo(fixtureDir)
	if err != nil {
		return nil, nil, err
	}
	ctx := context.Background()
	state, candidates, err := steps.BuildJevReplayState(ctx, fixtureDir, "refs/heads/jevbench-replay", base, head, excerptBytes, repoCfg.IgnorePatterns)
	if err != nil {
		return nil, nil, err
	}
	paths := make([]string, len(candidates))
	for i, c := range candidates {
		paths[i] = c.Path
	}
	if err := checkCandidateLabels("live "+head, paths, relevant); err != nil {
		return nil, nil, fmt.Errorf("%w; label the change before replaying it", err)
	}
	resp, err := client.Evaluate(ctx, state, steps.JevReplayQuestions(candidates))
	if err != nil {
		return nil, nil, fmt.Errorf("live jev evaluation: %w", err)
	}
	order := steps.RankJevPrebrief(resp, candidates)

	mode := "excerpts-off"
	if excerptBytes > 0 {
		mode = "excerpts-on"
	}
	recorded := recordedResponse{
		Change: change, Mode: mode, BaseSHA: base, HeadSHA: head,
		JevModel: resp.Model, JevInputTokens: resp.Usage.InputTokens,
		Answers: resp.Answers, Listed: order,
	}
	for _, c := range candidates {
		recorded.Candidates = append(recorded.Candidates, struct {
			Path     string  `json:"path"`
			Coupling float64 `json:"coupling"`
			Excerpt  string  `json:"excerpt,omitempty"`
		}{Path: c.Path, Coupling: c.Coupling, Excerpt: c.Excerpt})
	}
	encoded, err := json.MarshalIndent(recorded, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	recordPath := filepath.Join(outDir, "recorded-"+change+"-"+mode+".json")
	if err := os.WriteFile(recordPath, append(encoded, '\n'), 0o644); err != nil {
		return nil, nil, err
	}
	fmt.Printf("jevbench: live %s response recorded in %s\n", mode, recordPath)

	summary := summarizeReplay(change, mode, base, head, order, len(candidates), relevant, labeledRelevant)
	summary.JevModel = resp.Model
	summary.JevInputTokens = resp.Usage.InputTokens
	summary.ScoreRanked = true
	return order, summary, nil
}

func summarizeReplay(change, mode, base, head string, order []string, candidateCount int, relevant map[string]bool, labeledRelevant int) *replaySummary {
	top := order
	if len(top) > 10 {
		top = top[:10]
	}
	hit := 0
	for _, p := range top {
		if relevant[p] {
			hit++
		}
	}
	listedRelevant := 0
	for _, p := range order {
		if relevant[p] {
			listedRelevant++
		}
	}
	summary := &replaySummary{
		Change: change, Mode: mode, BaseSHA: base, HeadSHA: head,
		Candidates: candidateCount, LabeledRelevant: labeledRelevant,
		RankedOrder: order, HitAt10: hit,
		Listed: len(order), ListedRelevant: listedRelevant,
	}
	if labeledRelevant > 0 {
		summary.HitAt10Recall = float64(hit) / float64(labeledRelevant)
	}
	if len(order) > 0 {
		summary.ListedPrecision = float64(listedRelevant) / float64(len(order))
		summary.NonEmptyPrebrief = 1
	}
	return summary
}

func writeReplaySummary(outDir, change string, summary *replaySummary) error {
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(outDir, "replay-"+change+".json")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("jevbench: replay %s %s: candidates=%d relevant=%d hit@10=%d recall=%.2f listed=%d non-empty=%d score_ranked=%v -> %s\n",
		summary.Change, summary.Mode, summary.Candidates, summary.LabeledRelevant,
		summary.HitAt10, summary.HitAt10Recall, summary.Listed, summary.NonEmptyPrebrief, summary.ScoreRanked, path)
	return nil
}
