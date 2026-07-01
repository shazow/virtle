package balloon

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

func TestEvaluateGrowsGuestMemory(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	config := ControllerConfig{
		MinActual:             256,
		MaxActual:             1024,
		GrowBelowAvailable:    128,
		ReclaimAboveAvailable: 768,
		Step:                  128,
		PollIntervalSeconds:   5,
		ReclaimHoldoffSeconds: 30,
	}
	state := &controllerState{startedAt: now.Add(-time.Minute)}

	target, apply, err := evaluate(config, state, now, int64(512)*bytesPerMiB, guestStatsSample{
		AvailableMemoryBytes: int64(64) * bytesPerMiB,
		HasAvailableMemory:   true,
		LastUpdate:           now,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !apply {
		t.Fatal("expected grow decision")
	}
	if got, want := target, int64(640)*bytesPerMiB; got != want {
		t.Fatalf("unexpected grow target: got %d want %d", got, want)
	}
}

func TestEvaluateReclaimsAfterHoldoff(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	config := ControllerConfig{
		MinActual:             256,
		MaxActual:             1024,
		GrowBelowAvailable:    128,
		ReclaimAboveAvailable: 768,
		Step:                  128,
		PollIntervalSeconds:   5,
		ReclaimHoldoffSeconds: 30,
	}
	state := &controllerState{startedAt: now.Add(-time.Minute)}

	if target, apply, err := evaluate(config, state, now, int64(512)*bytesPerMiB, guestStatsSample{
		AvailableMemoryBytes: int64(900) * bytesPerMiB,
		HasAvailableMemory:   true,
		LastUpdate:           now,
	}); err != nil || apply || target != 0 {
		t.Fatalf("expected holdoff arm only, got target=%d apply=%v err=%v", target, apply, err)
	}

	target, apply, err := evaluate(config, state, now.Add(30*time.Second), int64(512)*bytesPerMiB, guestStatsSample{
		AvailableMemoryBytes: int64(900) * bytesPerMiB,
		HasAvailableMemory:   true,
		LastUpdate:           now.Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !apply {
		t.Fatal("expected reclaim decision")
	}
	if got, want := target, int64(384)*bytesPerMiB; got != want {
		t.Fatalf("unexpected reclaim target: got %d want %d", got, want)
	}
}

func TestEvaluateNoopsWithinHysteresis(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	config := ControllerConfig{
		MinActual:             256,
		MaxActual:             1024,
		GrowBelowAvailable:    128,
		ReclaimAboveAvailable: 768,
		Step:                  128,
		PollIntervalSeconds:   5,
		ReclaimHoldoffSeconds: 30,
	}
	state := &controllerState{
		startedAt:           now.Add(-time.Minute),
		aboveThresholdSince: now.Add(-time.Second),
	}

	target, apply, err := evaluate(config, state, now, int64(512)*bytesPerMiB, guestStatsSample{
		AvailableMemoryBytes: int64(512) * bytesPerMiB,
		HasAvailableMemory:   true,
		LastUpdate:           now,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if apply || target != 0 {
		t.Fatalf("expected no-op, got target=%d apply=%v", target, apply)
	}
	if !state.aboveThresholdSince.IsZero() {
		t.Fatalf("expected reclaim holdoff to reset, got %s", state.aboveThresholdSince)
	}
}

func TestEvaluateRejectsStaleStats(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	config := ControllerConfig{
		MinActual:             256,
		MaxActual:             1024,
		GrowBelowAvailable:    128,
		ReclaimAboveAvailable: 768,
		Step:                  128,
		PollIntervalSeconds:   5,
		ReclaimHoldoffSeconds: 30,
	}
	state := &controllerState{startedAt: now.Add(-time.Minute)}

	_, _, err := evaluate(config, state, now, int64(512)*bytesPerMiB, guestStatsSample{
		AvailableMemoryBytes: int64(512) * bytesPerMiB,
		HasAvailableMemory:   true,
		LastUpdate:           now.Add(-11 * time.Second),
	})
	if !errors.Is(err, errGuestStatsStale) {
		t.Fatalf("expected stale stats error, got %v", err)
	}
}

func TestEvaluateRejectsUnavailableStats(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	config := ControllerConfig{
		MinActual:             256,
		MaxActual:             1024,
		GrowBelowAvailable:    128,
		ReclaimAboveAvailable: 768,
		Step:                  128,
		PollIntervalSeconds:   5,
		ReclaimHoldoffSeconds: 30,
	}
	state := &controllerState{startedAt: now.Add(-11 * time.Second)}

	_, _, err := evaluate(config, state, now, int64(512)*bytesPerMiB, guestStatsSample{})
	if !errors.Is(err, errGuestStatsUnavailable) {
		t.Fatalf("expected unavailable stats error, got %v", err)
	}
}

func TestEvaluateClampsToBounds(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	config := ControllerConfig{
		MinActual:             256,
		MaxActual:             1024,
		GrowBelowAvailable:    128,
		ReclaimAboveAvailable: 768,
		Step:                  256,
		PollIntervalSeconds:   5,
		ReclaimHoldoffSeconds: 30,
	}
	state := &controllerState{startedAt: now.Add(-time.Minute)}

	target, apply, err := evaluate(config, state, now, int64(960)*bytesPerMiB, guestStatsSample{
		AvailableMemoryBytes: int64(64) * bytesPerMiB,
		HasAvailableMemory:   true,
		LastUpdate:           now,
	})
	if err != nil || !apply {
		t.Fatalf("expected clamped grow, got target=%d apply=%v err=%v", target, apply, err)
	}
	if got, want := target, int64(1024)*bytesPerMiB; got != want {
		t.Fatalf("unexpected max clamp: got %d want %d", got, want)
	}

	state = &controllerState{
		startedAt:           now.Add(-time.Minute),
		aboveThresholdSince: now.Add(-31 * time.Second),
	}
	target, apply, err = evaluate(config, state, now, int64(300)*bytesPerMiB, guestStatsSample{
		AvailableMemoryBytes: int64(900) * bytesPerMiB,
		HasAvailableMemory:   true,
		LastUpdate:           now,
	})
	if err != nil || !apply {
		t.Fatalf("expected clamped reclaim, got target=%d apply=%v err=%v", target, apply, err)
	}
	if got, want := target, int64(256)*bytesPerMiB; got != want {
		t.Fatalf("unexpected min clamp: got %d want %d", got, want)
	}
}

func TestAvailableMemoryFallsBackToFreeMemory(t *testing.T) {
	available, ok := availableMemory(stats{
		Stats: map[string]int64{
			"stat-free-memory": 1234,
		},
	})
	if !ok {
		t.Fatal("expected fallback to stat-free-memory")
	}
	if got, want := available, int64(1234); got != want {
		t.Fatalf("unexpected available memory: got %d want %d", got, want)
	}
}

func TestControllerResolveQOMPathFallsBackToQOMList(t *testing.T) {
	session := &fakeSession{
		listQOMProperties: map[string][]objectPropertyInfo{
			"/machine/peripheral": {
				{Name: "rng0", Type: "child<virtio-rng-device>"},
			},
			"/machine/peripheral-anon": {
				{Name: "device[1]", Type: "child<virtio-balloon-device>"},
			},
			"/machine/peripheral-anon/device[1]": {
				{Name: "guest-stats", Type: "dict"},
				{Name: "guest-stats-polling-interval", Type: "int"},
			},
		},
		listQOMPropertiesErr: map[string]error{
			"/machine/peripheral/balloon0": errors.New("not found"),
		},
	}

	controller := &controller{
		Session:    session,
		Logger:     slog.New(slog.DiscardHandler),
		DeviceID:   "balloon0",
		QMPTimeout: time.Second,
	}

	path, err := controller.resolveQOMPath()
	if err != nil {
		t.Fatalf("resolve balloon qom path: %v", err)
	}
	if got, want := path, "/machine/peripheral-anon/device[1]"; got != want {
		t.Fatalf("unexpected balloon qom path: got %q want %q", got, want)
	}
}

func TestControllerNotifiesAfterSuccessfulResize(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	now := time.Now()
	notifications := &fakeNotifier{}
	session := &fakeSession{
		queryBalloonInfo: info{ActualBytes: 4 * 1024 * bytesPerMiB},
		readBalloonStats: stats{
			Stats: map[string]int64{
				"stat-available-memory": 64 * bytesPerMiB,
			},
			LastUpdate: now,
		},
		listQOMProperties: map[string][]objectPropertyInfo{
			"/machine/peripheral/balloon0": {
				{Name: "guest-stats", Type: "dict"},
				{Name: "guest-stats-polling-interval", Type: "int"},
			},
		},
	}
	controller := &controller{
		Session:    session,
		Logger:     slog.New(slog.DiscardHandler),
		DeviceID:   "balloon0",
		QMPTimeout: time.Second,
		Config: ControllerConfig{
			MinActual:             1024,
			MaxActual:             8192,
			GrowBelowAvailable:    128,
			ReclaimAboveAvailable: 4096,
			Step:                  2048,
			PollIntervalSeconds:   1,
			ReclaimHoldoffSeconds: 1,
		},
		Notifier: notifications,
		Now:      func() time.Time { return now },
	}

	done := make(chan error, 1)
	go func() {
		done <- controller.Run(ctx)
	}()
	time.Sleep(1100 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run controller: %v", err)
	}

	if got, want := len(session.setBalloonLogicalSizes), 1; got != want {
		t.Fatalf("expected one resize, got %d", got)
	}
	if got, want := len(notifications.calls), 1; got != want {
		t.Fatalf("expected one notification, got %d", got)
	}
	call := notifications.calls[0]
	if call.state != "balloon:resize" {
		t.Fatalf("unexpected notification state: got %q", call.state)
	}
	if call.message != "Growing guest memory to 6GB" {
		t.Fatalf("unexpected notification message: got %q", call.message)
	}
	if call.values["target_mib"] != "6144" || call.values["delta_mib"] != "2048" {
		t.Fatalf("unexpected notification values: %#v", call.values)
	}
}

func TestControllerDoesNotNotifyWhenResizeIsNotApplied(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	now := time.Now()
	notifications := &fakeNotifier{}
	session := &fakeSession{
		queryBalloonInfo: info{ActualBytes: 4 * 1024 * bytesPerMiB},
		readBalloonStats: stats{
			Stats: map[string]int64{
				"stat-available-memory": 2 * 1024 * bytesPerMiB,
			},
			LastUpdate: now,
		},
		listQOMProperties: map[string][]objectPropertyInfo{
			"/machine/peripheral/balloon0": {
				{Name: "guest-stats", Type: "dict"},
				{Name: "guest-stats-polling-interval", Type: "int"},
			},
		},
	}
	controller := &controller{
		Session:    session,
		Logger:     slog.New(slog.DiscardHandler),
		DeviceID:   "balloon0",
		QMPTimeout: time.Second,
		Config: ControllerConfig{
			MinActual:             1024,
			MaxActual:             8192,
			GrowBelowAvailable:    128,
			ReclaimAboveAvailable: 4096,
			Step:                  2048,
			PollIntervalSeconds:   1,
			ReclaimHoldoffSeconds: 1,
		},
		Notifier: notifications,
		Now:      func() time.Time { return now },
	}

	done := make(chan error, 1)
	go func() {
		done <- controller.Run(ctx)
	}()
	time.Sleep(1100 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run controller: %v", err)
	}

	if got := len(session.setBalloonLogicalSizes); got != 0 {
		t.Fatalf("expected no resize, got %d", got)
	}
	if got := len(notifications.calls); got != 0 {
		t.Fatalf("expected no notification, got %d", got)
	}
}

type fakeSession struct {
	queryBalloonInfo       info
	queryBalloonErr        error
	setBalloonErr          error
	setBalloonLogicalSizes []int64
	enableBalloonStatsErr  error
	readBalloonStats       stats
	readBalloonStatsErr    error
	listQOMProperties      map[string][]objectPropertyInfo
	listQOMPropertiesErr   map[string]error
}

func (f *fakeSession) QueryBalloon(timeout time.Duration) (info, error) {
	if f.queryBalloonErr != nil {
		return info{}, f.queryBalloonErr
	}
	return f.queryBalloonInfo, nil
}

func (f *fakeSession) SetBalloonLogicalSize(timeout time.Duration, logicalSizeBytes int64) error {
	f.setBalloonLogicalSizes = append(f.setBalloonLogicalSizes, logicalSizeBytes)
	return f.setBalloonErr
}

func (f *fakeSession) EnableBalloonStatsPolling(timeout time.Duration, qomPath string, pollIntervalSeconds int) error {
	return f.enableBalloonStatsErr
}

type notificationCall struct {
	state   string
	message string
	values  map[string]string
}

type fakeNotifier struct {
	calls []notificationCall
}

func (f *fakeNotifier) Notify(ctx context.Context, state string, message string, values map[string]string) {
	f.calls = append(f.calls, notificationCall{
		state:   state,
		message: message,
		values:  values,
	})
}

func (f *fakeSession) ReadBalloonStats(timeout time.Duration, qomPath string) (stats, error) {
	if f.readBalloonStatsErr != nil {
		return stats{}, f.readBalloonStatsErr
	}
	return f.readBalloonStats, nil
}

func (f *fakeSession) ListQOMProperties(timeout time.Duration, path string) ([]objectPropertyInfo, error) {
	if err, ok := f.listQOMPropertiesErr[path]; ok {
		return nil, err
	}
	if props, ok := f.listQOMProperties[path]; ok {
		return append([]objectPropertyInfo(nil), props...), nil
	}
	return nil, errors.New("unexpected qom-list path")
}
