package launch

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shazow/virtle/internal/control"
)

type TimerEvent string

const (
	TimerStarted         TimerEvent = "started"
	TimerBootStarted     TimerEvent = "boot_started"
	TimerQMPReady        TimerEvent = "qmp_ready"
	TimerGuestAgentReady TimerEvent = "guest_agent_ready"
	TimerFilesReady      TimerEvent = "files_ready"
	TimerSSHReady        TimerEvent = "ssh_ready"
	TimerSSHAttempt      TimerEvent = "ssh_attempt"
	TimerSSHStarted      TimerEvent = "ssh_started"
	TimerCompleted       TimerEvent = "completed"
)

type Stats struct {
	mu     sync.RWMutex
	timers map[TimerEvent]time.Time
	counts map[TimerEvent]int
}

type statsSnapshot struct {
	timers map[TimerEvent]time.Time
	counts map[TimerEvent]int
}

func NewStats(started time.Time) *Stats {
	stats := &Stats{}
	stats.Timer(TimerStarted, started)
	return stats
}

func (s *Stats) Timer(event TimerEvent, t time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timers == nil {
		s.timers = map[TimerEvent]time.Time{}
	}
	if s.counts == nil {
		s.counts = map[TimerEvent]int{}
	}
	if event == TimerSSHAttempt {
		s.counts[event]++
		if !s.timers[event].IsZero() || t.IsZero() {
			return
		}
	}
	s.timers[event] = t
}

func (s *Stats) String() string {
	snapshot := s.snapshot()
	var fields []string
	started := snapshot.time(TimerStarted)
	bootStarted := snapshot.time(TimerBootStarted)
	qmpReady := snapshot.time(TimerQMPReady)
	guestAgentReady := snapshot.time(TimerGuestAgentReady)
	filesReady := snapshot.time(TimerFilesReady)
	firstSSHAttempt := snapshot.time(TimerSSHAttempt)
	sshStarted := snapshot.time(TimerSSHStarted)
	completed := snapshot.time(TimerCompleted)
	sshReady := snapshot.sshReady()

	fields = appendStatDuration(fields, "started_to_boot", started, bootStarted)
	fields = appendStatDuration(fields, "boot_to_qmp", bootStarted, qmpReady)
	fields = appendStatDuration(fields, "qmp_to_guest_agent", qmpReady, guestAgentReady)
	fields = appendStatDuration(fields, "guest_agent_to_files", guestAgentReady, filesReady)
	fields = appendStatDuration(fields, "files_to_first_ssh", filesReady, firstSSHAttempt)
	fields = appendStatDuration(fields, "files_to_ssh", filesReady, sshReady)
	fields = appendStatDuration(fields, "boot_to_ssh", bootStarted, sshReady)
	fields = appendStatDuration(fields, "ssh_to_completed", sshStarted, completed)
	if sshStarted.IsZero() {
		fields = appendStatDuration(fields, "boot_to_completed", bootStarted, completed)
	}
	fields = appendStatDuration(fields, "total", started, completed)
	if attempts := snapshot.count(TimerSSHAttempt); attempts > 0 {
		fields = append(fields, fmt.Sprintf("ssh_attempts=%d", attempts))
	}
	return strings.Join(fields, " ")
}

func ControlStats(stats *Stats) control.RuntimeStats {
	if stats == nil {
		return control.RuntimeStats{}
	}
	snapshot := stats.snapshot()
	started := snapshot.time(TimerStarted)
	bootStarted := snapshot.time(TimerBootStarted)
	qmpReady := snapshot.time(TimerQMPReady)
	filesReady := snapshot.time(TimerFilesReady)
	sshReady := snapshot.time(TimerSSHReady)
	sshStarted := snapshot.time(TimerSSHStarted)
	completed := snapshot.time(TimerCompleted)

	resp := control.RuntimeStats{
		StartedAt:     started,
		BootStartedAt: bootStarted,
		QMPReadyAt:    qmpReady,
		FilesReadyAt:  filesReady,
		SSHReadyAt:    sshReady,
		SSHStartedAt:  sshStarted,
		CompletedAt:   completed,
		SSHAttempts:   snapshot.count(TimerSSHAttempt),
	}
	resp.StartedToBoot = statDurationString(started, bootStarted)
	resp.BootToQMP = statDurationString(bootStarted, qmpReady)
	if sshReady.IsZero() {
		sshReady = sshStarted
	}
	resp.FilesToSSH = statDurationString(filesReady, sshReady)
	resp.BootToCompleted = statDurationString(bootStarted, completed)
	resp.Total = statDurationString(started, completed)
	return resp
}

func (s *Stats) snapshot() statsSnapshot {
	if s == nil {
		return statsSnapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return statsSnapshot{
		timers: copyTimers(s.timers),
		counts: copyCounts(s.counts),
	}
}

func (s statsSnapshot) time(event TimerEvent) time.Time {
	if s.timers == nil {
		return time.Time{}
	}
	return s.timers[event]
}

func (s statsSnapshot) count(event TimerEvent) int {
	if s.counts == nil {
		return 0
	}
	return s.counts[event]
}

func (s statsSnapshot) sshReady() time.Time {
	sshReady := s.time(TimerSSHReady)
	if sshReady.IsZero() {
		sshReady = s.time(TimerSSHStarted)
	}
	return sshReady
}

func copyTimers(src map[TimerEvent]time.Time) map[TimerEvent]time.Time {
	if src == nil {
		return nil
	}
	dst := make(map[TimerEvent]time.Time, len(src))
	for event, timestamp := range src {
		dst[event] = timestamp
	}
	return dst
}

func copyCounts(src map[TimerEvent]int) map[TimerEvent]int {
	if src == nil {
		return nil
	}
	dst := make(map[TimerEvent]int, len(src))
	for event, count := range src {
		dst[event] = count
	}
	return dst
}

func appendStatDuration(fields []string, name string, from, to time.Time) []string {
	if from.IsZero() || to.IsZero() {
		return fields
	}
	return append(fields, formatStatDuration(name, to.Sub(from)))
}

func statDurationString(from, to time.Time) string {
	if from.IsZero() || to.IsZero() {
		return ""
	}
	return to.Sub(from).String()
}

func formatStatDuration(name string, duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	return fmt.Sprintf("%s=%s", name, duration)
}
