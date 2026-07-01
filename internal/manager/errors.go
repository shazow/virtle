package manager

import (
	"context"
	"errors"

	"github.com/shazow/virtle/internal/manager/launch"
)

// ExitCode maps launcher failures onto process exit codes.
func ExitCode(err error) int {
	var cmdErr *launch.CommandError
	if errors.As(err, &cmdErr) && cmdErr.ExitCode >= 0 {
		return cmdErr.ExitCode
	}

	if errors.Is(err, context.Canceled) {
		return 130
	}

	return 1
}
