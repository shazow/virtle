package qga

import (
	"sort"
	"strings"
)

type Process struct {
	User    string
	Command string
}

func FormatProcessListExecData(data string) string {
	return FormatProcesses(ParseProcesses(DecodeExecData(data)))
}

func ParseProcesses(output string) []Process {
	var processes []Process
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		processes = append(processes, Process{
			User:    fields[0],
			Command: fields[1],
		})
	}
	return processes
}

func FormatProcesses(processes []Process) string {
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
