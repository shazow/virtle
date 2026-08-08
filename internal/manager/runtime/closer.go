package runtime

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/shazow/virtle/internal/manager/control"
	"github.com/shazow/virtle/internal/manager/launch"
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
}

func newCloser(state *state) *closer {
	return &closer{state: state}
}

func (c *closer) Close(actions closeActions) error {
	var err error
	c.once.Do(func() {
		if c.state != nil {
			c.state.Set(control.RuntimeStopping)
		}
		err = actions.Run()
		if c.state != nil {
			c.state.Set(control.RuntimeStopped)
		}
	})
	return err
}

func (a closeActions) Run() error {
	var err error
	if a.WriteBack != nil && !a.SkipWriteBack {
		writeBackCtx, cancelWriteBack := context.WithTimeout(context.Background(), a.WriteBackTimeout)
		err = errors.Join(err, a.WriteBack(writeBackCtx))
		cancelWriteBack()
	}
	if a.Control != nil {
		err = errors.Join(err, a.Control.Close())
	}
	if a.Processes != nil {
		// Teardown is never canceled from above; each process escalates on
		// its own grace period.
		err = errors.Join(err, a.Processes.Close(context.Background()))
	}
	if a.QMP != nil {
		err = errors.Join(err, a.QMP.Disconnect())
	}
	if a.Cleanup != nil {
		err = errors.Join(err, a.Cleanup())
	}
	return err
}
