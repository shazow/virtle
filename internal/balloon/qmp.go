package balloon

import (
	"encoding/json"
	"fmt"
	"time"

	rawQMP "github.com/digitalocean/go-qemu/qmp/raw"
)

type MonitorSession interface {
	WithRaw(timeout time.Duration, fn func(*rawQMP.Monitor) error) error
}

type qmpSession struct {
	session MonitorSession
}

func newQMPSession(session MonitorSession) session {
	return &qmpSession{session: session}
}

func (s *qmpSession) QueryBalloon(timeout time.Duration) (info, error) {
	var balloonInfo rawQMP.BalloonInfo
	err := s.session.WithRaw(timeout, func(monitor *rawQMP.Monitor) error {
		var err error
		balloonInfo, err = monitor.QueryBalloon()
		if err != nil {
			return fmt.Errorf("qmp query-balloon: %w", err)
		}
		return nil
	})
	if err != nil {
		return info{}, err
	}
	return info{ActualBytes: balloonInfo.Actual}, nil
}

func (s *qmpSession) SetBalloonLogicalSize(timeout time.Duration, logicalSizeBytes int64) error {
	return s.session.WithRaw(timeout, func(monitor *rawQMP.Monitor) error {
		if err := monitor.Balloon(logicalSizeBytes); err != nil {
			return fmt.Errorf("qmp balloon: %w", err)
		}
		return nil
	})
}

func (s *qmpSession) EnableBalloonStatsPolling(timeout time.Duration, qomPath string, pollIntervalSeconds int) error {
	return s.session.WithRaw(timeout, func(monitor *rawQMP.Monitor) error {
		if err := monitor.QomSet(qomPath, "guest-stats-polling-interval", int64(pollIntervalSeconds)); err != nil {
			return fmt.Errorf("qmp qom-set guest-stats-polling-interval: %w", err)
		}
		return nil
	})
}

func (s *qmpSession) ReadBalloonStats(timeout time.Duration, qomPath string) (stats, error) {
	var value interface{}
	err := s.session.WithRaw(timeout, func(monitor *rawQMP.Monitor) error {
		var err error
		value, err = monitor.QomGet(qomPath, "guest-stats")
		if err != nil {
			return fmt.Errorf("qmp qom-get guest-stats: %w", err)
		}
		return nil
	})
	if err != nil {
		return stats{}, err
	}

	type guestStats struct {
		Stats      map[string]int64 `json:"stats"`
		LastUpdate int64            `json:"last-update"`
	}

	payload, err := json.Marshal(value)
	if err != nil {
		return stats{}, fmt.Errorf("encode qmp guest-stats: %w", err)
	}

	var decoded guestStats
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return stats{}, fmt.Errorf("decode qmp guest-stats: %w", err)
	}

	snapshot := make(map[string]int64, len(decoded.Stats))
	for key, value := range decoded.Stats {
		snapshot[key] = value
	}

	var lastUpdate time.Time
	if decoded.LastUpdate > 0 {
		lastUpdate = time.Unix(decoded.LastUpdate, 0)
	}

	return stats{
		Stats:      snapshot,
		LastUpdate: lastUpdate,
	}, nil
}

func (s *qmpSession) ListQOMProperties(timeout time.Duration, path string) ([]objectPropertyInfo, error) {
	var props []rawQMP.ObjectPropertyInfo
	err := s.session.WithRaw(timeout, func(monitor *rawQMP.Monitor) error {
		var err error
		props, err = monitor.QomList(path)
		if err != nil {
			return fmt.Errorf("qmp qom-list: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	result := make([]objectPropertyInfo, 0, len(props))
	for _, prop := range props {
		result = append(result, objectPropertyInfo{
			Name: prop.Name,
			Type: prop.Type,
		})
	}
	return result, nil
}
