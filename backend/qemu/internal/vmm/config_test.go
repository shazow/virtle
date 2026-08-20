package vmm

import (
	"bytes"
	"log/slog"
	"strings"
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

func TestManagerDerivesPackageLoggersFromRoot(t *testing.T) {
	var output bytes.Buffer
	manager := newManagerFromConfig(Config{
		Logger: slog.New(slog.NewTextHandler(&output, nil)),
	})

	manager.logger.Info("vmm lifecycle")
	manager.sshLifecycleLogger().Info("ssh lifecycle")
	logs := output.String()
	for _, want := range []string{
		`msg="vmm lifecycle" package=vmm`,
		`msg="ssh lifecycle" package=ssh`,
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("expected %q in logs:\n%s", want, logs)
		}
	}
}
