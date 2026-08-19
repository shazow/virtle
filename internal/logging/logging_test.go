package logging

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestPackageVerbosity(t *testing.T) {
	tests := []struct {
		name      string
		verbosity int
		pkg       string
		want      []string
		unwanted  []string
	}{
		{name: "default main", pkg: "main", want: []string{"notice", "warning"}, unwanted: []string{"debug", "info"}},
		{name: "default package", pkg: "vmm", want: []string{"notice", "warning"}, unwanted: []string{"debug", "info"}},
		{name: "verbose main", verbosity: 1, pkg: "main", want: []string{"info", "notice", "warning"}, unwanted: []string{"debug"}},
		{name: "verbose session", verbosity: 1, pkg: "session", want: []string{"info", "notice", "warning"}, unwanted: []string{"debug"}},
		{name: "verbose ssh", verbosity: 1, pkg: "ssh", want: []string{"info", "notice", "warning"}, unwanted: []string{"debug"}},
		{name: "verbose vmm", verbosity: 1, pkg: "vmm", want: []string{"notice", "warning"}, unwanted: []string{"debug", "info"}},
		{name: "debug package", verbosity: 2, pkg: "vmm", want: []string{"debug", "info", "notice", "warning"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := New(&output, tt.verbosity).With("package", tt.pkg)
			logger.Debug("debug")
			logger.Info("info")
			Notice(logger, "notice")
			logger.Warn("warning")

			logs := output.String()
			for _, message := range tt.want {
				if !strings.Contains(logs, "msg="+message) {
					t.Fatalf("expected %q in logs:\n%s", message, logs)
				}
			}
			for _, message := range tt.unwanted {
				if strings.Contains(logs, "msg="+message) {
					t.Fatalf("unexpected %q in logs:\n%s", message, logs)
				}
			}
			if !strings.Contains(logs, "package="+tt.pkg) {
				t.Fatalf("expected package attribute in logs:\n%s", logs)
			}
		})
	}
}

func TestNoticeLevelName(t *testing.T) {
	var output bytes.Buffer
	logger := New(&output, 0).With("package", "main")
	logger.Log(context.Background(), LevelNotice, "complete")
	if logs := output.String(); !strings.Contains(logs, "level=NOTICE") {
		t.Fatalf("expected NOTICE level, got %q", logs)
	}
}
