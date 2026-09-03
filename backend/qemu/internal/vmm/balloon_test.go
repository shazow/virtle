package vmm

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shazow/virtle/backend/qemu/internal/balloon"
	"github.com/shazow/virtle/backend/qemu/internal/launch"
	imanifest "github.com/shazow/virtle/internal/manifest"
)

func TestBuildQEMUCommandAppendsBalloonArgs(t *testing.T) {
	manifest := validManifestWithBalloon("/tmp/work")
	manifest.QEMU.Devices.Balloon.DeflateOnOOM = true
	manifest.QEMU.Devices.Balloon.FreePageReporting = true

	spec, err := buildTestQEMUCommand(manifest, 42, false)
	if err != nil {
		t.Fatalf("build qemu command: %v", err)
	}
	if !containsString(commandArgs(spec), "virtio-balloon-pci,id=balloon0,deflate-on-oom=on,free-page-reporting=on") {
		t.Fatalf("expected qemu args to include balloon device: %v", commandArgs(spec))
	}
}

func TestStartVMStopsBalloonControllerBeforeQEMU(t *testing.T) {
	tmpDir := t.TempDir()
	launchManifest := validManifestWithBalloon(tmpDir)
	launchManifest.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")
	launchManifest.QEMU.Devices.Balloon.Controller = &imanifest.BalloonControllerConfig{
		PollInterval:   time.Nanosecond,
		ReclaimHoldoff: time.Second,
	}

	runner := &launchRunner{}
	readDone := make(chan struct{})
	var readOnce sync.Once
	var readAt, quitAt time.Time
	qmpClient := (&fakeQMPClient{
		onQuit: func() {
			quitAt = time.Now()
			runner.exitQEMU(nil)
		},
		onReadBalloonStats: func() {
			readOnce.Do(func() {
				readAt = time.Now()
				close(readDone)
			})
		},
		readBalloonStats: map[string]int64{
			"stat-available-memory": 900 * testMiB,
		},
		readBalloonStatsUpdated: time.Now(),
		queryBalloonActualBytes: 512 * testMiB,
	}).withDefaultBalloonPath("/machine/peripheral/balloon0")

	manager := newManagerFromConfig(Config{
		Locker:            &fileLocker{},
		Runner:            runner,
		SocketWaiter:      &fakeSocketWaiter{callback: createBalloonTestSockets},
		QMPDialer:         &fakeQMPDialer{client: qmpClient},
		Logger:            slog.New(slog.DiscardHandler),
		ShutdownDelay:     10 * time.Millisecond,
		QMPConnectTimeout: time.Second,
		QMPQuitTimeout:    time.Second,
	})
	v, err := manager.startVM(context.Background(), launch.Spec{Manifest: launchManifest})
	if err != nil {
		t.Fatalf("start VM: %v", err)
	}
	<-readDone
	if err := v.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown VM: %v", err)
	}
	if quitAt.Before(readAt) {
		t.Fatalf("QEMU quit before balloon controller stopped: quit=%s read=%s", quitAt, readAt)
	}
}

func TestStartVMContinuesWhenBalloonControllerFails(t *testing.T) {
	tmpDir := t.TempDir()
	launchManifest := validManifestWithBalloon(tmpDir)
	launchManifest.Paths.LockPath = filepath.Join(tmpDir, "virtle.lock")

	runner := &launchRunner{}
	controllerStarted := make(chan struct{})
	qmpClient := (&fakeQMPClient{
		enableBalloonStatsErr: errors.New("guest stats unavailable"),
		onEnableBalloonStats: func() {
			close(controllerStarted)
		},
		onQuit: func() {
			runner.exitQEMU(nil)
		},
	}).withDefaultBalloonPath("/machine/peripheral/balloon0")

	manager := newManagerFromConfig(Config{
		Locker:            &fileLocker{},
		Runner:            runner,
		SocketWaiter:      &fakeSocketWaiter{callback: createBalloonTestSockets},
		QMPDialer:         &fakeQMPDialer{client: qmpClient},
		Logger:            slog.New(slog.DiscardHandler),
		ShutdownDelay:     10 * time.Millisecond,
		QMPConnectTimeout: time.Second,
		QMPQuitTimeout:    time.Second,
	})
	v, err := manager.startVM(context.Background(), launch.Spec{Manifest: launchManifest})
	if err != nil {
		t.Fatalf("start VM: %v", err)
	}
	<-controllerStarted
	if err := v.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown VM: %v", err)
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
	}, nil, nil)
	if task == nil {
		t.Fatal("expected balloon controller task")
	}

	if err := task(context.Background()); err != nil {
		t.Fatalf("expected nil task error, got %v", err)
	}
}

func createBalloonTestSockets(paths []string) error {
	for _, path := range paths {
		if err := createStaleUnixSocketPath(path); err != nil {
			return err
		}
	}
	return nil
}

func TestBalloonControllerTaskWithNilLoggerDoesNotPanicOnAdjustment(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The fake stamps each stats read with the current time (whole seconds,
	// as QEMU reports last-update) and cancels the task once the controller
	// applies its first resize, so the test waits for exactly one poll tick.
	qmpClient := (&fakeQMPClient{
		readBalloonStats: map[string]int64{
			"stat-available-memory": 500 * testMiB,
		},
		queryBalloonActualBytes: 512 * testMiB,
		onSetBalloon:            cancel,
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
			PollInterval:          time.Second,
			ReclaimHoldoff:        time.Second,
		},
	}, nil, nil)
	if task == nil {
		t.Fatal("expected balloon controller task")
	}

	done := make(chan error, 1)
	go func() {
		done <- task(ctx)
	}()

	if err := <-done; err != nil {
		t.Fatalf("expected nil task error, got %v", err)
	}
	if got := len(qmpClient.setBalloonLogicalSizes); got == 0 {
		t.Fatal("expected balloon controller to adjust guest memory")
	}
}
