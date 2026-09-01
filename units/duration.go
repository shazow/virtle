package units

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// Duration is a time.Duration that decodes from a Go duration string such as
// "30s" or "5m". Bare numbers, quoted or not, decode as seconds; that form is
// undocumented and kept for backward compatibility.
type Duration time.Duration

// ParseDuration parses a Go duration string, treating a bare number as
// seconds.
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

// secondsDuration converts seconds to a Duration. Positive infinity and
// overflow saturate to the maximum duration (effectively no timeout); NaN and
// negative infinity are rejected because their integer conversion is
// platform-defined.
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

// MarshalText encodes the duration in Go's duration-string form so text-based
// encoders such as TOML emit values the decoder reads back unchanged.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// UnmarshalText decodes the duration grammar accepted by ParseDuration: a Go
// duration string or a bare number of seconds.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = parsed
	return nil
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

// UnmarshalTOML decodes TOML integers and floats as seconds and strings as Go
// durations.
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
