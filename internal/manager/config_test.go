package manager

import (
	"log/slog"
	"testing"
	"time"
)

func TestMergeConfigOverridesSetFieldsOnly(t *testing.T) {
	base := Config{
		Logger:            slog.Default(),
		SSHRetryDelay:     time.Second,
		QMPConnectTimeout: time.Second,
	}
	override := Config{
		SSHRetryDelay: time.Minute,
	}

	got := mergeConfig(base, override)
	if got.Logger != base.Logger {
		t.Fatal("expected nil logger override to preserve base logger")
	}
	if got.SSHRetryDelay != time.Minute {
		t.Fatalf("unexpected ssh retry delay: got %s want %s", got.SSHRetryDelay, time.Minute)
	}
	if got.QMPConnectTimeout != time.Second {
		t.Fatalf("unexpected qmp connect timeout: got %s want %s", got.QMPConnectTimeout, time.Second)
	}
}
