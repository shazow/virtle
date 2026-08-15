package qga

import (
	"encoding/base64"
	"testing"
)

func TestFormatProcessListExecDataSortsProcesses(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("root zsh\nagent sshd\nroot init\n"))

	got := FormatProcessListExecData(data)
	want := "USER COMMAND\nagent sshd\nroot init\nroot zsh"
	if got != want {
		t.Fatalf("process list:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestParseProcessesSkipsMalformedLines(t *testing.T) {
	got := ParseProcesses("root\nagent sshd\n\n")
	if len(got) != 1 || got[0].User != "agent" || got[0].Command != "sshd" {
		t.Fatalf("processes: %#v", got)
	}
}

func TestFormatProcessesEmpty(t *testing.T) {
	if got := FormatProcesses(nil); got != "" {
		t.Fatalf("empty process list: %q", got)
	}
}
