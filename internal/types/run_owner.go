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
