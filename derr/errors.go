// Package derr defines distinguishable error kinds for the diff/merge engine.
package derr

import "fmt"

// Kind classifies an error so callers can distinguish merge conflicts from
// malformed input, encoding problems and out-of-bounds failures.
type Kind int

const (
	KindUnknown Kind = iota
	// KindMalformedPatch: the patch text is not a valid unified diff.
	KindMalformedPatch
	// KindEncodingMismatch: the input encoding does not match the declared/expected one.
	KindEncodingMismatch
	// KindOutOfBounds: a hunk references lines outside the target content.
	KindOutOfBounds
	// KindAmbiguousMatch: context matched more than one location.
	KindAmbiguousMatch
	// KindNoMatch: context could not be located within the allowed tolerance.
	KindNoMatch
	// KindConflict: a three-way merge conflict (not an error per se).
	KindConflict
	// KindAborted: the operation was cancelled or timed out.
	KindAborted
	// KindCorrupt: stored content or metadata failed integrity checks.
	KindCorrupt
	// KindLocked: a concurrent operation holds the required lock.
	KindLocked
)

// Error is a typed engine error carrying a Kind and optional detail.
type Error struct {
	Kind    Kind
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

// New builds an Error with the given kind and message.
func New(k Kind, msg string) *Error {
	return &Error{Kind: k, Message: msg}
}

// Wrap builds an Error wrapping a cause.
func Wrap(k Kind, msg string, cause error) *Error {
	return &Error{Kind: k, Message: msg, Cause: cause}
}

// IsKind reports whether err is an *Error of the given Kind.
func IsKind(err error, k Kind) bool {
	e, ok := err.(*Error)
	if !ok {
		return false
	}
	return e.Kind == k
}
