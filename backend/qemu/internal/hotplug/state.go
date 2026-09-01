package hotplug

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shazow/virtle/internal/manifest"
)

const (
	privateDirectoryMode os.FileMode = 0o700
	privateFileMode      os.FileMode = 0o600
)

type State struct {
	ID   string               `json:"id"`
	Kind manifest.HotplugKind `json:"kind"`
	Bus  string               `json:"bus"`
	PID  int                  `json:"pid,omitempty"`
}

func StatePath(stateDir string, id string) (string, error) {
	if strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("hotplug id %q must not contain path separators", id)
	}
	return filepath.Join(stateDir, "hotplug", id+".json"), nil
}

func WriteState(path string, state State) error {
	if err := os.MkdirAll(filepath.Dir(path), privateDirectoryMode); err != nil {
		return fmt.Errorf("create hotplug state directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode hotplug state: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, privateFileMode); err != nil {
		return fmt.Errorf("write hotplug state %q: %w", path, err)
	}
	return nil
}

func ReadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, fmt.Errorf("hotplug state %q does not exist; is this device attached?", path)
		}
		return State{}, fmt.Errorf("read hotplug state %q: %w", path, err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode hotplug state %q: %w", path, err)
	}
	if state.ID == "" || state.Bus == "" || state.Kind == "" {
		return State{}, fmt.Errorf("invalid hotplug state %q", path)
	}
	return state, nil
}
