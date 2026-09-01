// Package limits defines the resource bounds enforced by the QEMU backend.
//
// These defaults bound data buffered across guest and local-control trust
// boundaries. Guest.Open streams file content and is not subject to
// DefaultMaxFileReadSize, though every QGA response frame remains bounded by
// DefaultMaxFrameSize.
package limits

import (
	"errors"
	"fmt"
	"time"
)

// DefaultMaxFrameSize bounds one line-delimited QMP or QGA message.
const DefaultMaxFrameSize int64 = 16 * 1024 * 1024

// DefaultMaxCommandOutputSize bounds the combined base64-encoded stdout and
// stderr retained for one guest command.
const DefaultMaxCommandOutputSize int64 = 8 * 1024 * 1024

// DefaultMaxFileReadSize bounds one buffered guest-file read.
const DefaultMaxFileReadSize int64 = 64 * 1024 * 1024

// DefaultMaxRequestSize bounds one local control request envelope.
const DefaultMaxRequestSize int64 = 16 * 1024 * 1024

// DefaultRequestReadTimeout bounds how long a client may take to send one
// local control request.
const DefaultRequestReadTimeout = 15 * time.Second

// DefaultMaxHandlers bounds concurrently executing local control requests.
const DefaultMaxHandlers = 64

// ErrExceeded identifies an operation that crossed a configured resource limit.
var ErrExceeded = errors.New("resource limit exceeded")

// Error reports which resource crossed its limit.
type Error struct {
	Resource string
	Limit    int64
	Unit     string
}

func (e *Error) Error() string {
	if e == nil {
		return ErrExceeded.Error()
	}
	unit := e.Unit
	if unit == "" {
		unit = "bytes"
	}
	if e.Resource == "" {
		return fmt.Sprintf("%s: maximum %d %s", ErrExceeded, e.Limit, unit)
	}
	return fmt.Sprintf("%s: %s exceeds maximum %d %s", ErrExceeded, e.Resource, e.Limit, unit)
}

func (e *Error) Unwrap() error { return ErrExceeded }
