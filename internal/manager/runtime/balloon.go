package runtime

import (
	"context"
	"errors"
	"fmt"

	rawQMP "github.com/digitalocean/go-qemu/qmp/raw"
	balloonpkg "github.com/shazow/virtle/internal/balloon"
	"github.com/shazow/virtle/internal/manager/control"
)

var errBalloonNotConfigured = errors.New("balloon device is not configured")

type balloonQMP interface {
	WithRaw(ctx context.Context, fn func(*rawQMP.Monitor) error) error
}

func balloon(ctx context.Context, device *balloonpkg.Device, qmp balloonQMP, req control.BalloonRequest) (control.BalloonResponse, error) {
	if device == nil {
		return control.BalloonResponse{}, errBalloonNotConfigured
	}
	var actual int64
	if err := qmp.WithRaw(ctx, func(monitor *rawQMP.Monitor) error {
		info, err := monitor.QueryBalloon()
		if err != nil {
			return fmt.Errorf("qmp query-balloon: %w", err)
		}
		actual = info.Actual
		if req.TargetBytes > 0 {
			if err := monitor.Balloon(req.TargetBytes); err != nil {
				return fmt.Errorf("qmp balloon: %w", err)
			}
		}
		return nil
	}); err != nil {
		return control.BalloonResponse{}, err
	}
	return control.BalloonResponse{ActualBytes: actual, TargetBytes: req.TargetBytes}, nil
}
