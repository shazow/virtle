package runtime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/launch"
	"github.com/shazow/virtle/internal/control"
)

type disconnecter interface {
	Disconnect() error
}

type closeActions struct {
	Processes        *launch.ProcessSet
	QMP              disconnecter
	WriteBack        func(context.Context) error
	WriteBackTimeout time.Duration
	SkipWriteBack    bool
	Control          *control.Server
	Cleanup          func() error
}

type closer struct {
	once  sync.Once
	state *state
	err   error
}

func newCloser(state *state) *closer {
	return &closer{state: state}
}

func (c *closer) Close(ctx context.Context, actions closeActions) error {
	c.once.Do(func() {
		if c.state != nil {
			c.state.Set(control.RuntimeStopping)
		}
		c.err = actions.Run(ctx)
		if c.state != nil {
			c.state.Set(control.RuntimeStopped)
			c.state.Finish(c.err)
		}
	})
	return c.err
}

func (a closeActions) Run(ctx context.Context) error {
	var err error
	if a.WriteBack != nil && !a.SkipWriteBack {
		writeBackCtx, cancelWriteBack := context.WithTimeout(ctx, a.WriteBackTimeout)
		err = errors.Join(err, a.WriteBack(writeBackCtx))
		cancelWriteBack()
	}
	if a.Control != nil {
		err = errors.Join(err, a.Control.Close())
	}
	if a.Processes != nil {
		// ctx expiry abandons the remaining graceful steps and kills (see
		// executor.Process.Stop); Kill passes context.Background so a hard
		// stop is never cut short.
		err = errors.Join(err, a.Processes.Close(ctx))
	}
	if a.QMP != nil {
		err = errors.Join(err, a.QMP.Disconnect())
	}
	if a.Cleanup != nil {
		err = errors.Join(err, a.Cleanup())
	}
	return err
}
