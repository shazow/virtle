package backendtest

import (
	"testing"

	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/vm"
	"github.com/shazow/virtle/vm/vmtest"
)

func TestInMemoryBackendConforms(t *testing.T) {
	TestBackend(t, func(t *testing.T) (backend.Backend, *vm.Spec) {
		return &Backend{Guest: &vmtest.Guest{Commands: map[string]vmtest.Result{"true": {}}}}, &vm.Spec{}
	})
}
