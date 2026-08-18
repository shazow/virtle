package balloon

import "context"

// SetActual sets the balloon's target guest memory size in bytes over QMP.
// It is the one-shot form of the resize the controller loop performs.
func SetActual(ctx context.Context, session MonitorSession, sizeBytes int64) error {
	return newQMPSession(session).SetBalloonLogicalSize(ctx, sizeBytes)
}
