package launch

import (
	"context"
	"time"

	"github.com/shazow/virtle/internal/executor"
)

type waitProcess interface {
	Done() <-chan struct{}
	Wait() error
	Name() string
}

type LifecycleProcessWait struct {
	Stage     string
	Process   waitProcess
	Delay     time.Duration
	Lifecycle *Lifecycle
	Watchers  executor.Group
	PollDelay time.Duration

	Suspend func(context.Context) error
	Info    func(context.Context)
}

func WaitForLifecycleProcess(ctx context.Context, wait LifecycleProcessWait) error {
	if wait.PollDelay <= 0 {
		wait.PollDelay = time.Second
	}
	var processDone <-chan struct{}
	if wait.Process != nil {
		processDone = wait.Process.Done()
	}
	var delayDone <-chan time.Time
	var timer *time.Timer
	if wait.Delay > 0 {
		timer = time.NewTimer(wait.Delay)
		delayDone = timer.C
		defer timer.Stop()
	}

	ticker := time.NewTicker(wait.PollDelay)
	defer ticker.Stop()

	for {
		select {
		case <-processDone:
			if wait.Process == nil {
				return nil
			}
			if err := wait.Process.Wait(); err != nil {
				return wrapCommandError(wait.Stage, wait.Process.Name(), err)
			}
			return nil
		case <-delayDone:
			return nil
		case <-suspendNotify(wait.Lifecycle):
			if wait.Suspend != nil {
				return wait.Suspend(ctx)
			}
		case <-infoNotify(wait.Lifecycle):
			if wait.Info != nil {
				wait.Info(ctx)
			}
		case <-ticker.C:
			if err := firstUnexpectedExit(wait.Stage, wait.Watchers); err != nil {
				return err
			}
		case <-ctx.Done():
			return wrapStage(wait.Stage, ctx.Err())
		}
	}
}

func suspendNotify(lifecycle *Lifecycle) <-chan struct{} {
	if lifecycle == nil || lifecycle.Suspend() == nil {
		return nil
	}
	return lifecycle.Suspend().Notify()
}

func infoNotify(lifecycle *Lifecycle) <-chan struct{} {
	if lifecycle == nil {
		return nil
	}
	return lifecycle.Info()
}
