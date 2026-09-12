package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CA persistence and locking require real files and processes: an in-memory
// filesystem cannot exercise cross-process publication or kernel locks.
func TestCAConcurrentProcessesShareIdentity(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	type initializer struct {
		cmd    *exec.Cmd
		start  io.WriteCloser
		output *bufio.Reader
		stderr bytes.Buffer
	}
	var children []*initializer
	for range 8 {
		child := &initializer{cmd: exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCAInitializerProcess$")}
		child.cmd.Env = append(os.Environ(), "VIRTLE_TEST_CA_DIR="+dir)
		child.cmd.Stderr = &child.stderr
		stdin, err := child.cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := child.cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		child.start, child.output = stdin, bufio.NewReader(stdout)
		if err := child.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = child.cmd.Process.Kill(); _ = child.cmd.Wait() })
		if line, err := child.output.ReadString('\n'); err != nil || line != "ready\n" {
			t.Fatalf("initializer ready: %q, %v, stderr %s", line, err, &child.stderr)
		}
		children = append(children, child)
	}
	// Every process has started before any is allowed to initialize the CA.
	for _, child := range children {
		if _, err := io.WriteString(child.start, "go\n"); err != nil {
			t.Fatal(err)
		}
		_ = child.start.Close()
	}
	var identity string
	for _, child := range children {
		line, err := child.output.ReadString('\n')
		if err != nil {
			t.Fatalf("initializer result: %v, stderr %s", err, &child.stderr)
		}
		if err := child.cmd.Wait(); err != nil {
			t.Fatalf("initializer: %v, stderr %s", err, &child.stderr)
		}
		if identity == "" {
			identity = line
		}
		if line != identity {
			t.Fatalf("initializers used different CAs: %s and %s", identity, line)
		}
	}
	loaded, err := tls.LoadX509KeyPair(filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x\n", sha256.Sum256(loaded.Certificate[0])); got != identity {
		t.Fatalf("published CA identity %s, initializer identity %s", got, identity)
	}
}

func TestCAInitializerProcess(t *testing.T) {
	dir := os.Getenv("VIRTLE_TEST_CA_DIR")
	if dir == "" {
		return
	}
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cert, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("%x\n", sha256.Sum256(cert.Certificate[0]))
}

func TestCACompletesInterruptedPublication(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The private bundle was published, but the public copy was not.
	if err := os.Remove(filepath.Join(dir, caCertFile)); err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Certificate[0], second.Certificate[0]) {
		t.Fatal("publication recovery changed the CA")
	}
	loaded, err := tls.LoadX509KeyPair(filepath.Join(dir, caCertFile), filepath.Join(dir, caKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Certificate[0], loaded.Certificate[0]) {
		t.Fatal("public copy does not match the CA bundle")
	}
}

func TestCALoadsSeparatePEMFiles(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, caKeyFile)
	bundle, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := pem.Decode(bundle)
	if key == nil || !strings.Contains(key.Type, "PRIVATE KEY") {
		t.Fatal("missing private key")
	}
	keyPEM := pem.EncodeToMemory(key)
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Certificate[0], second.Certificate[0]) {
		t.Fatal("loading separate PEM files changed the CA")
	}
	if err := os.Remove(filepath.Join(dir, caCertFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCA(dir); err == nil {
		t.Fatal("accepted a private key without its certificate")
	}
	retained, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(retained, keyPEM) {
		t.Fatalf("incomplete CA key was replaced: %v", err)
	}
}
