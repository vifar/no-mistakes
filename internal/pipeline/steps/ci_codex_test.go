package steps

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCodexThreadFindingsFollowPolicy(t *testing.T) {
	for _, pending := range []string{"none", "review", "request"} {
		for _, mode := range []string{"on_failure", "always"} {
			for _, resolved := range []bool{false, true} {
				name := mode + "/unresolved"
				if resolved {
					name = mode + "/resolved"
				}
				if pending != "none" {
					name += "/pending-" + pending
				}
				t.Run(name, func(t *testing.T) {
					comments := []map[string]any{{"author": "chatgpt-codex-connector[bot]", "path": "internal/store.go", "line": 73, "body": "![P1 Badge](https://img.shields.io/badge/P1-orange) Fix the lost update", "resolved": resolved}}
					raw, _ := json.Marshal(comments)
					env := append(fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions"}]`), "FAKE_CLI_REVIEW_COMMENTS="+string(raw))
					sctx, ag, logs, ready, head := newGreenReviewBotContext(t, mode, env)
					if pending == "review" {
						sctx.Env = append(sctx.Env, `FAKE_CLI_PR_REVIEWS=[[{"user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"`+head+`","state":"PENDING"}]]`)
					}
					if pending == "request" {
						sctx.Env = append(sctx.Env, `FAKE_CLI_PR_SUMMARIES=[[{"id":7,"body":"@codex review","created_at":"2026-10-21T12:00:00Z"}]]`, `FAKE_CLI_PR_REACTIONS=[[{"user":{"login":"chatgpt-codex-connector[bot]"},"content":"eyes"}]]`)
					}
					step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return context.Canceled }}
					out, err := driveCI(t, step, sctx)
					blocks := mode == "always" && !resolved
					if blocks {
						if err != nil || out == nil || !out.NeedsApproval {
							t.Fatalf("out=%+v err=%v; want findings; logs=%v", out, err, *logs)
						}
						f, e := types.ParseFindingsJSON(out.Findings)
						if e != nil {
							t.Fatal(e)
						}
						if len(f.Items) != 1 {
							t.Fatalf("findings=%+v", f)
						}
						item := f.Items[0]
						if item.Category != "ci-review-bot" || item.Action != "ask-user" || item.File != "internal/store.go" || item.Line != 73 || item.Severity != "error" {
							t.Fatalf("lost finding contract: %+v", item)
						}
						if *ready {
							t.Fatal("unresolved Codex thread reported ready")
						}
					} else if mode == "always" && resolved && pending != "none" {
						if *ready || out != nil {
							t.Fatalf("resolved thread must leave the pending review waiting: ready=%v out=%+v", *ready, out)
						}
					} else if !*ready {
						t.Fatalf("expected readiness: out=%+v err=%v; logs=%v", out, err, *logs)
					}
					if len(ag.calls) != 0 {
						t.Fatal("review threads must never auto-fix")
					}
				})
			}
		}
	}
}

func TestCodexSeverityBadges(t *testing.T) {
	for _, tc := range []struct{ badge, want string }{{"P1-orange", "error"}, {"P2-yellow", "warning"}} {
		t.Run(tc.badge, func(t *testing.T) {
			bot, _ := scm.ReviewBotForApp("chatgpt-codex-connector")
			body := "![" + tc.badge[:2] + " Badge](https://img.shields.io/badge/" + tc.badge + "?logo=github) Fix this"
			got := reviewBotFindings(scm.ProviderGitHub, []reviewBotCheck{{bot: bot, green: true}}, []scm.ReviewComment{{Author: "chatgpt-codex-connector[bot]", Body: body}})
			if len(got) != 1 || got[0].Severity != tc.want {
				t.Fatalf("findings=%+v want severity %s", got, tc.want)
			}
		})
	}
}

func TestCodexExternalExclusionIsSpecific(t *testing.T) {
	for _, pattern := range []string{"Codex Review", "Greptile Review", "unrelated*"} {
		t.Run(pattern, func(t *testing.T) {
			env := append(fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions"}]`), `FAKE_CLI_REVIEW_COMMENTS=[{"author":"chatgpt-codex-connector[bot]","path":"codex.go","line":1,"body":"Codex finding"},{"author":"greptile-apps[bot]","path":"greptile.go","line":2,"body":"Greptile finding"}]`)
			sctx, _, _, ready, _ := newGreenReviewBotContext(t, "always", env)
			sctx.Config.CI.ExternalChecks = []string{pattern}
			step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return context.Canceled }}
			out, err := driveCI(t, step, sctx)
			if err != nil || out == nil {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			f, e := types.ParseFindingsJSON(out.Findings)
			if e != nil {
				t.Fatal(e)
			}
			want := 2
			if pattern != "unrelated*" {
				want = 1
			}
			if len(f.Items) != want {
				t.Fatalf("findings=%+v want %d", f, want)
			}
			for _, item := range f.Items {
				if pattern == "Codex Review" && item.File != "greptile.go" || pattern == "Greptile Review" && item.File != "codex.go" {
					t.Fatalf("wrong bot silenced: %+v", item)
				}
			}
			if *ready {
				t.Fatal("unexcluded bot must block")
			}
		})
	}
}

func TestCodexPendingReviewWithholdsReadinessWithoutReadErrorGate(t *testing.T) {
	for _, mode := range []string{"always", "on_failure"} {
		t.Run(mode, func(t *testing.T) {
			env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions"}]`)
			sctx, ag, logs, ready, head := newGreenReviewBotContext(t, mode, env)
			sctx.Env = append(sctx.Env, `FAKE_CLI_PR_REVIEWS=[[{"user":{"login":"chatgpt-codex-connector[bot]"},"commit_id":"`+head+`","state":"PENDING"}]]`)
			polls := 0
			step := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error {
				polls++
				if polls >= 7 || mode == "on_failure" {
					return context.Canceled
				}
				return nil
			}}
			out, err := driveCI(t, step, sctx)
			if out != nil || err == nil {
				t.Fatalf("pending should monitor without a gate: out=%+v err=%v logs=%v", out, err, *logs)
			}
			if *ready != (mode == "on_failure") {
				t.Fatalf("readiness=%v mode=%s logs=%v", *ready, mode, *logs)
			}
			if len(ag.calls) != 0 {
				t.Fatal("pending review triggered repair")
			}
		})
	}
}

func TestCodexBadgeParsingPreservesGreptileAndRejectsPlainText(t *testing.T) {
	codex, _ := scm.ReviewBotForApp("chatgpt-codex-connector")
	greptile, _ := scm.ReviewBotForApp("greptile-apps")
	for _, tc := range []struct {
		name       string
		bot        scm.ReviewBot
		body, want string
	}{
		{"codex P1", codex, "![P1 Badge](https://img.shields.io/badge/P1-orange)", "error"},
		{"codex P2", codex, "![P2 Badge](https://img.shields.io/badge/P2-yellow)", "warning"},
		{"plain text", codex, "P1 issue in some prose", "warning"},
		{"mismatched badge", codex, "![P1 Badge](https://img.shields.io/badge/P2-yellow)", "warning"},
		{"greptile unchanged", greptile, "![P1 Badge](https://img.shields.io/badge/P1-orange)", "warning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reviewBotCommentSeverity(tc.bot, tc.body); got != tc.want {
				t.Fatalf("severity=%s want %s", got, tc.want)
			}
		})
	}
}
