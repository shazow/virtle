package control

import (
	"errors"
	"os"
	"syscall"
)

// InvalidParams reports request params that do not satisfy the method.
func InvalidParams(message string) error {
	return &RPCError{Code: ErrInvalidParams, Message: message}
}

// FailedPrecondition wraps err as an RPC failed-precondition error.
func FailedPrecondition(err error) error {
	return &RPCError{Code: ErrFailedPrecondition, Message: err.Error()}
}

// ResourceLimit wraps err as an RPC resource-limit error.
func ResourceLimit(err error) error {
	return &RPCError{Code: ErrResourceLimit, Message: err.Error()}
}

// IsSocketUnavailable reports whether err means no control socket is reachable.
func IsSocketUnavailable(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}
