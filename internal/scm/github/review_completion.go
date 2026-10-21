package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Codex does not publish check runs. Its review commit_id is authoritative;
// summary comments carry the reviewed commit explicitly. Never infer completion
// from arbitrary text, another author's comment, or a review of an older head.
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
	var activity []botReviewActivity
	for _, page := range pages {
		var rows []botReviewActivity
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
	var lastCompletion time.Time
	for _, review := range reviews {
		author, ok := scm.ReviewBotForLogin(scm.ProviderGitHub, review.User.Login)
		if !ok || author.AppSlug != bot.AppSlug {
			continue
		}
		if review.CommitID != pr.HeadSHA {
			continue
		}
		if review.State != "PENDING" && review.SubmittedAt.After(lastCompletion) {
			lastCompletion = review.SubmittedAt
		}
		switch review.State {
		case "PENDING":
			return "pending", nil
		case "COMMENTED", "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			state = "settled"
		default:
			return "", fmt.Errorf("unknown review completion state %q", review.State)
		}
	}
	for _, summary := range summaries {
		author, ok := scm.ReviewBotForLogin(scm.ProviderGitHub, summary.User.Login)
		if !ok || author.AppSlug != bot.AppSlug {
			continue
		}
		match := reviewedCommit.FindStringSubmatch(summary.Body)
		if len(match) == 2 && strings.HasPrefix(strings.ToLower(pr.HeadSHA), strings.ToLower(match[1])) {
			if summary.CreatedAt.After(lastCompletion) {
				lastCompletion = summary.CreatedAt
			}
			state = "settled"
		}
	}
	// The documented start signal is a bot-authored eyes reaction on an
	// explicit review request. Counts on the comment do not prove authorship.
	// Only a completion of the current head may retire pending requests; a
	// late completion of an older head cannot speak for the current review. A +1 reaction is the clean-review completion.
	for _, request := range summaries {
		if request.ID <= 0 || request.CreatedAt.IsZero() || !request.CreatedAt.After(lastCompletion) || !codexReviewRequest.MatchString(request.Body) {
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
			return "pending", nil
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
