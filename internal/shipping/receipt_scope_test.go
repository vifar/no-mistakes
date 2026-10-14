package shipping

import (
	"context"
	"encoding/json"
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

func (h *protectedReceiptForge) GetRequiredCheckContexts(context.Context, *scm.PR, string) ([]string, error) {
	h.reads++
	return h.required, nil
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

	// The default policy reads the target branch's required checks, so the
	// external approval contexts that are not required do not block readiness.
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
