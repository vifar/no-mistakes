package agent

import (
	"errors"
	"strings"
)

// errAgentRefused marks a failure where the provider itself refused the turn's
// content: a content filter or content-policy block ended the response before
// the agent could say anything about the work.
//
// It is a fallback class of its own (see FallbackReasonFor) because the failure
// says nothing about the agent's fitness for the job. The same prompt sent
// through another provider usually goes through, so failing the step on the
// first refusal discards the rest of the configured agent list for a reason
// none of the remaining agents shares.
//
// The marker travels with the error rather than being re-derived from its text
// wherever it matters, because the typed evidence - opencode's own
// provider.content-filter name, for instance - is available at the one place
// that already knows how to read it.
var errAgentRefused = errors.New("provider blocked the turn's content")

// MarkRefused tags err as a provider content-policy refusal. It is a no-op for
// a nil error or one already tagged, so a call site may tag defensively.
func MarkRefused(err error) error {
	if err == nil || IsRefused(err) {
		return err
	}
	return &refusalError{err: err}
}

// IsRefused reports whether err is a provider content-policy refusal, either
// tagged by an adapter that reads a typed refusal or matched by its wording.
//
// The wording arm exists for the native CLIs that report a refusal as plain
// stderr: codex, grok, claude and the ACP bridges have no typed refusal field
// to read, so their own sentence is the only evidence there is.
func IsRefused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errAgentRefused) {
		return true
	}
	return refusesTurn(err.Error())
}

// refusalNeedles matches the wording a provider or its CLI uses when it blocks
// a response on content policy.
//
// Every needle names the policy itself rather than a generic "refused" or
// "blocked" verb, and they are only ever read from a FAILED attempt's error, so
// an ordinary adapter failure - or a model quoting a file - cannot match. A
// refusal is the one class where re-running the same prompt under a different
// provider is the whole point, so a missed needle is a run that fails on an
// agent the operator did not have to use.
var refusalNeedles = []string{
	"provider.content-filter",
	"content-filter",
	"content_filter",
	"content policy",
	"content_policy",
}

// refusesTurn reports whether text names a content-policy refusal.
func refusesTurn(text string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	for _, needle := range refusalNeedles {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// refusalError tags a provider refusal. It unwraps to both the cause and the
// marker, so errors.Is finds either one and errors.As still reaches the
// adapter's own error type.
type refusalError struct {
	err error
}

func (e *refusalError) Error() string { return e.err.Error() }

func (e *refusalError) Unwrap() []error { return []error{e.err, errAgentRefused} }
