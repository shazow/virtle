package control

import (
	"errors"
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

func TestResourceLimit(t *testing.T) {
	err := ResourceLimit(errors.New("too much"))
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected RPCError, got %T", err)
	}
	if rpcErr.Code != ErrResourceLimit || rpcErr.Message != "too much" {
		t.Fatalf("unexpected rpc error: %+v", rpcErr)
	}
}
