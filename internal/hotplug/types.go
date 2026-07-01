package hotplug

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Kind string

const (
	KindVirtioFS Kind = "virtiofs"
	KindNet      Kind = "net"
	KindBlock    Kind = "block"
)

type Device struct {
	Kind     Kind     `json:"kind"`
	ID       string   `json:"id"`
	VirtioFS VirtioFS `json:"virtiofs,omitempty"`
	Net      Net      `json:"net,omitempty"`
	Block    Block    `json:"block,omitempty"`
}

type VirtioFS struct {
	Source     string   `json:"source"`
	Target     string   `json:"target,omitempty"`
	SocketPath string   `json:"socketPath"`
	Bin        string   `json:"bin"`
	Args       []string `json:"args,omitempty"`
}

type Net struct {
	Backend string    `json:"backend"`
	MAC     string    `json:"mac"`
	Forward []Forward `json:"forward,omitempty"`
}

type Forward struct {
	Proto string `json:"proto"`
	Host  string `json:"host"`
	Guest string `json:"guest"`
}

type Block struct {
	ImagePath string `json:"imagePath"`
	Format    string `json:"format"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
	Serial    string `json:"serial,omitempty"`
}

type State struct {
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`
	Bus  string `json:"bus"`
	PID  int    `json:"pid,omitempty"`
}

func StatePath(stateDir string, id string) (string, error) {
	if strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("hotplug id %q must not contain path separators", id)
	}
	return filepath.Join(stateDir, "hotplug", id+".json"), nil
}

func WriteState(path string, state State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create hotplug state directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode hotplug state: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
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

func DefaultVirtioFSArgs(socketPath string, source string, id string) []string {
	return []string{
		"--socket-path=" + socketPath,
		"--shared-dir=" + source,
		"--tag=" + id,
	}
}
