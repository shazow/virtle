package balloon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

var (
	errGuestStatsUnavailable = errors.New("balloon guest stats unavailable")
	errGuestStatsStale       = errors.New("balloon guest stats stale")
	errQOMPathNotFound       = errors.New("balloon qom path not found")
)

const bytesPerMiB int64 = 1024 * 1024

// maxConsecutiveFailures bounds how many consecutive cycle failures (QMP
// transport errors, stale or missing guest stats) the controller tolerates
// before giving up. Transient hiccups must not permanently disable ballooning.
const maxConsecutiveFailures = 5

type notifier interface {
	Notify(ctx context.Context, state string, message string, values map[string]string)
}

type session interface {
	QueryBalloon(ctx context.Context) (info, error)
	SetBalloonLogicalSize(ctx context.Context, logicalSizeBytes int64) error
	EnableBalloonStatsPolling(ctx context.Context, qomPath string, pollIntervalSeconds int) error
	ReadBalloonStats(ctx context.Context, qomPath string) (stats, error)
	ListQOMProperties(ctx context.Context, path string) ([]objectPropertyInfo, error)
}

type info struct {
	ActualBytes int64
}

type stats struct {
	Stats      map[string]int64
	LastUpdate time.Time
}

type objectPropertyInfo struct {
	Name string
	Type string
}

type controller struct {
	Session  session
	Logger   *slog.Logger
	DeviceID string
	Config   ControllerConfig
	Now      func() time.Time
	Notifier notifier
}

type controllerState struct {
	startedAt           time.Time
	aboveThresholdSince time.Time
}

type guestStatsSample struct {
	AvailableMemoryBytes int64
	HasAvailableMemory   bool
	LastUpdate           time.Time
}

func (c *controller) Run(ctx context.Context) error {
	if c == nil || c.Session == nil {
		return nil
	}

	now := c.nowFunc()
	state := controllerState{startedAt: now()}
	qomPath, err := c.resolveQOMPath(ctx)
	if err != nil {
		return err
	}

	if err := c.Session.EnableBalloonStatsPolling(ctx, qomPath, statsPollSeconds(c.Config.PollInterval)); err != nil {
		return err
	}

	ticker := time.NewTicker(c.pollInterval())
	defer ticker.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		if err := c.tick(ctx, qomPath, &state, now); err != nil {
			failures++
			if failures >= maxConsecutiveFailures {
				return fmt.Errorf("%d consecutive failures: %w", failures, err)
			}
			c.Logger.Warn("balloon controller cycle failed; retrying", "err", err, "consecutive_failures", failures)
			continue
		}
		failures = 0
	}
}

func (c *controller) tick(ctx context.Context, qomPath string, state *controllerState, now func() time.Time) error {
	actual, stats, err := c.readSample(ctx, qomPath)
	if err != nil {
		return err
	}

	target, apply, err := evaluate(c.Config, state, now(), actual.ActualBytes, stats)
	if err != nil {
		return err
	}
	if !apply {
		return nil
	}

	if err := c.Session.SetBalloonLogicalSize(ctx, target); err != nil {
		return err
	}
	c.notifyResize(ctx, actual.ActualBytes, target)
	c.Logger.Info("balloon controller set guest memory", "target_mib", target/bytesPerMiB)
	return nil
}

func (c *controller) notifyResize(ctx context.Context, actualBytes int64, targetBytes int64) {
	if c.Notifier == nil {
		return
	}
	c.Notifier.Notify(ctx, "balloon:resize", balloonResizeMessage(actualBytes, targetBytes), map[string]string{
		"device_id":  c.DeviceID,
		"actual_mib": strconv.FormatInt(actualBytes/bytesPerMiB, 10),
		"target_mib": strconv.FormatInt(targetBytes/bytesPerMiB, 10),
		"delta_mib":  strconv.FormatInt((targetBytes-actualBytes)/bytesPerMiB, 10),
	})
}

func balloonResizeMessage(actualBytes int64, targetBytes int64) string {
	if targetBytes < actualBytes {
		return fmt.Sprintf("Reclaiming %s of memory", formatMemorySize(actualBytes-targetBytes))
	}
	return fmt.Sprintf("Growing guest memory to %s", formatMemorySize(targetBytes))
}

func formatMemorySize(bytes int64) string {
	mib := bytes / bytesPerMiB
	if mib%1024 == 0 {
		return fmt.Sprintf("%dGiB", mib/1024)
	}
	return fmt.Sprintf("%dMiB", mib)
}

func availableMemory(stats stats) (int64, bool) {
	if value, ok := stats.Stats["stat-available-memory"]; ok && value >= 0 {
		return value, true
	}
	if value, ok := stats.Stats["stat-free-memory"]; ok && value >= 0 {
		return value, true
	}
	return 0, false
}

func (c *controller) resolveQOMPath(ctx context.Context) (string, error) {
	expected := "/machine/peripheral/" + c.DeviceID
	if c.qomPathSupportsGuestStats(ctx, expected) {
		return expected, nil
	}

	candidates := []string{expected}
	for _, root := range []string{"/machine/peripheral", "/machine/peripheral-anon"} {
		props, err := c.Session.ListQOMProperties(ctx, root)
		if err != nil {
			continue
		}
		for _, prop := range props {
			if !strings.HasPrefix(prop.Type, "child<") {
				continue
			}
			candidate := root + "/" + prop.Name
			if prop.Name == c.DeviceID {
				candidates = append([]string{candidate}, candidates...)
				continue
			}
			candidates = append(candidates, candidate)
		}
	}

	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		if c.qomPathSupportsGuestStats(ctx, candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("%w for %q", errQOMPathNotFound, c.DeviceID)
}

func (c *controller) qomPathSupportsGuestStats(ctx context.Context, path string) bool {
	props, err := c.Session.ListQOMProperties(ctx, path)
	if err != nil {
		return false
	}
	return hasQOMProperty(props, "guest-stats") && hasQOMProperty(props, "guest-stats-polling-interval")
}

func (c *controller) readSample(ctx context.Context, qomPath string) (info, guestStatsSample, error) {
	stats, err := c.Session.ReadBalloonStats(ctx, qomPath)
	if err != nil {
		return info{}, guestStatsSample{}, err
	}

	actual, err := c.Session.QueryBalloon(ctx)
	if err != nil {
		return info{}, guestStatsSample{}, err
	}

	available, ok := availableMemory(stats)
	return actual, guestStatsSample{
		AvailableMemoryBytes: available,
		HasAvailableMemory:   ok,
		LastUpdate:           stats.LastUpdate,
	}, nil
}

func (c *controller) pollInterval() time.Duration {
	return c.Config.PollInterval
}

// statsPollSeconds converts the poll interval for QEMU's integer-second
// guest-stats-polling-interval property, keeping at least one second.
func statsPollSeconds(interval time.Duration) int {
	seconds := int(interval / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func (c *controller) nowFunc() func() time.Time {
	if c.Now != nil {
		return c.Now
	}
	return time.Now
}

func evaluate(
	config ControllerConfig,
	state *controllerState,
	now time.Time,
	actualBytes int64,
	stats guestStatsSample,
) (int64, bool, error) {
	staleAfter := 2 * config.PollInterval

	if stats.LastUpdate.IsZero() {
		if now.Sub(state.startedAt) >= staleAfter {
			return 0, false, errGuestStatsUnavailable
		}
		return 0, false, nil
	}

	if now.Sub(stats.LastUpdate) > staleAfter {
		return 0, false, errGuestStatsStale
	}

	if !stats.HasAvailableMemory {
		if now.Sub(state.startedAt) >= staleAfter {
			return 0, false, errGuestStatsUnavailable
		}
		return 0, false, nil
	}

	minActualBytes := config.MinActual.Bytes().Int64()
	maxActualBytes := config.MaxActual.Bytes().Int64()
	stepBytes := config.Step.Bytes().Int64()
	growBelowBytes := config.GrowBelowAvailable.Bytes().Int64()
	reclaimAboveBytes := config.ReclaimAboveAvailable.Bytes().Int64()

	if stats.AvailableMemoryBytes < growBelowBytes {
		state.aboveThresholdSince = time.Time{}
		target := actualBytes + stepBytes
		if target > maxActualBytes {
			target = maxActualBytes
		}
		if target <= actualBytes {
			return 0, false, nil
		}
		return target, true, nil
	}

	if stats.AvailableMemoryBytes > reclaimAboveBytes {
		if actualBytes <= minActualBytes {
			state.aboveThresholdSince = time.Time{}
			return 0, false, nil
		}
		if state.aboveThresholdSince.IsZero() {
			state.aboveThresholdSince = now
			return 0, false, nil
		}
		if now.Sub(state.aboveThresholdSince) < config.ReclaimHoldoff {
			return 0, false, nil
		}

		state.aboveThresholdSince = time.Time{}
		target := actualBytes - stepBytes
		if target < minActualBytes {
			target = minActualBytes
		}
		if target >= actualBytes {
			return 0, false, nil
		}
		return target, true, nil
	}

	state.aboveThresholdSince = time.Time{}
	return 0, false, nil
}

func hasQOMProperty(props []objectPropertyInfo, name string) bool {
	for _, prop := range props {
		if prop.Name == name {
			return true
		}
	}
	return false
}
