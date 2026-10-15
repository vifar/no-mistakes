package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ghForge is the owned fake forge behind the stateful gh stub: the commit
// check rollup, branch protection, rulesets, Actions runs, and review threads
// for the PR head. It lives in its own JSON file ($FAKEAGENT_GH_FORGE) that the
// stub re-reads on every gh call, so an e2e test can rewrite it atomically
// while a real daemon is polling and have the next poll observe the new
// results. When the variable is unset or the file does not exist the stub keeps
// its original "forge with no checks" behavior.
type ghForge struct {
	// Checks are CheckRun nodes of the commit's statusCheckRollup. With no
	// Checks and no Statuses the rollup is null, like a commit nothing ran on.
	Checks   []ghForgeCheck  `json:"checks,omitempty"`
	Statuses []ghForgeStatus `json:"statuses,omitempty"`
	// Protection answers branches/<b>/protection/required_status_checks. Nil
	// answers 404 "Branch not protected" (no classic protection).
	Protection *ghForgeEndpoint `json:"protection,omitempty"`
	// Rules answers rules/branches/<b>. Nil answers one empty page.
	Rules *ghForgeRules `json:"rules,omitempty"`
	// Runs are verbatim actions/runs workflow_runs objects (id, name,
	// run_attempt, status, conclusion, event, head_branch, ...).
	Runs []json.RawMessage `json:"runs,omitempty"`
	// AttemptJobs maps "<runID>/<attempt>" to the job (check run) IDs of that
	// attempt, answering actions/runs/<id>/attempts/<n>/jobs.
	AttemptJobs map[string][]int64 `json:"attempt_jobs,omitempty"`
	// ReviewThreads are the PR's review threads.
	ReviewThreads []ghForgeThread `json:"review_threads,omitempty"`
	// Mergeable and MergeStateStatus are the PR's mergeability; empty means
	// MERGEABLE and CLEAN. CONFLICTING makes the CI step raise a merge-conflict
	// finding, and BEHIND (with ci.rebase_behind) a behind-base one.
	Mergeable        string `json:"mergeable,omitempty"`
	MergeStateStatus string `json:"merge_state_status,omitempty"`
}

type ghForgeCheck struct {
	DatabaseID  int64  `json:"database_id,omitempty"`
	Name        string `json:"name"`
	Status      string `json:"status,omitempty"`     // default COMPLETED
	Conclusion  string `json:"conclusion,omitempty"` // SUCCESS, FAILURE, ...
	CompletedAt string `json:"completed_at,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	DetailsURL  string `json:"details_url,omitempty"`
	AppSlug     string `json:"app_slug,omitempty"`
	AppID       int64  `json:"app_id,omitempty"`
	Workflow    string `json:"workflow,omitempty"`
}

type ghForgeStatus struct {
	ID        string `json:"id,omitempty"`
	Context   string `json:"context"`
	State     string `json:"state"` // SUCCESS, FAILURE, PENDING, ...
	TargetURL string `json:"target_url,omitempty"`
}

// ghForgeEndpoint is one REST answer: a non-zero Status other than 200 fails
// the call as gh would ("gh: <Message> (HTTP <Status>)"); otherwise Body is
// printed.
type ghForgeEndpoint struct {
	Status  int             `json:"status,omitempty"`
	Message string          `json:"message,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
}

// ghForgeRules answers the rules endpoint; Pages are the paginated arrays that
// `gh api --paginate --slurp` wraps in one outer array.
type ghForgeRules struct {
	Status  int               `json:"status,omitempty"`
	Message string            `json:"message,omitempty"`
	Pages   []json.RawMessage `json:"pages,omitempty"`
}

type ghForgeThread struct {
	Author   string `json:"author"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Body     string `json:"body"`
	Resolved bool   `json:"resolved,omitempty"`
}

// loadGhForge reads $FAKEAGENT_GH_FORGE. It returns nil when no forge file is
// configured, so the stub's legacy answers apply.
func loadGhForge() (*ghForge, error) {
	path := os.Getenv("FAKEAGENT_GH_FORGE")
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	forge := &ghForge{}
	if err := json.Unmarshal(data, forge); err != nil {
		return nil, fmt.Errorf("parse forge: %w", err)
	}
	return forge, nil
}

// graphqlChecks renders the commit check rollup response.
func (f *ghForge) graphqlChecks() string {
	if len(f.Checks) == 0 && len(f.Statuses) == 0 {
		return `{"data":{"repository":{"object":{"statusCheckRollup":null}}}}`
	}
	nodes := make([]map[string]any, 0, len(f.Checks)+len(f.Statuses))
	for _, c := range f.Checks {
		status := c.Status
		if status == "" {
			status = "COMPLETED"
		}
		node := map[string]any{
			"__typename": "CheckRun", "name": c.Name, "status": status,
			"conclusion": c.Conclusion, "completedAt": c.CompletedAt,
			"startedAt": c.StartedAt, "detailsUrl": c.DetailsURL,
		}
		if c.DatabaseID != 0 {
			node["databaseId"] = c.DatabaseID
		}
		suite := map[string]any{}
		if c.AppSlug != "" || c.AppID != 0 {
			app := map[string]any{"slug": c.AppSlug}
			if c.AppID != 0 {
				app["databaseId"] = c.AppID
			}
			suite["app"] = app
		}
		if c.Workflow != "" {
			suite["workflowRun"] = map[string]any{"workflow": map[string]any{"name": c.Workflow}}
		}
		if len(suite) > 0 {
			node["checkSuite"] = suite
		}
		nodes = append(nodes, node)
	}
	for _, s := range f.Statuses {
		id := s.ID
		if id == "" {
			id = "status-" + s.Context
		}
		nodes = append(nodes, map[string]any{"__typename": "StatusContext", "id": id, "context": s.Context, "state": s.State, "targetUrl": s.TargetURL})
	}
	data, _ := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"object": map[string]any{
		"statusCheckRollup": map[string]any{"contexts": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}}},
	}}}})
	return string(data)
}

// graphqlReviewThreads renders the reviewThreads response.
func (f *ghForge) graphqlReviewThreads() string {
	threads := make([]map[string]any, 0, len(f.ReviewThreads))
	for i, t := range f.ReviewThreads {
		threads = append(threads, map[string]any{
			"isResolved": t.Resolved,
			"comments": map[string]any{"nodes": []map[string]any{{
				"databaseId": i + 1, "body": t.Body, "path": t.Path, "line": t.Line,
				"url":       fmt.Sprintf("https://github.com/example/repo/pull/1#discussion_r%d", i+1),
				"createdAt": "2026-10-14T00:00:00Z",
				"author":    map[string]any{"login": t.Author},
			}}},
		})
	}
	data, _ := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
		"reviewThreads": map[string]any{"nodes": threads, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}},
	}}}})
	return string(data)
}

// rest answers the GET endpoints the check-scope code reads. handled is false
// for an endpoint the forge does not model.
func (f *ghForge) rest(repo, endpoint string, args []string) (out string, errText string, handled bool) {
	path, _, _ := strings.Cut(endpoint, "?")
	prefix := "repos/" + repo + "/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	path = strings.TrimPrefix(path, prefix)
	httpError := func(status int, message string) string {
		return fmt.Sprintf("gh: %s (HTTP %d)", message, status)
	}
	switch {
	case strings.HasPrefix(path, "branches/") && strings.HasSuffix(path, "/protection/required_status_checks"):
		if f.Protection == nil {
			return "", httpError(404, "Branch not protected"), true
		}
		if f.Protection.Status != 0 && f.Protection.Status != 200 {
			return "", httpError(f.Protection.Status, f.Protection.Message), true
		}
		if len(f.Protection.Body) == 0 {
			return "{}", "", true
		}
		return string(f.Protection.Body), "", true
	case strings.HasPrefix(path, "rules/branches/"):
		if f.Rules != nil && f.Rules.Status != 0 && f.Rules.Status != 200 {
			return "", httpError(f.Rules.Status, f.Rules.Message), true
		}
		pages := []json.RawMessage{json.RawMessage("[]")}
		if f.Rules != nil && len(f.Rules.Pages) > 0 {
			pages = f.Rules.Pages
		}
		data, _ := json.Marshal(pages)
		return string(data), "", true
	case path == "actions/runs":
		runs := f.Runs
		if runs == nil {
			runs = []json.RawMessage{}
		}
		data, _ := json.Marshal([]any{map[string]any{"total_count": len(runs), "workflow_runs": runs}})
		return string(data), "", true
	case strings.HasPrefix(path, "actions/runs/") && strings.HasSuffix(path, "/jobs"):
		// actions/runs/<id>/attempts/<n>/jobs
		parts := strings.Split(path, "/")
		if len(parts) != 6 || parts[3] != "attempts" {
			return "", "", false
		}
		ids := f.AttemptJobs[parts[2]+"/"+parts[4]]
		jobs := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			jobs = append(jobs, map[string]any{"id": id})
		}
		data, _ := json.Marshal([]any{map[string]any{"total_count": len(jobs), "jobs": jobs}})
		return string(data), "", true
	}
	return "", "", false
}
