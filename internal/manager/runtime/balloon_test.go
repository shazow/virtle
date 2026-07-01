package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	rawQMP "github.com/digitalocean/go-qemu/qmp/raw"
	balloonpkg "github.com/shazow/virtle/internal/balloon"
	"github.com/shazow/virtle/internal/manager/control"
)

type fakeBalloonQMP struct {
	err error
}

func (q fakeBalloonQMP) WithRaw(time.Duration, func(*rawQMP.Monitor) error) error {
	return q.err
}

func TestBalloonRequiresConfiguredDevice(t *testing.T) {
	_, err := balloon(context.Background(), nil, fakeBalloonQMP{}, time.Second, control.BalloonRequest{})
	if !errors.Is(err, errBalloonNotConfigured) {
		t.Fatalf("error: got %v want %v", err, errBalloonNotConfigured)
	}
}

func TestBalloonPropagatesQMPError(t *testing.T) {
	qmpErr := errors.New("qmp failed")
	_, err := balloon(context.Background(), &balloonpkg.Device{}, fakeBalloonQMP{err: qmpErr}, time.Second, control.BalloonRequest{})
	if !errors.Is(err, qmpErr) {
		t.Fatalf("error: got %v want %v", err, qmpErr)
	}
}
