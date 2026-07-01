package readiness

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type chunkReader struct {
	chunks []string
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	copy(p, chunk)
	return len(chunk), nil
}

func TestReadToken(t *testing.T) {
	tests := []struct {
		name    string
		reader  io.Reader
		wantErr string
	}{
		{name: "exact", reader: strings.NewReader("SSH-READY\n")},
		{name: "partial", reader: &chunkReader{chunks: []string{"SSH-", "READY\n"}}},
		{name: "eof", reader: strings.NewReader("SSH-"), wantErr: `unexpected readiness token "SSH-"`},
		{name: "invalid", reader: strings.NewReader("NOT_READY\n"), wantErr: `unexpected readiness token "NOT_READY"`},
		{name: "long", reader: strings.NewReader(strings.Repeat("x", 80)), wantErr: strings.Repeat("x", 32)},
		{name: "read error", reader: errorReader{}, wantErr: "read readiness token: boom"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ReadToken(tt.reader, "SSH-READY")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ReadToken: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("boom")
}

func TestTimeoutFromEnv(t *testing.T) {
	t.Setenv("VIRTIE_TEST_TIMEOUT", "5m")
	if got, want := TimeoutFromEnv("VIRTIE_TEST_TIMEOUT", time.Second), 5*time.Minute; got != want {
		t.Fatalf("unexpected parsed timeout: got %s want %s", got, want)
	}

	t.Setenv("VIRTIE_TEST_TIMEOUT", "0")
	if got, want := TimeoutFromEnv("VIRTIE_TEST_TIMEOUT", time.Second), time.Second; got != want {
		t.Fatalf("unexpected fallback timeout: got %s want %s", got, want)
	}

	t.Setenv("VIRTIE_TEST_TIMEOUT", "bad")
	if got, want := TimeoutFromEnv("VIRTIE_TEST_TIMEOUT", time.Second), time.Second; got != want {
		t.Fatalf("unexpected invalid fallback timeout: got %s want %s", got, want)
	}
}
