package shipping

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// protectedReceiptForge is a forge that also reports the target branch's
// required status checks.
type protectedReceiptForge struct {
	*receiptForge
	required []string
	reads    int
}

func (h *protectedReceiptForge) Capabilities() scm.Capabilities { return scm.Capabilities{} }

func (h *protectedReceiptForge) GetRequiredChecks(context.Context, *scm.PR, string) ([]scm.RequiredCheck, error) {
	h.reads++
	var required []scm.RequiredCheck
	for _, c := range h.required {
		required = append(required, scm.RequiredCheck{Context: c})
	}
	return required, nil
}

func recordScope(t *testing.T, f *exportFixture, policy scm.CheckScopePolicy) {
	t.Helper()
	encoded, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.database.SetRunCheckScope(f.run.ID, string(encoded)); err != nil {
		t.Fatal(err)
	}
}

func TestCIReadyReceiptUsesTheRecordedCheckScope(t *testing.T) {
	ctx := context.Background()
	f := newExportFixture(t)
	if err := f.database.SetRunCIReadyWithReason(f.run.ID, true, false); err != nil {
		t.Fatal(err)
	}
	forge := &protectedReceiptForge{receiptForge: f.forge, required: []string{"build"}}
	f.forge.checks = []scm.Check{
		{Name: "build", Bucket: scm.CheckBucketPass},
		{Name: "approve", Bucket: scm.CheckBucketFail},
		{Name: "approval-status", Bucket: scm.CheckBucketPending},
	}

	// An auto policy reads the target branch's required checks, so the
	// external approval contexts that are not required do not block readiness.
	recordScope(t, f, scm.CheckScopePolicy{RequiredChecks: scm.RequiredChecksAuto})
	if _, err := Export(ctx, f.database, f.repo, f.run.ID, "ci-ready", forge); err != nil {
		t.Fatalf("non-required failing checks blocked the receipt: %v", err)
	}
	if forge.reads != 1 {
		t.Fatalf("required checks read %d times for one export, want 1", forge.reads)
	}

	// A recorded policy is applied exactly as the CI step applied it.
	recordScope(t, f, scm.CheckScopePolicy{RequiredChecks: scm.RequiredChecksAll})
	if _, err := Export(ctx, f.database, f.repo, f.run.ID, "ci-ready", forge); err == nil || !strings.Contains(err.Error(), "all live checks green") {
		t.Fatalf("ci.required_checks: all must judge every check, got %v", err)
	}
	recordScope(t, f, scm.CheckScopePolicy{RequiredChecks: scm.RequiredChecksAll, ExternalChecks: []string{"approv*"}})
	if _, err := Export(ctx, f.database, f.repo, f.run.ID, "ci-ready", forge); err != nil {
		t.Fatalf("external checks blocked the receipt: %v", err)
	}
}

func TestCIReadyReceiptKeepsRequiredChecksStrict(t *testing.T) {
	ctx := context.Background()
	f := newExportFixture(t)
	if err := f.database.SetRunCIReadyWithReason(f.run.ID, true, false); err != nil {
		t.Fatal(err)
	}
	forge := &protectedReceiptForge{receiptForge: f.forge, required: []string{"build", "deploy"}}
	for name, tc := range map[string]struct {
		checks []scm.Check
		policy scm.CheckScopePolicy
	}{
		"required check failing": {[]scm.Check{{Name: "build", Bucket: scm.CheckBucketFail}, {Name: "deploy", Bucket: scm.CheckBucketPass}}, scm.CheckScopePolicy{}},
		"required check pending": {[]scm.Check{{Name: "build", Bucket: scm.CheckBucketPass}, {Name: "deploy", Bucket: scm.CheckBucketPending}}, scm.CheckScopePolicy{}},
		"required check missing": {[]scm.Check{{Name: "build", Bucket: scm.CheckBucketPass}}, scm.CheckScopePolicy{}},
		"must_pass check failing": {[]scm.Check{{Name: "build", Bucket: scm.CheckBucketPass}, {Name: "deploy", Bucket: scm.CheckBucketPass}, {Name: "lint", Bucket: scm.CheckBucketFail}},
			scm.CheckScopePolicy{MustPass: []string{"lint"}}},
	} {
		t.Run(name, func(t *testing.T) {
			f.forge.checks = tc.checks
			recordScope(t, f, tc.policy)
			if _, err := Export(ctx, f.database, f.repo, f.run.ID, "ci-ready", forge); err == nil {
				t.Fatal("receipt accepted a head the CI step would not call ready")
			}
		})
	}
	f.forge.checks = []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass}, {Name: "deploy", Bucket: scm.CheckBucketSkip}, {Name: "approve", Bucket: scm.CheckBucketFail}}
	recordScope(t, f, scm.CheckScopePolicy{})
	if _, err := Export(ctx, f.database, f.repo, f.run.ID, "ci-ready", forge); err != nil {
		t.Fatalf("green required checks were refused: %v", err)
	}
}

func TestCIReadyReceiptExternalOnlyUsesSharedReadiness(t *testing.T) {
	for _, tc := range []struct {
		name        string
		required    []string
		checks      []scm.Check
		noCI, ready bool
	}{
		{"strict no_ci with external check", nil, []scm.Check{{Name: "approval", Bucket: scm.CheckBucketFail}}, true, true},
		{"known required set entirely external", []string{"approval"}, []scm.Check{{Name: "approval", Bucket: scm.CheckBucketFail}}, false, true},
		{"known passing requirement with external failure", []string{"build"}, []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass}, {Name: "approval", Bucket: scm.CheckBucketFail}}, false, true},
		{"unknown required set waits", nil, []scm.Check{{Name: "approval", Bucket: scm.CheckBucketFail}}, false, false},
		{"unobserved remains empty", nil, nil, true, false},
		{"missing required check blocks despite no_ci", []string{"build"}, []scm.Check{{Name: "approval", Bucket: scm.CheckBucketFail}}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExportFixture(t)
			if err := f.database.SetRunCIReadyWithReason(f.run.ID, true, false); err != nil {
				t.Fatal(err)
			}
			f.forge.checks = tc.checks
			forge := &protectedReceiptForge{receiptForge: f.forge, required: tc.required}
			recordScope(t, f, scm.CheckScopePolicy{NoCI: tc.noCI, ExternalChecks: []string{"approval"}})
			_, err := Export(context.Background(), f.database, f.repo, f.run.ID, "ci-ready", forge)
			if (err == nil) != tc.ready {
				t.Fatalf("receipt error=%v want ready=%v", err, tc.ready)
			}
		})
	}
}

// A run started before check scoping recorded no policy. It was judged on
// every check then, so the receipt must not quietly widen what it accepts.
func TestCIReadyReceiptWithoutRecordedScopeJudgesEveryCheck(t *testing.T) {
	ctx := context.Background()
	f := newExportFixture(t)
	if err := f.database.SetRunCIReadyWithReason(f.run.ID, true, false); err != nil {
		t.Fatal(err)
	}
	if raw, err := f.database.GetRunCheckScope(f.run.ID); err != nil || raw != "" {
		t.Fatalf("fixture recorded a scope: %q, %v", raw, err)
	}
	forge := &protectedReceiptForge{receiptForge: f.forge, required: []string{"build"}}
	f.forge.checks = []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass}, {Name: "approve", Bucket: scm.CheckBucketFail}}
	if _, err := Export(ctx, f.database, f.repo, f.run.ID, "ci-ready", forge); err == nil || !strings.Contains(err.Error(), "all live checks green") {
		t.Fatalf("a failing non-required check must refuse a legacy run's receipt, got %v", err)
	}
	if forge.reads != 0 {
		t.Fatalf("legacy scope read branch protection %d times", forge.reads)
	}
	f.forge.checks = []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass}, {Name: "approve", Bucket: scm.CheckBucketPass}}
	if _, err := Export(ctx, f.database, f.repo, f.run.ID, "ci-ready", forge); err != nil {
		t.Fatalf("all-green legacy run was refused: %v", err)
	}
}

func TestCIReadyReceiptWaitsForRecordedReviewBotPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, app string
		bucket    scm.CheckBucket
		policy    string
		external  []string
		ready     bool
	}{
		{"pending bot", "greptile-apps", scm.CheckBucketPending, "always", nil, false},
		{"legacy pending bot", "", scm.CheckBucketPending, "always", nil, false},
		{"different app", "github-actions", scm.CheckBucketPending, "always", nil, true},
		{"completed bot", "greptile-apps", scm.CheckBucketPass, "always", nil, true},
		{"default policy", "greptile-apps", scm.CheckBucketPending, "on_failure", nil, true},
		{"external bot", "greptile-apps", scm.CheckBucketPending, "always", []string{"Greptile Review"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExportFixture(t)
			if err := f.database.SetRunCIReadyWithReason(f.run.ID, true, false); err != nil {
				t.Fatal(err)
			}
			forge := &protectedReceiptForge{receiptForge: f.forge, required: []string{"build"}}
			f.forge.checks = []scm.Check{
				{Name: "build", Bucket: scm.CheckBucketPass},
				{Name: "Greptile Review", App: tc.app, Bucket: tc.bucket},
			}
			recordScope(t, f, scm.CheckScopePolicy{ReviewBotComments: tc.policy, ExternalChecks: tc.external})
			_, err := Export(context.Background(), f.database, f.repo, f.run.ID, "ci-ready", forge)
			if (err == nil) != tc.ready {
				t.Fatalf("receipt error=%v, want ready=%v", err, tc.ready)
			}
		})
	}
}

type commentReceiptForge struct {
	*protectedReceiptForge
	comments     []scm.ReviewComment
	commentErr   error
	commentReads int
}

func (h *commentReceiptForge) Capabilities() scm.Capabilities {
	return scm.Capabilities{ReviewComments: true}
}
func (h *commentReceiptForge) Provider() scm.Provider { return scm.ProviderGitHub }
func (h *commentReceiptForge) GetReviewComments(context.Context, *scm.PR) ([]scm.ReviewComment, error) {
	h.commentReads++
	return h.comments, h.commentErr
}

func TestCIReadyReceiptVerifiesRecordedReviewBotComments(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		bucket                                   scm.CheckBucket
		absent, external, unresolved, unreadable bool
		policy                                   string
		ready                                    bool
		reads                                    int
	}{
		{name: "failed informational bot with unresolved comments", bucket: scm.CheckBucketFail, unresolved: true, policy: "always", reads: 1},
		{name: "failed informational bot without comments", bucket: scm.CheckBucketFail, policy: "always", ready: true, reads: 1},
		{name: "passing informational bot with unresolved comments", bucket: scm.CheckBucketPass, unresolved: true, policy: "always", reads: 1},
		{name: "absent bot with unresolved comments", absent: true, unresolved: true, policy: "always", reads: 1},
		{name: "cancelled informational bot with unresolved comments", bucket: scm.CheckBucketCancel, unresolved: true, policy: "always", reads: 1},
		{name: "unreadable comments", bucket: scm.CheckBucketFail, unreadable: true, policy: "always", reads: 1},
		{name: "external bot", bucket: scm.CheckBucketFail, external: true, unresolved: true, policy: "always", ready: true},
		{name: "absent external bot", absent: true, external: true, unresolved: true, policy: "always", ready: true},
		{name: "recorded on_failure", bucket: scm.CheckBucketFail, unresolved: true, policy: "on_failure", ready: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExportFixture(t)
			if err := f.database.SetRunCIReadyWithReason(f.run.ID, true, false); err != nil {
				t.Fatal(err)
			}
			f.forge.checks = []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass}}
			if !tc.absent {
				f.forge.checks = append(f.forge.checks, scm.Check{Name: "Greptile Review", App: "greptile-apps", Bucket: tc.bucket})
			}
			forge := &commentReceiptForge{protectedReceiptForge: &protectedReceiptForge{receiptForge: f.forge, required: []string{"build"}}}
			if tc.unresolved {
				forge.comments = []scm.ReviewComment{{ID: "thread-1", Author: "greptile-apps[bot]", Body: "Unresolved review concern"}}
			}
			if tc.unreadable {
				forge.commentErr = errors.New("comment read failed")
			}
			policy := scm.CheckScopePolicy{ReviewBotComments: tc.policy}
			if tc.external {
				policy.ExternalChecks = []string{"Greptile Review"}
			}
			recordScope(t, f, policy)
			_, err := Export(context.Background(), f.database, f.repo, f.run.ID, "ci-ready", forge)
			if (err == nil) != tc.ready {
				t.Fatalf("receipt error=%v, want ready=%v", err, tc.ready)
			}
			if !tc.ready && !strings.Contains(err.Error(), "review bot comments") {
				t.Fatalf("receipt refused for unrelated reason: %v", err)
			}
			if forge.commentReads != tc.reads {
				t.Fatalf("comment reads=%d, want %d", forge.commentReads, tc.reads)
			}
		})
	}
}

func TestCIReadyReceiptRefusesAWorkflowRerunThatFailedBeforeJobs(t *testing.T) {
	f := newExportFixture(t)
	if err := f.database.SetRunCIReadyWithReason(f.run.ID, true, false); err != nil {
		t.Fatal(err)
	}
	forge := &protectedReceiptForge{receiptForge: f.forge, required: []string{"build"}}
	f.forge.checks = []scm.Check{{Name: "CI", Kind: scm.CheckKindRun, Bucket: scm.CheckBucketFail, Supersedes: []scm.Check{{Name: "build", Workflow: "CI", Kind: scm.CheckKindRun}}}}
	recordScope(t, f, scm.CheckScopePolicy{RequiredChecks: scm.RequiredChecksAuto})
	if _, err := Export(context.Background(), f.database, f.repo, f.run.ID, "ci-ready", forge); err == nil {
		t.Fatal("receipt accepted a head whose required workflow rerun failed before creating jobs")
	}
}

// The scope a ci-ready proof was judged under is part of the authority the
// export brackets: a resumed CI execution that re-records it mid-export must
// not leave a receipt issued under the old scope.
func TestCIReadyReceiptRefusesAScopeRecordedMidExport(t *testing.T) {
	ctx := context.Background()
	f := newExportFixture(t)
	if err := f.database.SetRunCIReadyWithReason(f.run.ID, true, false); err != nil {
		t.Fatal(err)
	}
	f.forge.checks = []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass}, {Name: "approve", Bucket: scm.CheckBucketFail}}
	recordScope(t, f, scm.CheckScopePolicy{RequiredChecks: scm.RequiredChecksAuto})
	forge := &protectedReceiptForge{receiptForge: f.forge, required: []string{"build"}}
	if _, err := Export(ctx, f.database, f.repo, f.run.ID, "ci-ready", forge); err != nil {
		t.Fatalf("stable scope was refused: %v", err)
	}

	observed := 0
	f.forge.observe = func() {
		observed++
		if observed == 2 { // after readiness was judged, before the closing snapshot
			recordScope(t, f, scm.CheckScopePolicy{RequiredChecks: scm.RequiredChecksAll})
		}
	}
	if _, err := Export(ctx, f.database, f.repo, f.run.ID, "ci-ready", forge); err == nil || !strings.Contains(err.Error(), "authority moved") {
		t.Fatalf("a scope re-recorded mid-export must fail the export closed, got %v", err)
	}
}

// A CI execution that resumes under a changed policy while an export runs
// means the proof was judged under authority that no longer holds.
func TestCIReadyReceiptRefusesWhenTheRecordedScopeMovesDuringExport(t *testing.T) {
	ctx := context.Background()
	f := newExportFixture(t)
	if err := f.database.SetRunCIReadyWithReason(f.run.ID, true, false); err != nil {
		t.Fatal(err)
	}
	f.forge.checks = []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass}}
	recordScope(t, f, scm.CheckScopePolicy{RequiredChecks: scm.RequiredChecksAll})
	forge := &protectedReceiptForge{receiptForge: f.forge, required: []string{"build"}}
	if _, err := Export(ctx, f.database, f.repo, f.run.ID, "ci-ready", forge); err != nil {
		t.Fatalf("unmoved scope was refused: %v", err)
	}

	calls := 0
	f.forge.observe = func() {
		calls++
		if calls == 2 { // the observation after the readiness judgement
			recordScope(t, f, scm.CheckScopePolicy{RequiredChecks: scm.RequiredChecksAll, ExternalChecks: []string{"build"}})
		}
	}
	_, err := Export(ctx, f.database, f.repo, f.run.ID, "ci-ready", forge)
	if err == nil || !strings.Contains(err.Error(), "authority moved") {
		t.Fatalf("a scope change during export must fail closed, got %v", err)
	}
}
