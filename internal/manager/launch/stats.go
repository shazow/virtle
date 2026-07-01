package launch

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shazow/virtle/internal/manager/control"
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

	if !started.IsZero() && !bootStarted.IsZero() {
		fields = append(fields, formatStatDuration("started_to_boot", bootStarted.Sub(started)))
	}
	if !bootStarted.IsZero() && !qmpReady.IsZero() {
		fields = append(fields, formatStatDuration("boot_to_qmp", qmpReady.Sub(bootStarted)))
	}
	if !qmpReady.IsZero() && !guestAgentReady.IsZero() {
		fields = append(fields, formatStatDuration("qmp_to_guest_agent", guestAgentReady.Sub(qmpReady)))
	}
	if !guestAgentReady.IsZero() && !filesReady.IsZero() {
		fields = append(fields, formatStatDuration("guest_agent_to_files", filesReady.Sub(guestAgentReady)))
	}
	if !filesReady.IsZero() && !firstSSHAttempt.IsZero() {
		fields = append(fields, formatStatDuration("files_to_first_ssh", firstSSHAttempt.Sub(filesReady)))
	}
	if !filesReady.IsZero() && !sshReady.IsZero() {
		fields = append(fields, formatStatDuration("files_to_ssh", sshReady.Sub(filesReady)))
	}
	if !bootStarted.IsZero() && !sshReady.IsZero() {
		fields = append(fields, formatStatDuration("boot_to_ssh", sshReady.Sub(bootStarted)))
	}
	if !sshStarted.IsZero() && !completed.IsZero() {
		fields = append(fields, formatStatDuration("ssh_to_completed", completed.Sub(sshStarted)))
	}
	if sshStarted.IsZero() && !bootStarted.IsZero() && !completed.IsZero() {
		fields = append(fields, formatStatDuration("boot_to_completed", completed.Sub(bootStarted)))
	}
	if !started.IsZero() && !completed.IsZero() {
		fields = append(fields, formatStatDuration("total", completed.Sub(started)))
	}
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
	if !started.IsZero() && !bootStarted.IsZero() {
		resp.StartedToBoot = bootStarted.Sub(started).String()
	}
	if !bootStarted.IsZero() && !qmpReady.IsZero() {
		resp.BootToQMP = qmpReady.Sub(bootStarted).String()
	}
	if sshReady.IsZero() {
		sshReady = sshStarted
	}
	if !filesReady.IsZero() && !sshReady.IsZero() {
		resp.FilesToSSH = sshReady.Sub(filesReady).String()
	}
	if !bootStarted.IsZero() && !completed.IsZero() {
		resp.BootToCompleted = completed.Sub(bootStarted).String()
	}
	if !started.IsZero() && !completed.IsZero() {
		resp.Total = completed.Sub(started).String()
	}
	return resp
}

func (s *Stats) time(event TimerEvent) time.Time {
	return s.snapshot().time(event)
}

func (s *Stats) count(event TimerEvent) int {
	return s.snapshot().count(event)
}

func (s *Stats) sshReady() time.Time {
	return s.snapshot().sshReady()
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

func formatStatDuration(name string, duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	return fmt.Sprintf("%s=%s", name, duration)
}
