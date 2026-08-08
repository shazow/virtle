package manager

import (
	"log/slog"
	"testing"
	"time"
)

func TestMergeConfigOverridesSetFieldsOnly(t *testing.T) {
	base := Config{
		Logger:            slog.Default(),
		QMPConnectTimeout: time.Second,
	}
	override := Config{
		QMPQuitTimeout: time.Minute,
	}

	got := mergeConfig(base, override)
	if got.Logger != base.Logger {
		t.Fatal("expected nil logger override to preserve base logger")
	}
	if got.QMPQuitTimeout != time.Minute {
		t.Fatalf("unexpected qmp quit timeout: got %s want %s", got.QMPQuitTimeout, time.Minute)
	}
	if got.QMPConnectTimeout != time.Second {
		t.Fatalf("unexpected qmp connect timeout: got %s want %s", got.QMPConnectTimeout, time.Second)
	}
}
