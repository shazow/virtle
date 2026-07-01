package executor_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/shazow/virtle/internal/executor"
	"github.com/shazow/virtle/internal/executor/executortest"
)

func TestGroupCollectionSemantics(t *testing.T) {
	first := executor.Wrap(&executortest.Process{OverrideName: "first"})
	second := executor.Wrap(&executortest.Process{OverrideName: "second"})
	group := executor.NewGroup(first)
	snapshot := group.Snapshot()

	group.Add(nil, second)
	if got, want := group.Len(), 2; got != want {
		t.Fatalf("len after add: got %d want %d", got, want)
	}
	snapshot.Remove(first)

	if got, want := group.Processes(), []*executor.Process{first, second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("group changed unexpectedly: got %#v want %#v", got, want)
	}
	if got := snapshot.Len(); got != 0 {
		t.Fatalf("snapshot len: got %d want 0", got)
	}
	processes := group.Processes()
	processes[0] = nil

	if got, want := group.Processes(), []*executor.Process{first, second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("group was mutated through processes copy: got %#v want %#v", got, want)
	}
	if !group.Remove(first) {
		t.Fatal("expected first removal to succeed")
	}
	if group.Remove(first) {
		t.Fatal("expected second removal to fail")
	}
	if got, want := group.Processes(), []*executor.Process{second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("processes after remove: got %#v want %#v", got, want)
	}
}

func TestGroupFirstExit(t *testing.T) {
	first := executor.Wrap(&executortest.Process{OverrideName: "first"})
	secondProcess := &executortest.Process{OverrideName: "second"}
	second := executor.Wrap(secondProcess)
	group := executor.NewGroup(first, second)
	secondProcess.Complete(errors.New("second failed"))
	<-second.Done()

	process, err, ok := group.FirstExit()
	if !ok || process != second || err == nil || err.Error() != "second failed" {
		t.Fatalf("first exit: process=%v ok=%v err=%v", process, ok, err)
	}
}

func TestGroupStopAllStopsInReverseOrder(t *testing.T) {
	firstProcess := &executortest.Process{OverrideName: "first"}
	secondProcess := &executortest.Process{OverrideName: "second"}
	first := executor.Wrap(firstProcess)
	second := executor.Wrap(secondProcess)
	group := executor.NewGroup(first, second)

	if err := group.StopAll(time.Second); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	if got, want := []executortest.EventKind{secondProcess.EventKinds()[0], firstProcess.EventKinds()[0]}, []executortest.EventKind{executortest.EventSignal, executortest.EventSignal}; !reflect.DeepEqual(got, want) {
		t.Fatalf("events: got %v want %v", got, want)
	}
	if secondProcess.Events()[0].Sequence >= firstProcess.Events()[0].Sequence {
		t.Fatalf("expected second to stop before first: second=%d first=%d", secondProcess.Events()[0].Sequence, firstProcess.Events()[0].Sequence)
	}
}

func TestGroupZeroValueAndEmpty(t *testing.T) {
	var group executor.Group
	group.Add(nil)
	if group.Remove(nil) {
		t.Fatal("empty group unexpectedly removed process")
	}
	if group.Len() != 0 {
		t.Fatalf("empty group len: got %d want 0", group.Len())
	}
	if processes := group.Processes(); processes != nil {
		t.Fatalf("empty group processes: got %#v want nil", processes)
	}
	if process, err, ok := group.FirstExit(); process != nil || err != nil || ok {
		t.Fatalf("empty group first exit: process=%v err=%v ok=%v", process, err, ok)
	}
	if err := group.StopAll(time.Second); err != nil {
		t.Fatalf("empty group stop all: %v", err)
	}
}
