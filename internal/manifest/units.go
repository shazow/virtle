package manifest

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

const bytesPerMiB int64 = 1024 * 1024

// MiB is the manifest format's mebibyte-denominated size.
type MiB int

func (m MiB) Bytes() int64 { return int64(m) * bytesPerMiB }

func (m MiB) Int() int { return int(m) }

// Duration is the manifest format's duration encoding. Resolved runtime and
// public API types use time.Duration directly.
type Duration time.Duration

func ParseDuration(value string) (Duration, error) {
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		return secondsDuration(seconds)
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: expected a duration such as %q", value, "30s")
	}
	return Duration(parsed), nil
}

func secondsDuration(seconds float64) (Duration, error) {
	if math.IsNaN(seconds) || math.IsInf(seconds, -1) {
		return 0, fmt.Errorf("invalid duration %v: must not be NaN or negative infinity", seconds)
	}
	nanos := seconds * float64(time.Second)
	if nanos >= float64(math.MaxInt64) {
		return Duration(math.MaxInt64), nil
	}
	return Duration(nanos), nil
}

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var seconds float64
	if err := json.Unmarshal(data, &seconds); err == nil {
		parsed, err := secondsDuration(seconds)
		if err != nil {
			return err
		}
		*d = parsed
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("duration must be a string such as %q", "30s")
	}
	parsed, err := ParseDuration(value)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

func (d *Duration) UnmarshalTOML(value any) error {
	switch v := value.(type) {
	case int64:
		parsed, err := secondsDuration(float64(v))
		if err != nil {
			return err
		}
		*d = parsed
	case float64:
		parsed, err := secondsDuration(v)
		if err != nil {
			return err
		}
		*d = parsed
	case string:
		parsed, err := ParseDuration(v)
		if err != nil {
			return err
		}
		*d = parsed
	default:
		return fmt.Errorf("duration must be a string such as %q, got %T", "30s", value)
	}
	return nil
}
