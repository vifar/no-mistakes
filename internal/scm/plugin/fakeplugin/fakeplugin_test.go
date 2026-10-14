package fakeplugin

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestChecksNeverUseRequestedHeadAsForgeEvidence(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := State{
		Checks: []Check{{Name: "missing-head"}, {Name: "stale-head", HeadSHA: "stale-check-sha"}},
		PRs:    []PR{{Number: "7", URL: "https://forge.example/team/repo/pulls/7", HeadBranch: "feature", BaseBranch: "main", State: "open"}},
	}
	if err := Save(statePath, state); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"pr", "checks", "7",
		"--plugin=ssm", "--repo=team/repo", "--host=forge.example",
		"--raw-host=forge.example", "--remote-url=https://forge.example/team/repo",
		"--head-sha=requested-head-sha", "--json",
	}
	var stdout bytes.Buffer
	if code := Main(args, bytes.NewReader(nil), &stdout, statePath, ""); code != 0 {
		t.Fatalf("Main exit = %d, response: %s", code, stdout.String())
	}
	var response struct {
		Checks []Check `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode checks response: %v: %s", err, stdout.String())
	}
	if len(response.Checks) != 2 {
		t.Fatalf("checks = %+v", response.Checks)
	}
	if response.Checks[0].HeadSHA != "" {
		t.Fatalf("missing observed head was fabricated as %q", response.Checks[0].HeadSHA)
	}
	if response.Checks[1].HeadSHA != "stale-check-sha" {
		t.Fatalf("explicit stale check head = %q", response.Checks[1].HeadSHA)
	}
	after, err := Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if after.PRs[0].HeadSHA != "" {
		t.Fatalf("requested head populated missing PR head: %q", after.PRs[0].HeadSHA)
	}
}
