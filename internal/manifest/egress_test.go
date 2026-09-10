package manifest

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const egressBase = "[kernel]\npath = 'k'\ninitrd_path = 'i'\n\n[[networks]]\ntype = 'virtle'\n"

func decodeEgress(t *testing.T, body string) *Manifest {
	t.Helper()
	doc, err := DecodeDocumentBytes([]byte(body), "")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := doc.ResolveWorkingDir(); err != nil {
		t.Fatal(err)
	}
	m, err := doc.Manifest()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return m
}

func TestResolveEgress(t *testing.T) {
	m := decodeEgress(t, egressBase+`
[egress]
[[egress.allow]]
host = "api.github.com"
ports = [443]
inspect = true

[[egress.allow]]
host = "10.0.0.0/8"

[[egress.deny]]
host = "evil.github.com"

[[egress.secrets]]
name = "GITHUB_TOKEN"
from = "env:GH_TOKEN"
hosts = ["api.github.com"]
methods = ["GET", "POST"]
paths = ["/repos/*"]
in = ["header"]

[[egress.secrets]]
name = "NPM_TOKEN"
from = "file:npm.token"
hosts = ["*.npmjs.org"]
`)
	e := m.Egress
	if e == nil {
		t.Fatal("no egress resolved")
	}
	wantAllow := []EgressRule{{Host: "api.github.com", Ports: []int{443}, Inspect: true}, {Host: "10.0.0.0/8"}}
	if !reflect.DeepEqual(e.Allow, wantAllow) || !reflect.DeepEqual(e.Deny, []EgressRule{{Host: "evil.github.com"}}) || !e.Inspects {
		t.Fatalf("rules = %+v / %+v (inspects %v)", e.Allow, e.Deny, e.Inspects)
	}
	if len(e.Secrets) != 2 || e.Secrets[0].From != "env:GH_TOKEN" || e.Secrets[0].Methods[1] != "POST" || e.Secrets[0].In[0] != "header" {
		t.Fatalf("secrets = %+v", e.Secrets)
	}
	if want := "file:" + filepath.Join(m.Paths.WorkingDir, "npm.token"); e.Secrets[1].From != want {
		t.Fatalf("file source = %q, want %q", e.Secrets[1].From, want)
	}
	if want := filepath.Join(m.ResolvedPersistenceStateDir(), "egress-ca"); e.CADir != want {
		t.Fatalf("CADir = %q, want %q", e.CADir, want)
	}

	t.Setenv("GH_TOKEN", "ghp_secret")
	if v, err := e.Secrets[0].ValueFunc()(); err != nil || v != "ghp_secret" {
		t.Fatalf("env value = %q, %v", v, err)
	}
	os.Unsetenv("GH_TOKEN")
	if _, err := e.Secrets[0].ValueFunc()(); err == nil {
		t.Fatal("an unset variable read as a value")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "npm.token"), []byte("npm_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file := EgressSecret{Name: "NPM_TOKEN", From: "file:" + filepath.Join(dir, "npm.token")}
	if v, err := file.ValueFunc()(); err != nil || v != "npm_secret" {
		t.Fatalf("file value = %q, %v", v, err)
	}

	custom := decodeEgress(t, egressBase+"[egress]\nca_dir = 'ca'\n[[egress.allow]]\nhost = '*.test'\n")
	if custom.Egress.CADir != filepath.Join(custom.Paths.WorkingDir, "ca") || custom.Egress.Inspects {
		t.Fatalf("egress = %+v", custom.Egress)
	}
	if plain := decodeEgress(t, egressBase); plain.Egress != nil {
		t.Fatal("egress resolved without a section")
	}
}

func TestResolveEgressRejects(t *testing.T) {
	const inspecting = "[egress]\n[[egress.allow]]\nhost = 'api.test'\ninspect = true\n"
	for name, tc := range map[string]struct{ body, want string }{
		"without virtle network": {"[kernel]\npath = 'k'\ninitrd_path = 'i'\n[egress]\n[[egress.allow]]\nhost = 'a.test'\n", "network of type virtle"},
		"bad pattern":            {egressBase + "[egress]\n[[egress.allow]]\nhost = '['\n", "allow[0].host"},
		"bad port":               {egressBase + "[egress]\n[[egress.allow]]\nhost = 'a.test'\nports = [70000]\n", "not a port"},
		"bad deny":               {egressBase + "[egress]\n[[egress.deny]]\nhost = 'a b'\n", "deny[0].host"},
		"secret name":            {egressBase + inspecting + "[[egress.secrets]]\nname = '1x'\nfrom = 'env:A'\nhosts = ['api.test']\n", "environment variable name"},
		"secret source":          {egressBase + inspecting + "[[egress.secrets]]\nname = 'A'\nfrom = 'vault:A'\nhosts = ['api.test']\n", "unknown source"},
		"secret source shape":    {egressBase + inspecting + "[[egress.secrets]]\nname = 'A'\nfrom = 'A'\nhosts = ['api.test']\n", "env:NAME or file:PATH"},
		"secret env name":        {egressBase + inspecting + "[[egress.secrets]]\nname = 'A'\nfrom = 'env:a-b'\nhosts = ['api.test']\n", "not an environment variable name"},
		"secret hosts":           {egressBase + inspecting + "[[egress.secrets]]\nname = 'A'\nfrom = 'env:A'\n", "hosts is required"},
		"secret placement":       {egressBase + inspecting + "[[egress.secrets]]\nname = 'A'\nfrom = 'env:A'\nhosts = ['api.test']\nin = ['cookie']\n", "must be one of"},
		"secret without inspect": {egressBase + "[egress]\n[[egress.allow]]\nhost = 'api.test'\n[[egress.secrets]]\nname = 'A'\nfrom = 'env:A'\nhosts = ['api.test']\n", "inspect = true"},
		"duplicate secret":       {egressBase + inspecting + "[[egress.secrets]]\nname = 'A'\nfrom = 'env:A'\nhosts = ['api.test']\n[[egress.secrets]]\nname = 'A'\nfrom = 'env:B'\nhosts = ['api.test']\n", "twice"},
		"firecracker":            {"backend = 'firecracker'\n[kernel]\npath = 'k'\n[egress]\n[[egress.allow]]\nhost = 'a.test'\n", "firecracker"},
	} {
		t.Run(name, func(t *testing.T) {
			doc, err := DecodeDocumentBytes([]byte(tc.body), "")
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			_, err = doc.Manifest()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
