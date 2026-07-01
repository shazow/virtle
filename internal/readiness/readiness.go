// Package readiness contains generic token-based readiness helpers.
package readiness

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const tokenLimit = 64

// ReadToken reads a readiness token from reader and requires it to match
// expected exactly after surrounding whitespace is trimmed.
func ReadToken(reader io.Reader, expected string) error {
	var data bytes.Buffer
	buf := make([]byte, 32)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			data.Write(buf[:n])
			token := strings.TrimSpace(data.String())
			if token == expected {
				return nil
			}
			if token != "" && !strings.HasPrefix(expected, token) {
				return fmt.Errorf("unexpected readiness token %q", TruncateToken(token))
			}
			if data.Len() > tokenLimit && !strings.HasPrefix(expected, token) {
				return fmt.Errorf("unexpected readiness token %q", TruncateToken(token))
			}
		}
		if err == nil {
			continue
		}
		if err != io.EOF {
			return fmt.Errorf("read readiness token: %w", err)
		}
		token := strings.TrimSpace(data.String())
		return fmt.Errorf("unexpected readiness token %q", TruncateToken(token))
	}
}

// TruncateToken shortens token text for diagnostic messages.
func TruncateToken(token string) string {
	if len(token) <= tokenLimit {
		return token
	}
	return token[:tokenLimit] + "..."
}

// TimeoutFromEnv parses envName as a positive duration, falling back otherwise.
func TimeoutFromEnv(envName string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		return fallback
	}

	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout <= 0 {
		return fallback
	}
	return timeout
}
