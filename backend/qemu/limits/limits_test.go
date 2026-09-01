package limits_test

import (
	"errors"
	"testing"

	"github.com/shazow/virtle/backend/qemu/limits"
)

func TestErrorIdentifiesExceededLimit(t *testing.T) {
	err := &limits.Error{Resource: "message", Limit: 42}
	if !errors.Is(err, limits.ErrExceeded) {
		t.Fatalf("expected limit sentinel, got %v", err)
	}
	if got, want := err.Error(), "resource limit exceeded: message exceeds maximum 42 bytes"; got != want {
		t.Fatalf("unexpected error: got %q want %q", got, want)
	}
}
