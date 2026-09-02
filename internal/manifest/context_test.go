package manifest

import (
	"context"
	"testing"
	"time"

	"github.com/shazow/virtle/units"
)

func TestGuestCommandContextBoundsCommands(t *testing.T) {
	ctx, cancel := GuestCommandContext(context.Background(), 2500*time.Millisecond)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected guest command deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 2500*time.Millisecond {
		t.Fatalf("unexpected guest command deadline: %s remaining", remaining)
	}

	ctx, cancel = GuestCommandContext(context.Background(), 0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("expected no deadline for zero guest timeout")
	}

	document := validDocument()
	document.QEMU.GuestDefaultTimeout = units.Duration(30 * time.Second)
	resolved, err := document.Manifest()
	if err != nil {
		t.Fatalf("resolve manifest: %v", err)
	}
	ctx, cancel = resolved.GuestCommandContext(context.Background())
	defer cancel()
	deadline, ok = ctx.Deadline()
	if !ok {
		t.Fatal("expected manifest guest command deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 30*time.Second {
		t.Fatalf("unexpected manifest guest command deadline: %s remaining", remaining)
	}
}
