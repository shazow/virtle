package manifest

import (
	"reflect"
	"testing"

	"github.com/shazow/virtle/internal/executor"
)

func TestRenderCommandTemplatesAndMergesEnv(t *testing.T) {
	renderer, err := executor.NewWithEnviron(executor.Context{
		"State":   "runtime:resume",
		"Message": "Restored",
	}, nil)
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	command, err := RenderCommand(Command{
		Path: "notify-{{.State}}",
		Args: []string{"{{.Message}}"},
		Env:  []string{"EXISTING=1"},
	}, renderer)
	if err != nil {
		t.Fatalf("render command: %v", err)
	}

	if got, want := command.Path, "notify-runtime:resume"; got != want {
		t.Fatalf("unexpected path: got %q want %q", got, want)
	}
	if got, want := command.Args, []string{"Restored"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected args: got %#v want %#v", got, want)
	}
	if got, want := command.Env, []string{"EXISTING=1", "MESSAGE=Restored", "STATE=runtime:resume"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected env: got %#v want %#v", got, want)
	}
}
