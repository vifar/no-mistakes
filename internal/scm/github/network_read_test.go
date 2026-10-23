package github

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/netretry"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestAPIReadHeaderEvidence(t *testing.T) {
	for _, combined := range []bool{false, true} {
		for _, scenario := range []string{"recover", "exhausted", "no header", "primary limit", "probe succeeds", "malformed headers", "probe timeout", "CRLF", "slurp headers"} {
			t.Run(scenario+map[bool]string{false: "/stdout", true: "/combined"}[combined], func(t *testing.T) {
				reads, probes := 0, 0
				args := []string{"api", "--hostname", "ghe.example.com", "--method", "GET", "repos/test/repo/pulls/7"}
				failure := "gh: You have exceeded a secondary rate limit (HTTP 403)\n"
				body := "{\"number\":7,\"body\":\"permission denied tests\"}\n\r\n\r\n"
				host := fastRetryHost(func(ctx context.Context, name string, got ...string) *exec.Cmd {
					step := ghScriptStep{stderr: failure, code: 1}
					if len(got) == len(args)+1 && got[len(got)-1] == "--include" {
						if !reflect.DeepEqual(got[:len(args)], args) {
							t.Fatalf("probe changed the read: %v", got)
						}
						probes++
						headers := "HTTP/2.0 403 Forbidden\nRetry-After: 60\nX-Private: header-secret\n\n"
						if scenario == "no header" {
							headers = "HTTP/2.0 403 Forbidden\nX-Private: header-secret\n\n"
						}
						if scenario == "primary limit" {
							step.stderr = "gh: API rate limit exceeded (HTTP 403)\n"
						}
						if scenario == "CRLF" {
							headers = "HTTP/2.0 403 Forbidden\nRetry-After: 60\r\nX-Private: header-secret\r\n\r\n"
						}
						if scenario == "slurp headers" {
							headers = "[" + headers
						}
						step.stdout = headers + body
						if scenario == "probe succeeds" {
							step = ghScriptStep{stdout: "HTTP/2.0 200 OK\nX-Private: header-secret\n\n" + body}
						}
						if scenario == "malformed headers" {
							step.stdout = "HTTP/2.0 403 Forbidden\nRetry-After: 60\nX-Private: header-secret"
						}
						if scenario == "probe timeout" {
							step = ghScriptStep{stderr: ghReadTimeout, code: 1}
						}
					} else {
						if !reflect.DeepEqual(got, args) {
							t.Fatalf("unexpected read: %v", got)
						}
						reads++
						if reads > 1 && scenario != "exhausted" {
							step = ghScriptStep{stdout: body}
						}
					}
					factory, _ := sequencedGHFactory(t, []ghScriptStep{step})
					return factory(ctx, name, got...)
				})
				var out []byte
				var detail string
				var err error
				if combined {
					out, err = host.ghCombined(context.Background(), "read PR facts", "", args...)
				} else {
					out, detail, err = host.ghStdout(context.Background(), "read PR facts", "", args...)
				}
				if strings.Contains(string(out)+detail, "HTTP/") || strings.Contains(string(out)+detail, "Retry-After") || strings.Contains(string(out)+detail, "header-secret") || err != nil && (strings.Contains(err.Error(), "header-secret") || strings.Contains(err.Error(), "Retry-After")) {
					t.Fatalf("headers escaped: output=%q detail=%q err=%v", out, detail, err)
				}
				switch scenario {
				case "exhausted":
					if err == nil || reads != 3 || probes != 3 || netretry.Describe(err) != string(netretry.ClassRateLimit) {
						t.Fatalf("reads=%d probes=%d err=%v, want bounded secondary-limit failure", reads, probes, err)
					}
				case "no header", "primary limit", "malformed headers", "probe succeeds":
					if err == nil || reads != 1 || probes != 1 || netretry.Retryable(err) {
						t.Fatalf("reads=%d probes=%d err=%v, want no retry without qualifying evidence", reads, probes, err)
					}
				default:
					if err != nil || string(out) != body || detail != "" || reads != 2 || probes != 1 {
						t.Fatalf("reads=%d probes=%d output=%q detail=%q err=%v, want exact response body", reads, probes, out, detail, err)
					}
				}
			})
		}
	}
}

func TestAPIHeaderProbeIsLimitedToGET(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		probe bool
	}{
		{[]string{"api", "rate_limit"}, true},
		{[]string{"api", "rate_limit", "-X", "GET"}, true},
		{[]string{"api", "--method=GET", "rate_limit"}, true},
		{[]string{"api", "-XGET", "rate_limit"}, true},
		{[]string{"api", "-f", "page=1", "--method", "GET", "rate_limit"}, true},
		{[]string{"api", "--method", "POST", "repos/test/repo/issues"}, false},
		{[]string{"api", "-X", "PATCH", "repos/test/repo/issues/7"}, false},
		{[]string{"api", "--method=PUT", "repos/test/repo/contents/file"}, false},
		{[]string{"api", "-XDELETE", "repos/test/repo/issues/7"}, false},
		{[]string{"api", "repos/test/repo/issues", "-f", "title=hello"}, false},
		{[]string{"api", "repos/test/repo/issues", "--field=title=hello"}, false},
		{[]string{"api", "repos/test/repo/issues", "--input", "-"}, false},
		{[]string{"api", "graphql", "-f", "query=query { viewer { login } }"}, false},
		{[]string{"pr", "create", "--title", "hello", "--body-file", "-"}, false},
		{[]string{"pr", "edit", "7", "--body-file", "-"}, false},
		{[]string{"run", "rerun", "9"}, false},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			for _, combined := range []bool{false, true} {
				factory, calls := sequencedGHFactory(t, []ghScriptStep{
					{stderr: "gh: HTTP 429 Too Many Requests", code: 1},
					{stdout: "HTTP/2.0 429 Too Many Requests\n\n{}", stderr: "gh: HTTP 429 Too Many Requests", code: 1},
				})
				host := fastRetryHost(factory)
				var err error
				if combined {
					_, err = host.ghCombined(context.Background(), "test", "", tc.args...)
				} else {
					_, _, err = host.ghStdout(context.Background(), "test", "", tc.args...)
				}
				want := 1
				if tc.probe {
					want = 2
				}
				if err == nil || calls() != want {
					t.Fatalf("combined=%v calls=%d err=%v, want %d calls", combined, calls(), err, want)
				}
			}
		})
	}
}

func TestAvailableProbesOnlyAmbiguousAuthFailures(t *testing.T) {
	for _, scenario := range []string{"recovered", "exhausted", "probe succeeds", "probe forbidden", "Bad credentials", "not logged in", "Failed to log in", "token is invalid", "Permission denied"} {
		t.Run(scenario, func(t *testing.T) {
			auth, probes := 0, 0
			host := fastRetryHost(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				step := ghScriptStep{}
				if reflect.DeepEqual(args, []string{"auth", "status", "--hostname", "ghe.example.com"}) {
					auth++
					if scenario != "recovered" || auth == 1 {
						step = ghScriptStep{stderr: "gh: could not determine account status", code: 1}
						if scenario != "recovered" && scenario != "exhausted" && !strings.HasPrefix(scenario, "probe ") {
							step.stderr = scenario
						}
					}
				} else {
					if !reflect.DeepEqual(args, []string{"api", "--hostname", "ghe.example.com", "-X", "GET", "rate_limit"}) {
						t.Fatalf("unexpected availability probe: %v", args)
					}
					probes++
					if scenario == "recovered" || scenario == "exhausted" {
						step = ghScriptStep{stderr: ghReadTimeout, code: 1}
					} else if scenario == "probe forbidden" {
						step = ghScriptStep{stderr: "HTTP 401: Bad credentials", code: 1}
					} else {
						step.stdout = `{"rate":{"remaining":100}}`
					}
				}
				factory, _ := sequencedGHFactory(t, []ghScriptStep{step})
				return factory(ctx, name, args...)
			})
			host.host = "ghe.example.com"
			err := host.Available(context.Background())
			switch scenario {
			case "recovered":
				if err != nil || auth != 2 || probes != 1 {
					t.Fatalf("auth=%d probes=%d err=%v, want recovery within the availability retry", auth, probes, err)
				}
			case "exhausted":
				if err == nil || errors.Is(err, scm.ErrUnauthenticated) || !netretry.Retryable(err) || auth != 3 || probes != 3 {
					t.Fatalf("auth=%d probes=%d err=%v, want bounded transport failure without unauthenticated verdict", auth, probes, err)
				}
			default:
				wantProbes := 0
				if strings.HasPrefix(scenario, "probe ") {
					wantProbes = 1
				}
				if !errors.Is(err, scm.ErrUnauthenticated) || auth != 1 || probes != wantProbes {
					t.Fatalf("auth=%d probes=%d err=%v, want original unauthenticated verdict", auth, probes, err)
				}
			}
		})
	}
}
