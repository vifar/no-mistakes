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
		for _, scenario := range []string{"recover", "exhausted", "no header", "primary limit", "probe 200", "probe 204", "malformed headers", "probe timeout", "CRLF", "informational headers", "header in body", "probe unauthorized", "probe server failure", "explicit pagination flags"} {
			t.Run(scenario+map[bool]string{false: "/stdout", true: "/combined"}[combined], func(t *testing.T) {
				reads, probes := 0, 0
				probeArgs := []string{"api", "--hostname", "ghe.example.com", "--method", "GET", "repos/test/repo/pulls", "-f", "per_page=100"}
				args := append(append([]string{}, probeArgs...), "--paginate", "--slurp")
				if scenario == "explicit pagination flags" {
					args = append(append([]string{}, probeArgs...), "--paginate=true", "--slurp=true")
				}
				probeArgs = append(probeArgs, "--include")
				failure := "gh: You have exceeded a secondary rate limit (HTTP 403)\n"
				body := "[[{\"number\":7,\"body\":\"permission denied tests\"}],[{\"number\":8}]]\n"
				host := fastRetryHost(func(ctx context.Context, name string, got ...string) *exec.Cmd {
					step := ghScriptStep{stdout: "[[{\"number\":7}]", stderr: failure, code: 1}
					if reflect.DeepEqual(got, probeArgs) {
						probes++
						headers := "HTTP/2.0 403 Forbidden\nRetry-After: 60\nX-Private: header-secret\n\n"
						switch scenario {
						case "no header", "primary limit", "header in body":
							headers = "HTTP/2.0 403 Forbidden\nX-Private: header-secret\n\n"
						case "CRLF":
							headers = "HTTP/2.0 403 Forbidden\nRetry-After: 60\r\nX-Private: header-secret\r\n\r\n"
						case "informational headers":
							headers = "HTTP/1.1 103 Early Hints\r\n\r\n" + headers
						case "probe 200":
							headers = "HTTP/2.0 200 OK\nX-Private: header-secret\n\n"
							step.code, step.stderr = 0, ""
						case "probe 204":
							headers = "HTTP/2.0 204 No Content\nX-Private: header-secret\n\n"
							step.code, step.stderr = 0, ""
						case "probe unauthorized":
							headers = "HTTP/2.0 401 Unauthorized\nRetry-After: 60\n\n"
						case "probe server failure":
							headers = "HTTP/2.0 503 Service Unavailable\n\n"
						}
						step.stdout = headers + `{"probe_body":"must-never-be-returned","Retry-After":60}`
						if scenario == "primary limit" {
							step.stderr = "gh: API rate limit exceeded (HTTP 403)\n"
						}
						if scenario == "malformed headers" {
							step.stdout = "HTTP/2.0 403 Forbidden\nRetry-After: 60\nX-Private: header-secret"
						}
						if scenario == "probe timeout" {
							step = ghScriptStep{stderr: ghReadTimeout, code: 1}
						}
					} else {
						if !reflect.DeepEqual(got, args) {
							t.Fatalf("unexpected command arguments: %v", got)
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
				reported := string(out) + detail
				if err != nil {
					reported += err.Error()
				}
				for _, private := range []string{"HTTP/", "header-secret", "Retry-After", "must-never-be-returned"} {
					if strings.Contains(reported, private) {
						t.Fatalf("probe escaped into response or error: %q", reported)
					}
				}
				switch scenario {
				case "exhausted":
					// A secondary limit is waited out, not spent from the three
					// attempts, and the wait backs off until its budget is gone.
					if err == nil || reads < 4 || reads > 8 || reads != probes || !errors.Is(err, netretry.ErrRateLimitBudgetSpent) || netretry.Describe(err) != string(netretry.ClassRateLimit) {
						t.Fatalf("reads=%d probes=%d err=%v, want a secondary-limit wait bounded by its budget", reads, probes, err)
					}
				default:
					if err != nil || string(out) != body || detail != "" || reads != 2 || probes != 1 {
						t.Fatalf("reads=%d probes=%d output=%q detail=%q err=%v, want exact response from retried paginated command", reads, probes, out, detail, err)
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
					{stderr: "gh: HTTP 403 Forbidden", code: 1},
					{stdout: "HTTP/2.0 403 Forbidden\n\n{}", stderr: "gh: HTTP 403 Forbidden", code: 1},
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
