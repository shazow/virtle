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

type shutdownResources struct {
	Processes *launch.ProcessSet
	QMP       disconnecter
}

type closeActions struct {
	shutdownResources
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

func (c *closer) Close(actions closeActions) error {
	return c.CloseContext(context.Background(), actions)
}

func (c *closer) CloseContext(ctx context.Context, actions closeActions) error {
	c.once.Do(func() {
		if c.state != nil {
			c.state.Set(control.RuntimeStopping)
		}
		c.err = actions.RunContext(ctx)
		if c.state != nil {
			c.state.Set(control.RuntimeStopped)
			c.state.Finish(c.err)
		}
	})
	return c.err
}

func (a closeActions) Run() error {
	return a.RunContext(context.Background())
}

func (a closeActions) RunContext(ctx context.Context) error {
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
		// Teardown is never canceled from above; each process escalates on
		// its own grace period.
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
