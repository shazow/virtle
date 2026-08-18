package vmm

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/balloon"
	imanifest "github.com/shazow/virtle/internal/manifest"
)

func TestBuildQEMUCommandAppendsBalloonArgs(t *testing.T) {
	manifest := validManifestWithBalloon("/tmp/work")
	manifest.QEMU.Devices.Balloon.DeflateOnOOM = true
	manifest.QEMU.Devices.Balloon.FreePageReporting = true

	spec, err := buildQEMUCommand(manifest, 42, false)
	if err != nil {
		t.Fatalf("build qemu command: %v", err)
	}
	if !containsString(commandArgs(spec), "virtio-balloon-pci,id=balloon0,deflate-on-oom=on,free-page-reporting=on") {
		t.Fatalf("expected qemu args to include balloon device: %v", commandArgs(spec))
	}
}

func TestManagerLaunchStartsBalloonControllerAndStopsItBeforeQuit(t *testing.T) {
	tmpDir := t.TempDir()
	manifest := validManifestWithBalloon(tmpDir)
	manifest.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	manifest.QEMU.Devices.Balloon.Controller = &imanifest.BalloonControllerConfig{
		PollInterval:   1 * time.Second,
		ReclaimHoldoff: 1 * time.Second,
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner := &launchRunner{
		cancel:      cancel,
		cancelDelay: 2 * time.Second,
	}

	var quitAt time.Time
	qmpClient := (&fakeQMPClient{
		onQuit: func() {
			quitAt = time.Now()
			runner.exitQEMU(nil)
		},
		readBalloonStats: map[string]int64{
			"stat-available-memory": 900 * testMiB,
		},
		readBalloonStatsDelay:   400 * time.Millisecond,
		readBalloonStatsUpdated: time.Now(),
		queryBalloonActualBytes: 512 * testMiB,
	}).withDefaultBalloonPath("/machine/peripheral/balloon0")

	waiter := &fakeSocketWaiter{
		callback: func(paths []string) error {
			for _, path := range paths {
				if err := createStaleUnixSocketPath(path); err != nil {
					return err
				}
			}
			return nil
		},
	}

	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      waiter,
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		logger:            slog.New(slog.DiscardHandler),
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: time.Second,
		qmpQuitTimeout:    time.Second,
	}

	err := manager.launch(cancelCtx, manifest, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}

	if got, want := qmpClient.quitCount(), 1; got != want {
		t.Fatalf("expected qmp quit to be called once, got %d", got)
	}

	readCompleted := qmpClient.readCompletionTime()
	if readCompleted.IsZero() {
		t.Fatal("expected balloon controller poll to run before shutdown")
	}
	if quitAt.Before(readCompleted) {
		t.Fatalf("expected qmp quit after balloon controller stopped: quit=%s read-complete=%s", quitAt, readCompleted)
	}
}

func TestManagerLaunchDoesNotAbortOnBalloonControllerFailure(t *testing.T) {
	tmpDir := t.TempDir()
	manifest := validManifestWithBalloon(tmpDir)
	manifest.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")

	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runner := &launchRunner{cancel: cancel}
	qmpClient := (&fakeQMPClient{
		enableBalloonStatsErr: errors.New("guest stats unavailable"),
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}).withDefaultBalloonPath("/machine/peripheral/balloon0")

	waiter := &fakeSocketWaiter{
		callback: func(paths []string) error {
			for _, path := range paths {
				if err := createStaleUnixSocketPath(path); err != nil {
					return err
				}
			}
			return nil
		},
	}

	manager := &manager{
		locker:            &fileLocker{},
		runner:            runner,
		socketWaiter:      waiter,
		qmpDialer:         &fakeQMPDialer{client: qmpClient},
		logger:            slog.New(slog.DiscardHandler),
		shutdownDelay:     10 * time.Millisecond,
		qmpRetryDelay:     0,
		qmpConnectTimeout: time.Second,
		qmpQuitTimeout:    time.Second,
	}

	err := manager.launch(cancelCtx, manifest, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}

	if got, want := len(runner.sshArgs()), 1; got != want {
		t.Fatalf("expected ssh session to start despite balloon controller failure, got %d ssh starts", got)
	}
	if got, want := qmpClient.quitCount(), 1; got != want {
		t.Fatalf("expected qmp quit to still be used on teardown, got %d", got)
	}
}

func TestBalloonControllerTaskWithNilLoggerDoesNotPanicOnFailure(t *testing.T) {
	qmpClient := (&fakeQMPClient{
		enableBalloonStatsErr: errors.New("guest stats unavailable"),
	}).withDefaultBalloonPath("/machine/peripheral/balloon0")
	task := balloon.ControllerTask(qmpClient, &imanifest.BalloonDevice{
		ID:        "balloon0",
		Transport: "pci",
		Controller: &imanifest.BalloonControllerConfig{
			MinActual:             512,
			MaxActual:             1024,
			GrowBelowAvailable:    256,
			ReclaimAboveAvailable: 512,
			Step:                  256,
			PollInterval:          1 * time.Second,
			ReclaimHoldoff:        1 * time.Second,
		},
	}, nil)
	if task == nil {
		t.Fatal("expected balloon controller task")
	}

	if err := task(context.Background()); err != nil {
		t.Fatalf("expected nil task error, got %v", err)
	}
}

func TestBalloonControllerTaskWithNilLoggerDoesNotPanicOnAdjustment(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	qmpClient := (&fakeQMPClient{
		readBalloonStats: map[string]int64{
			"stat-available-memory": 500 * testMiB,
		},
		readBalloonStatsUpdated: time.Now(),
		queryBalloonActualBytes: 512 * testMiB,
	}).withDefaultBalloonPath("/machine/peripheral/balloon0")
	task := balloon.ControllerTask(qmpClient, &imanifest.BalloonDevice{
		ID:        "balloon0",
		Transport: "pci",
		Controller: &imanifest.BalloonControllerConfig{
			MinActual:             512,
			MaxActual:             1024,
			GrowBelowAvailable:    600,
			ReclaimAboveAvailable: 900,
			Step:                  256,
			PollInterval:          1 * time.Second,
			ReclaimHoldoff:        1 * time.Second,
		},
	}, nil)
	if task == nil {
		t.Fatal("expected balloon controller task")
	}

	done := make(chan error, 1)
	go func() {
		done <- task(ctx)
	}()
	time.Sleep(1100 * time.Millisecond)
	cancel()

	if err := <-done; err != nil {
		t.Fatalf("expected nil task error, got %v", err)
	}
	if got := len(qmpClient.setBalloonLogicalSizes); got == 0 {
		t.Fatal("expected balloon controller to adjust guest memory")
	}
}
