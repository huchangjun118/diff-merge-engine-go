package derr

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsKindThroughWrap(t *testing.T) {
	inner := New(KindAmbiguousMatch, "two locations")
	wrapped := Wrap(KindAmbiguousMatch, "hunk 0", inner)
	if !IsKind(wrapped, KindAmbiguousMatch) {
		t.Fatal("IsKind must traverse wrapped causes")
	}
	if IsKind(wrapped, KindNoMatch) {
		t.Fatal("IsKind matched the wrong kind")
	}
	var e *Error
	if !errors.As(wrapped, &e) || e.Kind != KindAmbiguousMatch {
		t.Fatal("errors.As did not recover the typed error")
	}
	if !errors.Is(wrapped, inner) {
		t.Fatal("wrapped error should be comparable via errors.Is")
	}
}

func TestIsKindOnNonEngineError(t *testing.T) {
	if IsKind(fmt.Errorf("plain"), KindMalformedPatch) {
		t.Fatal("plain error must not match an engine kind")
	}
	if IsKind(nil, KindUnknown) {
		t.Fatal("nil error must not match any kind")
	}
}
