package runtime

import (
	"context"
	"errors"
	"github.com/shazow/virtle/internal/manifest"
	"testing"

	rawQMP "github.com/digitalocean/go-qemu/qmp/raw"
	"github.com/shazow/virtle/internal/control"
)

type fakeBalloonQMP struct {
	err error
}

func (q fakeBalloonQMP) WithRaw(context.Context, func(*rawQMP.Monitor) error) error {
	return q.err
}

func TestBalloonRequiresConfiguredDevice(t *testing.T) {
	_, err := balloon(context.Background(), nil, fakeBalloonQMP{}, control.BalloonRequest{})
	if !errors.Is(err, errBalloonNotConfigured) {
		t.Fatalf("error: got %v want %v", err, errBalloonNotConfigured)
	}
}

func TestBalloonPropagatesQMPError(t *testing.T) {
	qmpErr := errors.New("qmp failed")
	_, err := balloon(context.Background(), &manifest.BalloonDevice{}, fakeBalloonQMP{err: qmpErr}, control.BalloonRequest{})
	if !errors.Is(err, qmpErr) {
		t.Fatalf("error: got %v want %v", err, qmpErr)
	}
}
