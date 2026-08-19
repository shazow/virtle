package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestBuildVersionPrefersStampedVersion(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}
	if got := buildVersion("1.2", info); got != "1.2" {
		t.Fatalf("buildVersion stamped = %q, want %q", got, "1.2")
	}
}

func TestBuildVersionFallsBackToBuildInfo(t *testing.T) {
	tests := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{
			name: "module version",
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.2.1"}},
			want: "v0.2.1",
		},
		{
			name: "checkout pseudo-version",
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20260818144734-ab30dd63d8f6+dirty"}},
			want: "v0.0.0-20260818144734-ab30dd63d8f6+dirty",
		},
		{
			name: "no vcs stamps",
			info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			want: "devel",
		},
		{
			name: "no build info",
			info: nil,
			want: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildVersion("", tt.info); got != tt.want {
				t.Fatalf("buildVersion = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunVersionPrintsCompiledVersion(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "alone", args: []string{"--version"}},
		{name: "before command", args: []string{"--version", "launch"}},
		{name: "after command", args: []string{"launch", "--version"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := captureStdout(t, func() {
				if err := run(tt.args); err != nil {
					t.Fatalf("run(%v): %v", tt.args, err)
				}
			})

			want := "virtle " + versionString() + "\n"
			if output != want {
				t.Fatalf("run(%v) printed %q, want %q", tt.args, output, want)
			}
			if strings.TrimSpace(versionString()) == "" {
				t.Fatalf("versionString() is empty")
			}
		})
	}
}
