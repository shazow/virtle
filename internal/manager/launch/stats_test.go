package launch

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStatsStringFromTimerEvents(t *testing.T) {
	started := time.Unix(100, 0)
	stats := NewStats(started)
	stats.Timer(TimerBootStarted, started.Add(10*time.Millisecond))
	stats.Timer(TimerQMPReady, started.Add(30*time.Millisecond))
	stats.Timer(TimerGuestAgentReady, started.Add(40*time.Millisecond))
	stats.Timer(TimerFilesReady, started.Add(60*time.Millisecond))
	stats.Timer(TimerSSHAttempt, started.Add(80*time.Millisecond))
	stats.Timer(TimerSSHReady, started.Add(90*time.Millisecond))
	stats.Timer(TimerSSHStarted, started.Add(100*time.Millisecond))
	stats.Timer(TimerCompleted, started.Add(150*time.Millisecond))

	got := stats.String()
	for _, want := range []string{
		"started_to_boot=10ms",
		"boot_to_qmp=20ms",
		"qmp_to_guest_agent=10ms",
		"guest_agent_to_files=20ms",
		"files_to_first_ssh=20ms",
		"files_to_ssh=30ms",
		"boot_to_ssh=80ms",
		"ssh_to_completed=50ms",
		"total=150ms",
		"ssh_attempts=1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stats string %q missing %q", got, want)
		}
	}

	fallback := NewStats(started)
	fallback.Timer(TimerBootStarted, started.Add(10*time.Millisecond))
	fallback.Timer(TimerCompleted, started.Add(5*time.Millisecond))
	got = fallback.String()
	for _, want := range []string{
		"boot_to_completed=0s",
		"total=5ms",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("fallback stats string %q missing %q", got, want)
		}
	}
}

func TestStatsTimerSSHAttemptKeepsFirstAttemptAndCountsAllAttempts(t *testing.T) {
	started := time.Unix(200, 0)
	stats := NewStats(started)
	stats.Timer(TimerFilesReady, started.Add(10*time.Millisecond))
	stats.Timer(TimerSSHAttempt, started.Add(20*time.Millisecond))
	stats.Timer(TimerSSHAttempt, started.Add(30*time.Millisecond))
	stats.Timer(TimerSSHReady, started.Add(50*time.Millisecond))

	got := stats.String()
	for _, want := range []string{
		"files_to_first_ssh=10ms",
		"files_to_ssh=40ms",
		"ssh_attempts=2",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stats string %q missing %q", got, want)
		}
	}
	if got := ControlStats(stats).SSHAttempts; got != 2 {
		t.Fatalf("ssh attempts: got %d want 2", got)
	}
}

func TestStatsTimerSSHAttemptZeroTimeDoesNotBlockFirstRealAttempt(t *testing.T) {
	started := time.Unix(250, 0)
	stats := NewStats(started)
	stats.Timer(TimerFilesReady, started.Add(10*time.Millisecond))
	stats.Timer(TimerSSHAttempt, time.Time{})
	stats.Timer(TimerSSHAttempt, started.Add(30*time.Millisecond))

	got := stats.String()
	for _, want := range []string{
		"files_to_first_ssh=20ms",
		"ssh_attempts=2",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stats string %q missing %q", got, want)
		}
	}
}

func TestStatsConcurrentTimerAndReadAccess(t *testing.T) {
	started := time.Unix(275, 0)
	stats := NewStats(started)
	stats.Timer(TimerBootStarted, started.Add(time.Millisecond))
	stats.Timer(TimerFilesReady, started.Add(2*time.Millisecond))

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 1000 {
				now := started.Add(time.Duration(i*1000+j) * time.Millisecond)
				stats.Timer(TimerSSHAttempt, now)
				stats.Timer(TimerSSHStarted, now)
				stats.Timer(TimerCompleted, now)
			}
		}(i)
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				_ = stats.String()
				_ = ControlStats(stats)
			}
		}()
	}
	wg.Wait()
}

func TestControlStatsFromTimerEvents(t *testing.T) {
	started := time.Unix(300, 0)
	stats := NewStats(started)
	stats.Timer(TimerBootStarted, started.Add(time.Second))
	stats.Timer(TimerQMPReady, started.Add(2*time.Second))
	stats.Timer(TimerGuestAgentReady, started.Add(2500*time.Millisecond))
	stats.Timer(TimerFilesReady, started.Add(3*time.Second))
	stats.Timer(TimerSSHAttempt, started.Add(4*time.Second))
	stats.Timer(TimerSSHStarted, started.Add(5*time.Second))
	stats.Timer(TimerCompleted, started.Add(8*time.Second))

	got := ControlStats(stats)
	if got.StartedAt != started ||
		got.BootStartedAt != started.Add(time.Second) ||
		got.QMPReadyAt != started.Add(2*time.Second) ||
		got.FilesReadyAt != started.Add(3*time.Second) ||
		got.SSHStartedAt != started.Add(5*time.Second) ||
		got.CompletedAt != started.Add(8*time.Second) {
		t.Fatalf("unexpected timestamps: %#v", got)
	}
	if !got.SSHReadyAt.IsZero() {
		t.Fatalf("expected zero ssh ready timestamp when only ssh started was recorded: %#v", got)
	}
	if got.StartedToBoot != "1s" ||
		got.BootToQMP != "1s" ||
		got.FilesToSSH != "2s" ||
		got.BootToCompleted != "7s" ||
		got.Total != "8s" {
		t.Fatalf("unexpected durations: %#v", got)
	}
	if got.SSHAttempts != 1 {
		t.Fatalf("unexpected ssh attempts: got %d want 1", got.SSHAttempts)
	}
}
