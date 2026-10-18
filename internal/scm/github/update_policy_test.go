package github

import (
	"context"
	"testing"
)

func TestRequiresUpToDateUsesAllProtectionSources(t *testing.T) {
	for _, tc := range []struct {
		name, classic, rules string
		want, fail           bool
	}{
		{"non-strict", `{"strict":false}`, `[[]]`, false, false},
		{"classic strict", `{"strict":true}`, `[[]]`, true, false},
		{"later rules page", `{"strict":false}`, `[[],[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":true}}]]`, true, false},
		{"both non-strict", `{"strict":false}`, `[[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false}}]]`, false, false},
		{"queue", `{"strict":false}`, `[[{"type":"merge_queue"}]]`, true, false},
		{"missing strict", `{}`, `[[]]`, false, true},
		{"missing rules strict", `{"strict":false}`, `[[{"type":"required_status_checks","parameters":{}}]]`, false, true},
		{"null pages", `{"strict":false}`, `null`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := New(githubTestCmdFactory(map[string]githubTestResponse{protectionCmd: {stdout: tc.classic}, rulesCmd: {stdout: tc.rules}}), nil, "", "test/repo")
			got, err := host.RequiresUpToDate(context.Background(), nil, "main")
			if (err != nil) != tc.fail || !tc.fail && got != tc.want {
				t.Fatalf("policy=%t error=%v want=%t fail=%t", got, err, tc.want, tc.fail)
			}
		})
	}
	for _, source := range []string{protectionCmd, rulesCmd} {
		t.Run("unreadable "+source, func(t *testing.T) {
			replies := map[string]githubTestResponse{protectionCmd: {stdout: `{"strict":false}`}, rulesCmd: {stdout: `[[]]`}}
			replies[source] = githubTestResponse{stderr: "HTTP 403", code: 1}
			host := New(githubTestCmdFactory(replies), nil, "", "test/repo")
			if _, err := host.RequiresUpToDate(context.Background(), nil, "main"); err == nil {
				t.Fatal("unreadable protection treated as false")
			}
		})
	}
}
