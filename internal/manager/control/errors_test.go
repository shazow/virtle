package control

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestFailedPrecondition(t *testing.T) {
	err := FailedPrecondition(errors.New("not ready"))
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected RPCError, got %T", err)
	}
	if rpcErr.Code != ErrFailedPrecondition || rpcErr.Message != "not ready" {
		t.Fatalf("unexpected rpc error: %+v", rpcErr)
	}
}

func TestIsSocketUnavailable(t *testing.T) {
	for _, err := range []error{os.ErrNotExist, syscall.ENOENT, syscall.ECONNREFUSED} {
		if !IsSocketUnavailable(err) {
			t.Fatalf("expected unavailable for %v", err)
		}
	}
	if IsSocketUnavailable(errors.New("other")) {
		t.Fatalf("did not expect unavailable for arbitrary error")
	}
}
