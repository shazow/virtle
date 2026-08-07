package units

import (
	"encoding/json"
	"fmt"
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
		return Duration(seconds * float64(time.Second)), nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: expected a duration such as %q", value, "30s")
	}
	return Duration(parsed), nil
}

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var seconds float64
	if err := json.Unmarshal(data, &seconds); err == nil {
		*d = Duration(seconds * float64(time.Second))
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
		*d = Duration(time.Duration(v) * time.Second)
	case float64:
		*d = Duration(v * float64(time.Second))
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
