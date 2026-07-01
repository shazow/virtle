package launch

import (
	"context"
	"sync"
	"testing"

	"github.com/shazow/virtle/internal/executor/executortest"
)

func TestProcessSetWatchersAndVMWatchers(t *testing.T) {
	qemu := (&executortest.Process{}).Process()
	run := (&executortest.Process{}).Process()
	processes := NewProcessSet()
	processes.SetQEMU(qemu)
	processes.Add(run)

	watchers := processes.Watchers()
	if got, want := len(watchers.Processes()), 2; got != want {
		t.Fatalf("unexpected watcher count: got %d want %d", got, want)
	}
	vmWatcherGroup := processes.VMWatchers()
	vmWatchers := vmWatcherGroup.Processes()
	if got, want := len(vmWatchers), 1; got != want {
		t.Fatalf("unexpected vm watcher count: got %d want %d", got, want)
	}
	if vmWatchers[0] != run {
		t.Fatalf("vm watchers should exclude qemu process")
	}
}

func TestProcessSetCloseStopsTasksBeforeProcesses(t *testing.T) {
	var order []string
	processes := NewProcessSet()
	process := (&executortest.Process{}).Process()
	process.SetShutdown(func() error {
		order = append(order, "process")
		return nil
	})
	processes.Add(process)

	processes.StartTasks(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		order = append(order, "task")
		return nil
	})

	if err := processes.Close(0); err != nil {
		t.Fatalf("close process set: %v", err)
	}
	if got, want := order, []string{"task", "process"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("unexpected close order: got %#v want %#v", got, want)
	}
}

func TestProcessSetConcurrentWatchersAndMutation(t *testing.T) {
	processes := NewProcessSet()
	processes.SetQEMU((&executortest.Process{Exited: true}).Process())

	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 200; i++ {
				process := (&executortest.Process{Exited: true}).Process()
				processes.Add(process)
				_ = processes.QEMU()
				watchers := processes.Watchers()
				_ = len(watchers.Processes())
				vmWatchers := processes.VMWatchers()
				_ = len(vmWatchers.Processes())
				processes.Remove(process)
			}
		}()
	}
	close(start)
	wg.Wait()
}
