package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Codex does not publish check runs. Its review commit_id is authoritative;
// summary comments carry the reviewed commit explicitly. Never infer completion
// from arbitrary text or another author's comment.
var reviewedCommit = regexp.MustCompile("(?m)\\*\\*Reviewed commit:\\*\\* `([0-9a-fA-F]{7,40})`")

type botReviewActivity struct {
	ID      int64  `json:"id"`
	Content string `json:"content"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	CommitID    string    `json:"commit_id"`
	State       string    `json:"state"`
	Body        string    `json:"body"`
	SubmittedAt time.Time `json:"submitted_at"`
	CreatedAt   time.Time `json:"created_at"`
}

func (h *Host) reviewActivity(ctx context.Context, endpoint string) ([]botReviewActivity, error) {
	return readReviewActivity[botReviewActivity](ctx, h, endpoint)
}

func readReviewActivity[T any](ctx context.Context, h *Host, endpoint string) ([]T, error) {
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "--method", "GET", endpoint, "--paginate", "--slurp")
	out, detail, err := stdoutOnly(h.cmd(ctx, "gh", args...))
	if err != nil {
		return nil, fmt.Errorf("read review completion: %s: %w", detail, err)
	}
	var pages []json.RawMessage
	if err = json.Unmarshal(out, &pages); err != nil || pages == nil {
		return nil, errors.New("invalid review completion pages")
	}
	var activity []T
	for _, page := range pages {
		var rows []T
		if err = json.Unmarshal(page, &rows); err != nil || rows == nil {
			return nil, errors.New("invalid review completion page")
		}
		activity = append(activity, rows...)
	}
	return activity, nil
}

// ReviewBotPending implements scm.ReviewBotPendingHost. A provider-visible
// PENDING review on this head holds readiness; a submitted review or its summary
// settles it. An absent bot does not become a required review gate.
func (h *Host) ReviewBotPending(ctx context.Context, pr *scm.PR, bot scm.ReviewBot) (bool, error) {
	state, err := h.reviewBotState(ctx, pr, bot)
	return state == "pending", err
}

func (h *Host) reviewBotState(ctx context.Context, pr *scm.PR, bot scm.ReviewBot) (string, error) {
	if !bot.CompletionFromReview {
		return "absent", nil
	}
	if pr == nil || strings.TrimSpace(pr.HeadSHA) == "" {
		return "", errors.New("review completion requires a PR head")
	}
	repo := h.repoSlug()
	if repo == "" {
		repo = RepoSlug(pr.URL)
	}
	number := pr.Number
	if number == "" {
		n, err := parsePullRequestURL(pr.URL, h.host, repo)
		if err != nil {
			return "", err
		}
		number = strconv.Itoa(n)
	}
	n, err := strconv.Atoi(number)
	if repo == "" || err != nil || n <= 0 {
		return "", errors.New("review completion requires a repository and positive PR number")
	}
	reviews, err := h.reviewActivity(ctx, "repos/"+repo+"/pulls/"+number+"/reviews")
	if err != nil {
		return "", err
	}
	summaries, err := h.reviewActivity(ctx, "repos/"+repo+"/issues/"+number+"/comments")
	if err != nil {
		return "", err
	}
	state := "absent"
	type completion struct {
		head string
		at   time.Time
	}
	var completions []completion
	for _, review := range reviews {
		author, ok := scm.ReviewBotForLogin(scm.ProviderGitHub, review.User.Login)
		if !ok || author.AppSlug != bot.AppSlug {
			continue
		}
		switch review.State {
		case "PENDING":
			if review.CommitID == pr.HeadSHA {
				return "pending", nil
			}
		case "COMMENTED", "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			if review.CommitID == pr.HeadSHA {
				state = "settled"
			}
			if isFullCommitSHA(review.CommitID) && !review.SubmittedAt.IsZero() {
				completions = append(completions, completion{review.CommitID, review.SubmittedAt})
			}
		default:
			if review.CommitID == pr.HeadSHA {
				return "", fmt.Errorf("unknown review completion state %q", review.State)
			}
		}
	}
	for _, summary := range summaries {
		author, ok := scm.ReviewBotForLogin(scm.ProviderGitHub, summary.User.Login)
		if !ok || author.AppSlug != bot.AppSlug {
			continue
		}
		match := reviewedCommit.FindStringSubmatch(summary.Body)
		if len(match) == 2 {
			head := strings.ToLower(match[1])
			if !summary.CreatedAt.IsZero() {
				completions = append(completions, completion{head, summary.CreatedAt})
			}
			if strings.HasPrefix(pr.HeadSHA, head) {
				state = "settled"
			}
		}
	}
	var pushes []reviewHeadPush
	historyRead := false
	for _, request := range summaries {
		if request.ID <= 0 || request.CreatedAt.IsZero() || !codexReviewRequest.MatchString(request.Body) {
			continue
		}
		reactions, err := h.reviewActivity(ctx, "repos/"+repo+"/issues/comments/"+strconv.FormatInt(request.ID, 10)+"/reactions")
		if err != nil {
			return "", err
		}
		eyes, clean := false, false
		for _, reaction := range reactions {
			author, ok := scm.ReviewBotForLogin(scm.ProviderGitHub, reaction.User.Login)
			if !ok || author.AppSlug != bot.AppSlug {
				continue
			}
			eyes = eyes || reaction.Content == "eyes"
			clean = clean || reaction.Content == "+1"
		}
		if eyes && !clean {
			settled := false
			for _, completed := range completions {
				if !completed.at.After(request.CreatedAt) {
					continue
				}
				if !historyRead {
					pushes, err = h.reviewRequestHistory(ctx, &scm.PR{Number: number, URL: pr.URL, HeadSHA: pr.HeadSHA})
					if err != nil {
						return "", err
					}
					historyRead = true
				}
				requestedHead := reviewRequestHead(pushes, request.CreatedAt)
				if requestedHead != "" && strings.HasPrefix(requestedHead, completed.head) {
					settled = true
					break
				}
			}
			if !settled {
				return "pending", nil
			}
			state = "settled"
		}
		if clean {
			state = "settled"
		}
	}
	return state, nil
}

// Other mentions ask Codex to work; they are not review readiness signals.
// https://developers.openai.com/codex/integrations/github/
var codexReviewRequest = regexp.MustCompile(`(?im)^\s*@codex[ \t]+review(?:[ \t].*)?\s*$`)

type reviewHeadPush struct {
	Ref      string    `json:"ref"`
	Before   string    `json:"before"`
	After    string    `json:"after"`
	PushedAt time.Time `json:"pushed_at"`
}

func (h *Host) reviewRequestHistory(ctx context.Context, pr *scm.PR) ([]reviewHeadPush, error) {
	facts, err := h.ReadPRFacts(ctx, pr)
	if err != nil {
		return nil, err
	}
	if facts.HeadSHA != pr.HeadSHA {
		return nil, errors.New("PR head changed while attributing review requests")
	}
	ref := "refs/heads/" + facts.SourceBranch
	pushes, err := readReviewActivity[reviewHeadPush](ctx, h, "repos/"+facts.SourceRepository+"/activity?ref="+url.QueryEscape(ref)+"&per_page=100")
	if err != nil {
		return nil, err
	}
	for _, push := range pushes {
		if push.Ref != ref || push.PushedAt.IsZero() || !isFullCommitSHA(push.Before) || !isFullCommitSHA(push.After) {
			return nil, errors.New("invalid review request head history")
		}
	}
	sort.Slice(pushes, func(i, j int) bool { return pushes[i].PushedAt.Before(pushes[j].PushedAt) })
	for i := 1; i < len(pushes); i++ {
		if !pushes[i].PushedAt.After(pushes[i-1].PushedAt) || pushes[i].Before != pushes[i-1].After {
			return nil, errors.New("ambiguous review request head history")
		}
	}
	if len(pushes) > 0 && pushes[len(pushes)-1].After != pr.HeadSHA {
		return nil, errors.New("review request history does not reach the PR head")
	}
	return pushes, nil
}

func reviewRequestHead(pushes []reviewHeadPush, at time.Time) string {
	head := ""
	for _, push := range pushes {
		if push.PushedAt.Equal(at) {
			return ""
		}
		if push.PushedAt.After(at) {
			break
		}
		head = push.After
	}
	if head == strings.Repeat("0", 40) {
		return ""
	}
	return head
}
