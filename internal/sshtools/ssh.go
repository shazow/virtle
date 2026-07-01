// Package sshtools contains reusable SSH command helpers.
package sshtools

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	shellquote "github.com/kballard/go-shellquote"
	"golang.org/x/crypto/ssh"
)

const (
	AutoprovisionIdentityName = "id_ed25519"
	retryOutputLimit          = 64 * 1024
)

type Config struct {
	Exec []string
	User string
}

type Command struct {
	Path string
	Args []string
}

func NewCommand(cfg Config, cid int, remoteCommand []string) (Command, error) {
	if len(cfg.Exec) == 0 {
		return Command{}, errors.New("ssh command is empty")
	}
	argv := append([]string(nil), cfg.Exec...)
	command := Command{
		Path: argv[0],
		Args: append([]string{"-tt"}, argv[1:]...),
	}
	command.Args = append(command.Args, cfg.Destination(cid))
	if len(remoteCommand) > 0 {
		command.Args = append(command.Args, encodeRemoteCommand(remoteCommand))
	}
	return command, nil
}

func BuildArgs(cfg Config, cid int, remoteCommand []string) (path string, args []string) {
	command, err := NewCommand(cfg, cid, remoteCommand)
	if err != nil {
		return "", nil
	}
	return command.Path, command.Args
}

func CommandHint(cfg Config, cid int) string {
	return cfg.Hint(cid)
}

func (cfg Config) WithIdentity(identityFile string) Config {
	return Config{
		Exec: AddIdentity(cfg.Exec, identityFile),
		User: cfg.User,
	}
}

func WithIdentity(argv []string, identityFile string) []string {
	return AddIdentity(argv, identityFile)
}

func AddIdentity(argv []string, identityFile string) []string {
	withIdentity := append([]string(nil), argv...)
	return append(withIdentity, "-i", identityFile, "-o", "IdentitiesOnly=yes")
}

func (cfg Config) Destination(cid int) string {
	return VSockDestination(cfg.User, cid)
}

func VSockDestination(user string, cid int) string {
	return fmt.Sprintf("%s@vsock/%d", user, cid)
}

func (cfg Config) Hint(cid int) string {
	if len(cfg.Exec) == 0 {
		return ""
	}
	args := append([]string(nil), cfg.Exec...)
	args = append(args, cfg.Destination(cid))
	return shellDisplayJoin(args...)
}

func (c Command) Argv() []string {
	if c.Path == "" {
		return append([]string(nil), c.Args...)
	}
	argv := []string{c.Path}
	return append(argv, c.Args...)
}

func (c Command) String() string {
	return shellDisplayJoin(c.Argv()...)
}

func shellDisplayJoin(args ...string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellDisplayQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellDisplayQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.ContainsAny(arg, " \t\n'\"\\$&|;()<>*?[#~=%!") {
		return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
	}
	return arg
}

type Failure int

const (
	FailureNone Failure = iota
	FailureTransient
	FailureAuthentication
)

type RetryPhase int

const (
	RetryPhaseNone RetryPhase = iota
	RetryPhaseWaiting
	RetryPhaseConnecting
)

func ClassifyFailure(err error, stderr string) Failure {
	message := lowerFailureMessage(err, stderr)
	if message == "" {
		return FailureNone
	}
	for _, authMessage := range []string{
		"permission denied (publickey",
		"permission denied, please try again",
		"no more authentication methods to try",
		"publickey,password",
	} {
		if strings.Contains(message, authMessage) {
			return FailureAuthentication
		}
	}
	if RetryPhaseForFailure(err, stderr) != RetryPhaseNone {
		return FailureTransient
	}
	return FailureNone
}

func RetryPhaseForFailure(err error, stderr string) RetryPhase {
	message := lowerFailureMessage(err, stderr)
	for _, transient := range []string{
		"connection reset",
		"connection closed",
	} {
		if strings.Contains(message, transient) {
			return RetryPhaseConnecting
		}
	}
	for _, transient := range []string{
		"connection refused",
		"connection timed out",
		"no route to host",
		"network is unreachable",
	} {
		if strings.Contains(message, transient) {
			return RetryPhaseWaiting
		}
	}
	return RetryPhaseNone
}

func lowerFailureMessage(err error, stderr string) string {
	if err != nil {
		return strings.ToLower(err.Error() + "\n" + stderr)
	}
	return strings.ToLower(stderr)
}

type RetryOutput struct {
	mu          sync.Mutex
	output      io.Writer
	verbose     bool
	captured    cappedBuffer
	pending     cappedBuffer
	timer       *time.Timer
	revealed    bool
	suppressed  bool
	revealDelay time.Duration
}

func NewRetryOutput(writer io.Writer, verbose bool, revealDelay time.Duration) *RetryOutput {
	return &RetryOutput{
		output:      writer,
		verbose:     verbose,
		captured:    cappedBuffer{limit: retryOutputLimit},
		pending:     cappedBuffer{limit: retryOutputLimit},
		revealDelay: revealDelay,
	}
}

func (o *RetryOutput) Write(p []byte) (int, error) {
	if o == nil {
		return len(p), nil
	}

	o.mu.Lock()
	_, _ = o.captured.Write(p)
	if o.verbose || o.revealed {
		output := o.output
		o.mu.Unlock()
		if output != nil {
			_, _ = output.Write(p)
		}
		return len(p), nil
	}
	if !o.suppressed {
		_, _ = o.pending.Write(p)
		if o.timer == nil && o.revealDelay > 0 {
			o.timer = time.AfterFunc(o.revealDelay, o.Flush)
		}
	}
	o.mu.Unlock()
	return len(p), nil
}

func (o *RetryOutput) String() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.captured.String()
}

func (o *RetryOutput) Suppress() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.timer != nil {
		o.timer.Stop()
		o.timer = nil
	}
	o.pending.Reset()
	o.suppressed = true
}

func (o *RetryOutput) Flush() {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.suppressed && !o.verbose {
		o.mu.Unlock()
		return
	}
	if o.timer != nil {
		o.timer.Stop()
		o.timer = nil
	}
	o.revealed = true
	output := o.output
	pending := append([]byte(nil), o.pending.Bytes()...)
	o.pending.Reset()
	o.mu.Unlock()

	if output != nil && len(pending) > 0 {
		_, _ = output.Write(pending)
	}
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.limit <= b.Len() {
		return n, nil
	}
	remaining := b.limit - b.Len()
	if len(p) > remaining {
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

type Key struct {
	IdentityFile  string
	PublicKeyFile string
	AuthorizedKey string
}

type KeyStore struct {
	Dir     string
	Comment string
}

func (s KeyStore) Ensure() (Key, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return Key{}, fmt.Errorf("create ssh key directory %q: %w", s.Dir, err)
	}

	identityFile := filepath.Join(s.Dir, AutoprovisionIdentityName)
	publicKeyFile := identityFile + ".pub"
	if _, err := os.Stat(identityFile); err == nil {
		if chmodErr := os.Chmod(identityFile, 0o600); chmodErr != nil {
			return Key{}, fmt.Errorf("chmod ssh identity %q: %w", identityFile, chmodErr)
		}
		return ensurePublicKeyForExistingIdentity(identityFile, publicKeyFile)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Key{}, fmt.Errorf("stat ssh identity %q: %w", identityFile, err)
	}

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Key{}, fmt.Errorf("generate ssh identity: %w", err)
	}

	comment := s.Comment
	if comment == "" {
		comment = "virtie-autoprovision"
	}
	block, err := ssh.MarshalPrivateKey(privateKey, comment)
	if err != nil {
		return Key{}, fmt.Errorf("encode ssh identity: %w", err)
	}
	if err := writeNewFile(identityFile, pem.EncodeToMemory(block), 0o600); err != nil {
		return Key{}, err
	}

	return writePublicKey(identityFile, publicKeyFile, privateKey.Public())
}

func ensurePublicKeyForExistingIdentity(identityFile string, publicKeyFile string) (Key, error) {
	if data, err := os.ReadFile(publicKeyFile); err == nil {
		if chmodErr := os.Chmod(publicKeyFile, 0o644); chmodErr != nil {
			return Key{}, fmt.Errorf("chmod ssh public key %q: %w", publicKeyFile, chmodErr)
		}
		if authorizedKey := strings.TrimSpace(string(data)); authorizedKey != "" {
			return Key{IdentityFile: identityFile, PublicKeyFile: publicKeyFile, AuthorizedKey: authorizedKey}, nil
		}
		if err := os.Remove(publicKeyFile); err != nil {
			return Key{}, fmt.Errorf("remove empty ssh public key %q: %w", publicKeyFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Key{}, fmt.Errorf("read ssh public key %q: %w", publicKeyFile, err)
	}

	data, err := os.ReadFile(identityFile)
	if err != nil {
		return Key{}, fmt.Errorf("read ssh identity %q: %w", identityFile, err)
	}
	privateKey, err := ssh.ParseRawPrivateKey(data)
	if err != nil {
		return Key{}, fmt.Errorf("parse ssh identity %q: %w", identityFile, err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		return Key{}, fmt.Errorf("derive ssh public key from %q: %w", identityFile, err)
	}
	return writePublicKey(identityFile, publicKeyFile, signer.PublicKey())
}

func writePublicKey(identityFile string, publicKeyFile string, key any) (Key, error) {
	var publicKey ssh.PublicKey
	switch typed := key.(type) {
	case ssh.PublicKey:
		publicKey = typed
	default:
		var err error
		publicKey, err = ssh.NewPublicKey(typed)
		if err != nil {
			return Key{}, fmt.Errorf("encode ssh public key: %w", err)
		}
	}

	publicKeyBytes := ssh.MarshalAuthorizedKey(publicKey)
	if err := os.WriteFile(publicKeyFile, publicKeyBytes, 0o644); err != nil {
		return Key{}, fmt.Errorf("write ssh public key %q: %w", publicKeyFile, err)
	}
	if err := os.Chmod(publicKeyFile, 0o644); err != nil {
		return Key{}, fmt.Errorf("chmod ssh public key %q: %w", publicKeyFile, err)
	}
	return Key{
		IdentityFile:  identityFile,
		PublicKeyFile: publicKeyFile,
		AuthorizedKey: strings.TrimSpace(string(publicKeyBytes)),
	}, nil
}

func writeNewFile(filePath string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %q: %w", filePath, err)
	}
	writeErr := func() error {
		if _, err := file.Write(data); err != nil {
			return fmt.Errorf("write %q: %w", filePath, err)
		}
		return nil
	}()
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return fmt.Errorf("close %q: %w", filePath, closeErr)
	}
	if err := os.Chmod(filePath, mode); err != nil {
		return fmt.Errorf("chmod %q: %w", filePath, err)
	}
	return nil
}

func AuthorizedKeysPath(user string) string {
	if user == "root" {
		return "/root/.ssh/authorized_keys"
	}
	return "/home/" + user + "/.ssh/authorized_keys"
}

type AuthorizedKeysInstallPlan struct {
	AuthorizedKeysPath string
	SSHDir             string
	Owner              string
	TempKeyPath        string
	TempKeyText        string
	TempKeyMode        string
	AppendScript       string
}

type AuthorizedKeysAppendCommand struct {
	Name      string
	Path      string
	Args      []string
	InputPath string
}

func NewAuthorizedKeysInstallPlan(user string, authorizedKey string) AuthorizedKeysInstallPlan {
	authorizedKeysPath := AuthorizedKeysPath(user)
	tempKeyPath := "/run/virtie-autoprovision-authorized-key.pub"
	return AuthorizedKeysInstallPlan{
		AuthorizedKeysPath: authorizedKeysPath,
		SSHDir:             path.Dir(authorizedKeysPath),
		Owner:              user + ":users",
		TempKeyPath:        tempKeyPath,
		TempKeyText:        authorizedKey + "\n",
		TempKeyMode:        "0600",
		AppendScript: `set -eu
PATH=/run/current-system/sw/bin:/bin
auth=$1
keyfile=$2
touch "$auth"
if ! grep -qxF -f "$keyfile" "$auth"; then
  cat "$keyfile" >> "$auth"
fi
rm -f "$keyfile"`,
	}
}

func (p AuthorizedKeysInstallPlan) AppendCommand(shellPath string) AuthorizedKeysAppendCommand {
	return AuthorizedKeysAppendCommand{
		Name:      "append authorized_keys",
		Path:      shellPath,
		Args:      []string{"-c", p.AppendScript, "virtie-ssh-autoprovision", p.AuthorizedKeysPath, p.TempKeyPath},
		InputPath: p.AuthorizedKeysPath,
	}
}

func encodeRemoteCommand(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return shellquote.Join(args...)
}
