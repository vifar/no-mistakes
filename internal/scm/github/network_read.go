package github

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/netretry"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

type ghRateLimitError struct {
	error
}

func (e *ghRateLimitError) Unwrap() error              { return e.error }
func (e *ghRateLimitError) RetryClass() netretry.Class { return netretry.ClassRateLimit }

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
			return out, detail, &ghRateLimitError{error: err}
		}
		if response.StatusCode >= 200 {
			break
		}
	}
	return out, detail, err
}
