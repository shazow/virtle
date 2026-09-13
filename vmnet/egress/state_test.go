package egress

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/shazow/virtle/vm"
)

func TestTokenCheckpointRestoresIssuedPlaceholders(t *testing.T) {
	newPolicy := func() *Policy {
		return &Policy{Injections: []Injection{
			{Name: "TOKEN", Hosts: []string{"api.test"}, Value: constValue("host-secret-value")},
			{Name: "FIXED", Token: "fixed-placeholder", Hosts: []string{"api.test"}, Value: constValue("fixed-secret-value")},
		}}
	}
	first := newPolicy()
	want := first.GuestEnv(nil)
	data, err := json.Marshal(first.SaveTokens())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-value") {
		t.Fatal("checkpoint contains a secret value")
	}
	var tokens map[string]string
	if err := json.Unmarshal(data, &tokens); err != nil {
		t.Fatal(err)
	}
	next := newPolicy()
	if slices.Equal(next.GuestEnv(nil), want) {
		t.Fatal("independent policy did not issue a different token")
	}
	if err := next.RestoreTokens(tokens, true); err != nil {
		t.Fatal(err)
	}
	if got := next.GuestEnv(nil); !slices.Equal(got, want) {
		t.Fatalf("restored guest environment = %v, want %v", got, want)
	}
	if got := next.GuestEnv(&vm.Egress{Secrets: []string{"FIXED"}}); !slices.Equal(got, []string{"FIXED=fixed-placeholder"}) {
		t.Fatalf("guest permissions changed after restore: %v", got)
	}
	// Neither direction shares mutable maps with its caller.
	tokens["TOKEN"] = "changed"
	saved := next.SaveTokens()
	saved["TOKEN"] = "changed again"
	if got := next.GuestEnv(nil); !slices.Equal(got, want) {
		t.Fatalf("caller changed restored tokens: %v", got)
	}
}

func TestRestoreTokensValidatesBeforeChangingPolicy(t *testing.T) {
	for name, invalid := range map[string]map[string]string{
		"unknown injection": {"TOKEN": "restored", "MISSING": "value"},
		"empty token":       {"TOKEN": ""},
		"fixed changed":     {"TOKEN": "restored", "FIXED": "different"},
	} {
		t.Run(name, func(t *testing.T) {
			p := &Policy{Injections: []Injection{
				{Name: "TOKEN", Hosts: []string{"api.test"}, Value: constValue("secret")},
				{Name: "FIXED", Token: "fixed", Hosts: []string{"api.test"}, Value: constValue("secret")},
			}}
			p.GuestEnv(nil)
			before := p.SaveTokens()
			if err := p.RestoreTokens(invalid, true); err == nil {
				t.Fatal("invalid token checkpoint accepted")
			}
			if got := p.SaveTokens(); !maps.Equal(got, before) {
				t.Fatal("failed restore changed issued tokens")
			}
		})
	}
}

func TestRestoreTokensMergesUnissuedTokens(t *testing.T) {
	p := &Policy{Injections: []Injection{
		{Name: "ACTIVE", Hosts: []string{"api.test"}, Value: constValue("secret")},
		{Name: "RESUMED", Hosts: []string{"api.test"}, Value: constValue("secret")},
	}}
	active := p.GuestEnv(&vm.Egress{Secrets: []string{"ACTIVE"}})
	if err := p.RestoreTokens(map[string]string{"RESUMED": "saved-placeholder"}, false); err != nil {
		t.Fatal(err)
	}
	if got := p.GuestEnv(&vm.Egress{Secrets: []string{"ACTIVE"}}); !slices.Equal(got, active) {
		t.Fatal("merging changed an active guest's issued token")
	}
	if got := p.GuestEnv(&vm.Egress{Secrets: []string{"RESUMED"}}); !slices.Equal(got, []string{"RESUMED=saved-placeholder"}) {
		t.Fatalf("resumed token = %v", got)
	}
	before := p.SaveTokens()
	if err := p.RestoreTokens(map[string]string{"RESUMED": "different"}, false); err == nil {
		t.Fatal("merge replaced an already issued token")
	}
	if !maps.Equal(p.SaveTokens(), before) {
		t.Fatal("failed merge changed issued tokens")
	}
}
