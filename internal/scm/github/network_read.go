package github

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/netretry"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// ghRateLimitError types a rate limit with the evidence GitHub gave for it: a
// response's Retry-After header, or the reset time `gh api rate_limit`
// reported. netretry waits on it without spending an attempt.
type ghRateLimitError struct {
	error
	class      netretry.Class
	reset      time.Time
	retryAfter time.Duration
}

func (e *ghRateLimitError) Unwrap() error { return e.error }
func (e *ghRateLimitError) RetryClass() netretry.Class {
	if e.class == "" {
		return netretry.ClassRateLimit
	}
	return e.class
}
func (e *ghRateLimitError) RateLimitResetAt() time.Time        { return e.reset }
func (e *ghRateLimitError) RateLimitRetryAfter() time.Duration { return e.retryAfter }

// probeRateLimit reads `gh api rate_limit`, which GitHub does not count
// against any limit, when a call failed with the primary-limit message. The
// message alone is ambiguous: GitHub returned "API rate limit already
// exceeded" both when the hourly GraphQL budget really was at zero and while
// it still had thousands of points left (its secondary, per-minute or
// concurrency limit). A resource at zero waits for its reset; one with room is
// the secondary limit and backs off. A probe that cannot answer leaves the
// failure as it was, which backs off without a known reset.
func (h *Host) probeRateLimit(ctx context.Context, err error) error {
	if netretry.RateLimitClass(err) != netretry.ClassPrimaryRateLimit {
		return err
	}
	var typed *ghRateLimitError
	if errors.As(err, &typed) {
		return err
	}
	resource := "core"
	if strings.Contains(strings.ToLower(err.Error()), "graphql") {
		resource = "graphql"
	}
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "-X", "GET", "rate_limit")
	out, _, probeErr := h.ghReadAttempt(ctx, "", args)
	if probeErr != nil {
		return err
	}
	var limits struct {
		Resources map[string]*struct {
			Remaining *int   `json:"remaining"`
			Reset     *int64 `json:"reset"`
		} `json:"resources"`
	}
	if json.Unmarshal(out, &limits) != nil {
		return err
	}
	limit := limits.Resources[resource]
	if limit == nil || limit.Remaining == nil {
		return err
	}
	if *limit.Remaining > 0 {
		return &ghRateLimitError{error: err, class: netretry.ClassRateLimit}
	}
	if limit.Reset == nil || *limit.Reset <= 0 {
		return err
	}
	return &ghRateLimitError{error: err, class: netretry.ClassPrimaryRateLimit, reset: time.Unix(*limit.Reset, 0)}
}

func retryAfterHeader(value string) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if until := time.Until(at); until > 0 {
			return until
		}
	}
	return 0
}

var ghRateLimitStatus = regexp.MustCompile(`\bHTTP (?:403|429)\b`)

func ghAPIGet(args []string) bool {
	if len(args) == 0 || args[0] != "api" {
		return false
	}
	method, explicit := "GET", ""
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-X" || arg == "--method":
			if i+1 >= len(args) {
				return false
			}
			i++
			explicit = args[i]
		case strings.HasPrefix(arg, "--method="):
			explicit = strings.TrimPrefix(arg, "--method=")
		case strings.HasPrefix(arg, "-X"):
			explicit = strings.TrimPrefix(strings.TrimPrefix(arg, "-X"), "=")
		case arg == "graphql":
			return false
		case arg == "-f" || arg == "-F" || arg == "--field" || arg == "--raw-field" || arg == "--input":
			method = "POST"
			i++
		case strings.HasPrefix(arg, "--field=") || strings.HasPrefix(arg, "--raw-field=") || strings.HasPrefix(arg, "--input=") || strings.HasPrefix(arg, "-f") || strings.HasPrefix(arg, "-F"):
			method = "POST"
		case arg == "--hostname" || arg == "--header" || arg == "-H" || arg == "--jq" || arg == "-q" || arg == "--template" || arg == "-t" || arg == "--cache":
			i++
		}
	}
	if explicit != "" {
		method = explicit
	}
	return method == "GET"
}

func (h *Host) ghReadAttempt(ctx context.Context, stdin string, args []string) ([]byte, string, error) {
	run := func(callArgs []string) ([]byte, string, error) {
		cmd := h.cmd(ctx, "gh", callArgs...)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		var out []byte
		err := runGHCommand(ctx, cmd, func() error {
			var callErr error
			out, callErr = shellenv.OutputShellCommand(cmd)
			return callErr
		})
		return out, stderr.String(), err
	}
	out, detail, err := run(args)
	if err == nil || ctx.Err() != nil || !ghAPIGet(args) || !ghRateLimitStatus.MatchString(detail) {
		return out, detail, err
	}
	probeArgs := make([]string, 0, len(args)+1)
	for _, arg := range args {
		flag, _, _ := strings.Cut(arg, "=")
		if flag != "--paginate" && flag != "--slurp" {
			probeArgs = append(probeArgs, arg)
		}
	}
	probe, probeDetail, probeErr := run(append(probeArgs, "--include"))
	if probeErr != nil && ctx.Err() != nil {
		return out, probeDetail, probeErr
	}
	reader := bufio.NewReader(bytes.NewReader(probe))
	for {
		response, headerErr := http.ReadResponse(reader, nil)
		if headerErr != nil {
			break
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 ||
			(response.StatusCode == 403 || response.StatusCode == 429) && response.Header.Get("Retry-After") != "" {
			return out, detail, &ghRateLimitError{error: err, retryAfter: retryAfterHeader(response.Header.Get("Retry-After"))}
		}
		if response.StatusCode >= 200 {
			break
		}
	}
	return out, detail, err
}
