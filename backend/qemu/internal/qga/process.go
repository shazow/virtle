package qga

import (
	"sort"
	"strings"
)

type process struct {
	User    string
	Command string
}

// FormatProcessListExecData turns the base64 output of a guest ps listing
// into the "USER COMMAND" table shown in guest diagnostics.
func FormatProcessListExecData(data string) string {
	return formatProcesses(parseProcesses(DecodeExecData(data)))
}

func parseProcesses(output string) []process {
	var processes []process
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		processes = append(processes, process{
			User:    fields[0],
			Command: fields[1],
		})
	}
	return processes
}

func formatProcesses(processes []process) string {
	if len(processes) == 0 {
		return ""
	}

	sort.Slice(processes, func(i, j int) bool {
		if processes[i].User != processes[j].User {
			return processes[i].User < processes[j].User
		}
		return processes[i].Command < processes[j].Command
	})

	var builder strings.Builder
	builder.WriteString("USER COMMAND\n")
	for _, process := range processes {
		builder.WriteString(process.User)
		builder.WriteByte(' ')
		builder.WriteString(process.Command)
		builder.WriteByte('\n')
	}
	return strings.TrimRight(builder.String(), "\n")
}
