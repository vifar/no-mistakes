package scm

import (
	"context"
	"errors"
	"testing"
)

type reviewSignalTestHost struct {
	Host
	provider Provider
	pending  bool
	comments []ReviewComment
	reads    int
}

func (h *reviewSignalTestHost) Provider() Provider         { return h.provider }
func (h *reviewSignalTestHost) Capabilities() Capabilities { return Capabilities{ReviewComments: true} }
func (h *reviewSignalTestHost) ReviewBotPending(context.Context, *PR, ReviewBot) (bool, error) {
	return h.pending, nil
}
func (h *reviewSignalTestHost) GetReviewComments(context.Context, *PR) ([]ReviewComment, error) {
	h.reads++
	return h.comments, nil
}

func TestCodexPendingDoesNotWithholdAnotherBotsFindings(t *testing.T) {
	h := &reviewSignalTestHost{provider: ProviderGitHub, pending: true, comments: []ReviewComment{
		{Author: "greptile-apps[bot]", Path: "greptile.go", Line: 1},
		{Author: "chatgpt-codex-connector[bot]", Path: "codex.go", Line: 2},
	}}
	got, err := ReadReadinessReviewBotComments(context.Background(), h, &PR{}, CheckScopePolicy{ReviewBotComments: "always"}, nil)
	if err != nil || len(got) != 1 || got[0].Path != "greptile.go" {
		t.Fatalf("comments=%+v err=%v; the settled Greptile finding must still escalate", got, err)
	}
	h.comments = h.comments[1:]
	got, err = ReadReadinessReviewBotComments(context.Background(), h, &PR{}, CheckScopePolicy{ReviewBotComments: "always"}, nil)
	if len(got) != 0 || !errors.Is(err, ErrReviewBotPending) {
		t.Fatalf("comments=%+v err=%v; want pending without old Codex comments", got, err)
	}
}

func TestCodexDoesNotAddGitLabReadinessReads(t *testing.T) {
	h := &reviewSignalTestHost{provider: ProviderGitLab, pending: true}
	got, err := ReadReadinessReviewBotComments(context.Background(), h, &PR{}, CheckScopePolicy{ReviewBotComments: "always"}, nil, Check{App: "greptile-apps", Name: "Greptile Review"})
	if len(got) != 0 || err != nil || h.reads != 0 {
		t.Fatalf("comments=%+v err=%v reads=%d; Codex is GitHub-only", got, err, h.reads)
	}
}
