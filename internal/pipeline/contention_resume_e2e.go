//go:build e2e

package pipeline

// E2e journeys assert the outcome of an exhausted state write, so the e2e
// binary settles it without in-place resumes. Unit tests cover the resumes.
func init() { ContentionResumes = 0 }
