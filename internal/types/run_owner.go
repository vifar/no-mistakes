package types

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type runOwnerKey struct{}

// WithRunOwner carries caller-supplied responsibility through launch admission.
// It grants no authority to answer a gate and is never inferred from transcripts.
func WithRunOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, runOwnerKey{}, strings.TrimSpace(owner))
}
func RunOwnerFromContext(ctx context.Context) string {
	owner, _ := ctx.Value(runOwnerKey{}).(string)
	return owner
}
func ValidateRunOwner(owner string) error {
	if len(owner) > 256 || !utf8.ValidString(owner) || strings.ContainsFunc(owner, unicode.IsControl) {
		return fmt.Errorf("owner must be valid UTF-8, at most 256 bytes, without control characters")
	}
	return nil
}

// OwnerAdmissionEvent is the immutable admission audit stored with a run.
// It is local state, independent of the daemon's configurable logging level.
type OwnerAdmissionEvent struct {
	Event                string `json:"event" toon:"event"`
	RunID                string `json:"run_id" toon:"run_id"`
	Branch               string `json:"branch" toon:"branch"`
	CallerPath           string `json:"caller_path" toon:"caller_path"`
	OwnerlessAdmitted    bool   `json:"ownerless_admitted" toon:"ownerless_admitted"`
	OwnerValidationError string `json:"owner_validation_error" toon:"owner_validation_error"`
	Warning              string `json:"warning" toon:"warning"`
	CreatedAt            int64  `json:"created_at" toon:"created_at"`
}
