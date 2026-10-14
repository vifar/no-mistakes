package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const factsHead = "0123456789abcdef0123456789abcdef01234567"

func TestReadPRFacts(t *testing.T) {
	h, tr := newTestHost(t, Options{})
	tr.reply(`{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_branch":"topic","base_branch":"main","head_sha":"` + factsHead + `"},"state":"open"}`)
	facts, err := h.ReadPRFacts(context.Background(), testPR())
	if err != nil {
		t.Fatal(err)
	}
	if facts.SourceRepository != "team/repo" || facts.SourceBranch != "topic" || facts.HeadSHA != factsHead || facts.BaseBranch != "main" || facts.State != scm.PRStateOpen {
		t.Fatalf("facts = %+v", facts)
	}
	if len(tr.calls) != 1 || !strings.Contains(strings.Join(tr.last().argv, " "), "--repo=team/repo") {
		t.Fatalf("expected one scoped view, calls = %+v", tr.calls)
	}
}

func TestReadPRFactsRejectsIncompleteIdentity(t *testing.T) {
	for name, doc := range map[string]string{
		"missing head":   `{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_branch":"topic","base_branch":"main"},"state":"open"}`,
		"malformed head": `{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_branch":"topic","base_branch":"main","head_sha":"abc"},"state":"open"}`,
		"wrong number":   `{"pr":{"number":8,"url":"https://git.example.com/team/repo/pulls/8","head_branch":"topic","base_branch":"main","head_sha":"` + factsHead + `"},"state":"open"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h, tr := newTestHost(t, Options{})
			tr.reply(`{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_branch":"topic","base_branch":"main","head_sha":"` + factsHead + `"},"state":"open"}`)
			if facts, err := h.ReadPRFacts(context.Background(), testPR()); err != nil || facts.HeadSHA != factsHead {
				t.Fatalf("valid identity = %+v, %v", facts, err)
			}
			tr.reply(doc)
			_, err := h.ReadPRFacts(context.Background(), testPR())
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestReadPRFactsRequiresMergeProof(t *testing.T) {
	h, tr := availableHost(t, statusAllCaps, Options{})
	tr.reply(`{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_branch":"topic","base_branch":"main","head_sha":"`+factsHead+`"},"state":"merged"}`, `{"merged":true,"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_sha":"`+factsHead+`","merge_commit_sha":"abcdef0123456789abcdef0123456789abcdef01","merged_at":"2026-01-01T00:00:00Z"}`)
	if facts, err := h.ReadPRFacts(context.Background(), testPR()); err != nil || facts.MergeCommitSHA != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Fatalf("valid merge proof = %+v, %v", facts, err)
	}
	tr.reply(`{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_branch":"topic","base_branch":"main","head_sha":"`+factsHead+`"},"state":"merged"}`, `{"merged":true,"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_sha":"`+factsHead+`","merge_commit_sha":"bad","merged_at":"2026-01-01T00:00:00Z"}`)
	_, err := h.ReadPRFacts(context.Background(), testPR())
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("error = %v", err)
	}
}

func TestFindOpenPRFacts(t *testing.T) {
	h, tr := newTestHost(t, Options{})
	tr.reply(`{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_branch":"topic","base_branch":"main","head_sha":"`+factsHead+`"}}`, `{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_branch":"topic","base_branch":"main","head_sha":"`+factsHead+`"},"state":"open"}`)
	facts, err := h.FindOpenPRFacts(context.Background(), "team/repo", "topic")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || facts[0].SourceBranch != "topic" {
		t.Fatalf("facts = %+v", facts)
	}
}

func TestFindOpenPRFactsRejectsWrongSource(t *testing.T) {
	h, tr := newTestHost(t, Options{})
	find := `{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_branch":"topic","base_branch":"main","head_sha":"` + factsHead + `"}}`
	view := `{"pr":{"number":7,"url":"https://git.example.com/team/repo/pulls/7","head_branch":"topic","base_branch":"main","head_sha":"` + factsHead + `"},"state":"open"}`
	tr.reply(find, view)
	if facts, err := h.FindOpenPRFacts(context.Background(), "team/repo", "topic"); err != nil || len(facts) != 1 {
		t.Fatalf("valid source = %+v, %v", facts, err)
	}
	calls := len(tr.calls)
	tr.reply(find, view)
	if _, err := h.FindOpenPRFacts(context.Background(), "someone/else", "topic"); err == nil || !strings.Contains(err.Error(), "source differs from invocation repository") {
		t.Fatalf("foreign source rejection = %v", err)
	}
	if len(tr.calls) != calls {
		t.Fatalf("wrong source caused plugin calls: %+v", tr.calls[calls:])
	}
}

func TestGetChecksRejectsReportedHeadDifferentFromRequest(t *testing.T) {
	h, tr := newTestHost(t, Options{})
	requested := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tr.reply(`{"checks":[{"name":"build","bucket":"pass","head_sha":"` + requested + `"}]}`)
	if checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "7", URL: testPR().URL, HeadSHA: requested}); err != nil || len(checks) != 1 || checks[0].HeadSHA != requested {
		t.Fatalf("valid current check = %+v, %v", checks, err)
	}
	tr.reply(`{"checks":[{"name":"build","bucket":"pass","head_sha":"` + factsHead + `"}]}`)
	checks, err := h.GetChecks(context.Background(), &scm.PR{Number: "7", URL: testPR().URL, HeadSHA: requested})
	if !errors.Is(err, scm.ErrHeadChanged) || len(checks) != 0 {
		t.Fatalf("stale reported head remained actionable: checks=%+v err=%v", checks, err)
	}
}
