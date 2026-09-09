package firecracker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAPIRequest(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"success", 204, "", ""},
		{"fault", 400, `{"fault_message":"invalid kernel"}`, "invalid kernel"},
		{"malformed", 500, "\x1b[31muntrusted", `\x1b`},
		{"bounded", 400, strings.Repeat("x", 65537), "exceeds"},
		{"unexpected success", 200, "{}", "HTTP 200"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &apiClient{http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != "PUT" || r.URL.Path != "/actions" || r.Header.Get("Content-Type") != "application/json" {
					t.Fatalf("request: %v", r)
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"action_type":"InstanceStart"}` {
					t.Fatalf("body: %s", body)
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})}}
			err := client.put(t.Context(), "/actions", action{Type: "InstanceStart"})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if len(err.Error()) > 2048 || strings.ContainsRune(err.Error(), '\x1b') {
				t.Fatal("unbounded or unsafe error")
			}
		})
	}
}

func TestAPIContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := &apiClient{http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })}}
	if err := client.put(ctx, "/actions", action{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
