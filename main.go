// Command virtle launches the supported agentspace sandbox session.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"
	"github.com/jessevdk/go-flags"
	"github.com/shazow/virtle/internal/balloon"
	"github.com/shazow/virtle/internal/manager"
	"github.com/shazow/virtle/internal/manager/control"
	"github.com/shazow/virtle/internal/manifest"
	manifestschema "github.com/shazow/virtle/internal/manifest/schema"
)

type Options struct {
	Manifest string `long:"manifest" value-name:"MANIFEST" description:"Path to the virtle manifest"`
	Verbose  []bool `short:"v" long:"verbose" description:"Show verbose logging."`

	Launch struct {
		Resume string `long:"resume" choice:"no" choice:"auto" choice:"force" default:"auto" description:"Resume suspended VM instead of launching a fresh one"`
		SSH    bool   `long:"ssh" description:"Attach an SSH session after launch readiness"`

		Args struct {
			RemoteCommand []string `positional-arg-name:"remote-cmd"`
		} `positional-args:"yes"`
	} `command:"launch" description:"Launch a virtiofs + ssh sandbox session" long-description:"Start configured host-side run processes, launch QEMU directly, then optionally attach over ssh."`

	Suspend struct{} `command:"suspend" description:"Suspend a running sandbox session" long-description:"Save QEMU state to disk and exit the launch session."`

	Hotplug struct {
		Detach bool `long:"detach" description:"Detach the hotplug device instead of attaching it"`

		Args struct {
			ID string `positional-arg-name:"id" required:"yes"`
		} `positional-args:"yes"`
	} `command:"hotplug" description:"Attach or detach a predefined hotplug device" long-description:"Attach or detach a device described under manifest [hotplug]."`

	RPC struct {
		Args struct {
			Method string `positional-arg-name:"method" required:"yes"`
			Params string `positional-arg-name:"json-args"`
		} `positional-args:"yes"`
	} `command:"rpc" description:"Call a virtle control socket RPC method" long-description:"Call a method on the running virtle control socket with optional JSON params."`

	ManifestCommand struct {
		Defaults struct {
			Resolved bool `long:"resolved" description:"Print the resolved internal runtime manifest instead of the input manifest defaults"`
		} `command:"defaults" description:"Print the manifest defaults as TOML" long-description:"Print the manifest input defaults assumed by virtle when optional fields are omitted, encoded as TOML. Use --resolved to print the internal resolved runtime manifest defaults."`

		Validate struct{} `command:"validate" description:"Validate a manifest" long-description:"Load, resolve, and validate the virtle manifest input format."`
		Resolve  struct{} `command:"resolve" description:"Print the resolved manifest" long-description:"Load, resolve, validate, and print the internal runtime manifest as TOML."`
		Schema   struct{} `command:"schema" description:"Print the manifest JSON Schema" long-description:"Print the generated JSON Schema for the virtle manifest input format."`
	} `command:"manifest" description:"Inspect and work with virtle manifests" long-description:"Inspect and work with the virtle manifest input format."`
}

func runLaunch(options *Options) error {
	if len(options.Launch.Args.RemoteCommand) > 0 && !options.Launch.SSH {
		return fmt.Errorf("remote command arguments require --ssh")
	}

	baseLogger := slog.Default()
	discardLogger := slog.New(slog.DiscardHandler)
	manifestLogger := discardLogger
	manager.SetLogger(discardLogger)
	balloon.SetLogger(discardLogger)
	if len(options.Verbose) > 0 {
		manifestLogger = baseLogger.With("package", "manifest")
		manager.SetLogger(baseLogger.With("package", "manager"))
	}
	if len(options.Verbose) > 1 {
		balloon.SetLogger(baseLogger.With("package", "balloon"))
	}

	manifest, err := loadLaunchManifest(options.Manifest, manifestLogger)
	if err != nil {
		return err
	}

	return manager.LaunchWithOptions(context.Background(), manifest, options.Launch.Args.RemoteCommand, manager.LaunchOptions{
		Resume:    manager.ResumeMode(options.Launch.Resume),
		SSH:       options.Launch.SSH,
		Verbosity: len(options.Verbose),
	})
}

func runSuspend(options *Options) error {
	manifest, err := loadManifest(options.Manifest)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return manager.Suspend(ctx, manifest)
}

func runManifestDefaults(options *Options) error {
	if options.ManifestCommand.Defaults.Resolved {
		defaults, err := manifest.DefaultManifest()
		if err != nil {
			return err
		}
		return toml.NewEncoder(os.Stdout).Encode(defaults)
	}
	return toml.NewEncoder(os.Stdout).Encode(manifest.DefaultDocument())
}

func runManifestResolve(options *Options) error {
	manifest, err := loadManifest(options.Manifest)
	if err != nil {
		return err
	}
	return toml.NewEncoder(os.Stdout).Encode(manifest)
}

func runManifestValidate(options *Options) error {
	_, err := loadManifest(options.Manifest)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "manifest is valid")
	return nil
}

func runManifestSchema() error {
	data, err := manifestschema.GenerateJSON()
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}

func runHotplug(options *Options) error {
	baseLogger := slog.Default()
	discardLogger := slog.New(slog.DiscardHandler)
	manifestLogger := discardLogger
	manager.SetLogger(discardLogger)
	if len(options.Verbose) > 0 {
		manifestLogger = baseLogger.With("package", "manifest")
		manager.SetLogger(baseLogger.With("package", "manager"))
	}

	manifest, err := loadLaunchManifest(options.Manifest, manifestLogger)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return manager.Hotplug(ctx, manifest, options.Hotplug.Args.ID, options.Hotplug.Detach)
}

func runRPC(options *Options) error {
	manifest, err := loadManifest(options.Manifest)
	if err != nil {
		return err
	}
	controlSocketPath, err := manifest.ResolvedControlSocketPath()
	if err != nil {
		return err
	}

	params := json.RawMessage("{}")
	if options.RPC.Args.Params != "" {
		params = json.RawMessage(options.RPC.Args.Params)
		if !json.Valid(params) {
			return fmt.Errorf("rpc params must be valid JSON")
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	result, err := control.Dial(controlSocketPath).Raw(ctx, options.RPC.Args.Method, params)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, string(result))
	return err
}

func loadLaunchManifest(path string, logger *slog.Logger) (*manifest.Manifest, error) {
	doc, resolvedPath, data, err := loadManifestDocumentData(path)
	if err != nil {
		return nil, err
	}
	workingDir := doc.WorkingDir
	if workingDir == "" {
		workingDir = "."
	}
	if !filepath.IsAbs(workingDir) {
		resolvedWorkingDir, err := filepath.Abs(workingDir)
		if err != nil {
			return nil, fmt.Errorf("resolve manifest working directory %q: %w", workingDir, err)
		}
		doc.WorkingDir = resolvedWorkingDir
		if err := writeManifestWorkingDir(resolvedPath, data, resolvedWorkingDir); err != nil {
			return nil, err
		}
	}

	return doc.ManifestWithOptions(manifest.ResolveOptions{Logger: logger})
}

func loadManifest(path string) (*manifest.Manifest, error) {
	manifest, _, _, err := loadManifestData(path)
	return manifest, err
}

func loadManifestData(path string) (*manifest.Manifest, string, []byte, error) {
	doc, resolvedPath, data, err := loadManifestDocumentData(path)
	if err != nil {
		return nil, "", nil, err
	}
	manifest, err := doc.Manifest()
	if err != nil {
		return nil, "", nil, fmt.Errorf("load manifest %q: %w", resolvedPath, err)
	}
	return manifest, resolvedPath, data, nil
}

func loadManifestDocumentData(path string) (manifest.Document, string, []byte, error) {
	resolvedPath, err := resolveManifestPath(path)
	if err != nil {
		return manifest.Document{}, "", nil, fmt.Errorf("resolve manifest path %q: %w", path, err)
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return manifest.Document{}, "", nil, fmt.Errorf("open manifest %q: %w", resolvedPath, err)
	}
	doc, err := manifest.DecodeDocumentBytes(data, resolvedPath)
	if err != nil {
		return manifest.Document{}, "", nil, fmt.Errorf("load manifest %q: %w", resolvedPath, err)
	}

	return doc, resolvedPath, data, nil
}

func resolveManifestPath(path string) (string, error) {
	if path != "" {
		return filepath.Abs(path)
	}

	var checked []string
	for _, candidate := range []string{"manifest.toml", "manifest.json"} {
		resolved, err := filepath.Abs(candidate)
		if err != nil {
			return "", err
		}
		checked = append(checked, resolved)
		if _, err := os.Stat(resolved); err == nil {
			return resolved, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}

	return "", fmt.Errorf("no manifest path provided and no default manifest found; checked %s", strings.Join(checked, ", "))
}

func writeManifestWorkingDir(path string, data []byte, workingDir string) error {
	updated, err := manifest.UpdateWorkingDirBytes(data, path, workingDir)
	if err != nil {
		return fmt.Errorf("update manifest %q working dir: %w", path, err)
	}

	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary manifest %q: %w", path, err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = os.Remove(tempPath)
	}()

	if _, err := temp.Write(updated); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary manifest %q: %w", tempPath, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary manifest %q: %w", tempPath, err)
	}
	if err := os.Chmod(tempPath, 0o644); err != nil {
		return fmt.Errorf("chmod temporary manifest %q: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace manifest %q: %w", path, err)
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		var flagsErr *flags.Error
		if errors.As(err, &flagsErr) && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		}

		fmt.Fprintln(os.Stderr, err)
		os.Exit(manager.ExitCode(err))
	}
}

func run(args []string) error {
	opts := &Options{}
	parser := newParserForOptions(opts)

	if _, err := parser.ParseArgs(args); err != nil {
		return err
	}

	switch parser.Active.Name {
	case "launch":
		return runLaunch(opts)
	case "suspend":
		return runSuspend(opts)
	case "hotplug":
		return runHotplug(opts)
	case "rpc":
		return runRPC(opts)
	case "manifest":
		switch parser.Active.Active.Name {
		case "defaults":
			return runManifestDefaults(opts)
		case "validate":
			return runManifestValidate(opts)
		case "resolve":
			return runManifestResolve(opts)
		case "schema":
			return runManifestSchema()
		default:
			return fmt.Errorf("unknown manifest command %q", parser.Active.Active.Name)
		}
	default:
		return fmt.Errorf("unknown command %q", parser.Active.Name)
	}
}

func newParser() *flags.Parser {
	return newParserForOptions(&Options{})
}

func newParserForOptions(opts *Options) *flags.Parser {
	return flags.NewParser(opts, flags.Default|flags.PassDoubleDash)
}
