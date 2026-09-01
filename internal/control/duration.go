package control

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// Duration is the control protocol's duration encoding. Bare numbers are
// retained as a backward-compatible seconds form.
type Duration time.Duration

func parseDuration(value string) (Duration, error) {
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		return controlSecondsDuration(seconds)
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: expected a duration such as %q", value, "30s")
	}
	return Duration(parsed), nil
}

func controlSecondsDuration(seconds float64) (Duration, error) {
	if math.IsNaN(seconds) || math.IsInf(seconds, -1) {
		return 0, fmt.Errorf("invalid duration %v: must not be NaN or negative infinity", seconds)
	}
	nanos := seconds * float64(time.Second)
	if nanos >= float64(math.MaxInt64) {
		return Duration(math.MaxInt64), nil
	}
	return Duration(nanos), nil
}

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var seconds float64
	if err := json.Unmarshal(data, &seconds); err == nil {
		parsed, err := controlSecondsDuration(seconds)
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
	parsed, err := parseDuration(value)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}
