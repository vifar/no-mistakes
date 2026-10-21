package scm

import "errors"

// ErrTransientTimeout identifies a credential-store timeout, not a rejected
// credential. Only read-only PR-context operations may retry this class.
var ErrTransientTimeout = errors.New("transient keyring timeout")

// ErrUnauthenticated identifies an unsuccessful authentication check after
// interruption, missing executables and transient timeouts have been excluded.
var ErrUnauthenticated = errors.New("gh CLI is not authenticated")
