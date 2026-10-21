package github

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// Issue #100: gh exits 1 for both keyring timeouts and invalid credentials.
func TestIssue100KeyringTimeoutIsNotUnauthenticated(t *testing.T) {
	t.Parallel()
	const timeout = "github.com\n X Timeout trying to log in to github.com account test (keyring)\n - Active account: true"
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh auth status": {stderr: timeout, code: 1},
		"gh pr view 42 --json baseRefName --jq .baseRefName": {stderr: timeout, code: 1},
	}), nil, "", "")
	for name, read := range map[string]func() error{
		"auth": func() error { return host.Available(context.Background()) },
		"base": func() error { _, err := host.GetPRBaseBranch(context.Background(), &scm.PR{Number: "42"}); return err },
	} {
		t.Run(name, func(t *testing.T) {
			err := read()
			if !errors.Is(err, scm.ErrTransientTimeout) || errors.Is(err, scm.ErrUnauthenticated) || err == nil || !strings.Contains(err.Error(), "transient keyring timeout") || strings.Contains(err.Error(), "not authenticated") {
				t.Fatalf("error = %v, want a transient keyring timeout distinct from unauthenticated", err)
			}
		})
	}
}

func TestIssue100RealAuthFailureHasADifferentClass(t *testing.T) {
	t.Parallel()
	host := New(githubTestCmdFactory(map[string]githubTestResponse{"gh auth status": {stderr: "You are not logged into any GitHub hosts", code: 1}}), nil, "", "")
	err := host.Available(context.Background())
	if !errors.Is(err, scm.ErrUnauthenticated) || errors.Is(err, scm.ErrTransientTimeout) {
		t.Fatalf("auth class=%v", err)
	}
}

func TestIssue100PRFactsPreserveKeyringTimeoutClass(t *testing.T) {
	t.Parallel()
	const detail = "Timeout trying to log in to github.com account test (keyring)"
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api --hostname github.com --method GET repos/test/repo/pulls/42":                                                                    {stderr: detail, code: 1},
		"gh api --hostname github.com --method GET repos/test/repo/pulls -f state=open -f head=test:feature -f per_page=100 --paginate --slurp": {stderr: detail, code: 1},
	}), nil, "github.com", "test/repo")
	_, err := host.ReadPRFacts(context.Background(), &scm.PR{Number: "42"})
	if !errors.Is(err, scm.ErrTransientTimeout) {
		t.Fatalf("facts class=%v", err)
	}

	_, err = host.FindOpenPRFacts(context.Background(), "test/repo", "feature")
	if !errors.Is(err, scm.ErrTransientTimeout) {
		t.Fatalf("list class=%v", err)
	}
}

func TestIssue100UnrelatedTimeoutTextIsNotRetryable(t *testing.T) {
	for _, detail := range []string{"HTTP 401: Bad credentials", "network timeout", "Failed to log in to github.com account test (keyring)", "Timeout trying to log in to github.com account test (oauth_token)", "Timeout trying to log in to github.com account test (keyring)\nHTTP 401: Bad credentials"} {
		host := New(githubTestCmdFactory(map[string]githubTestResponse{"gh auth status": {stderr: detail, code: 1}}), nil, "", "")
		if err := host.Available(context.Background()); errors.Is(err, scm.ErrTransientTimeout) {
			t.Fatalf("misclassified %q", detail)
		}
	}
}
