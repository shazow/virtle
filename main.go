// Command virtle launches and controls QEMU sandbox VMs described by a
// manifest. Run virtle --help for the command list; see README.md for the
// manifest format.
package main

import (
	"bytes"
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
	"github.com/shazow/virtle/backend"
	"github.com/shazow/virtle/backend/qemu"
	"github.com/shazow/virtle/backend/qemu/session"
	"github.com/shazow/virtle/internal/control"
	"github.com/shazow/virtle/internal/manifest"
	manifestschema "github.com/shazow/virtle/internal/manifest/schema"
	manifestapi "github.com/shazow/virtle/manifest"
)

type Options struct {
	Manifest string `long:"manifest" value-name:"MANIFEST" description:"Path to the virtle manifest"`
	Verbose  []bool `short:"v" long:"verbose" description:"Increase logging verbosity (-v for lifecycle, -vv for debugging)."`
	Version  bool   `long:"version" description:"Print the virtle version and exit"`

	Launch struct {
		Resume string `long:"resume" choice:"no" choice:"auto" choice:"force" default:"auto" description:"Resume suspended VM instead of launching a fresh one"`
		SSH    bool   `long:"ssh" description:"Attach an SSH session after launch readiness"`

		Args struct {
			RemoteCommand []string `positional-arg-name:"remote-cmd"`
		} `positional-args:"yes"`
	} `command:"launch" description:"Launch a virtiofs + ssh sandbox session" long-description:"Start configured host-side run processes, launch QEMU directly, then optionally attach over ssh."`

	Suspend struct{} `command:"suspend" description:"Suspend a running sandbox session" long-description:"Save QEMU state to disk and exit the launch session."`

	Status struct{} `command:"status" description:"Report the running virtual machine status"`

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

var rootLogger = slog.New(slog.DiscardHandler)

const extraHelp = `Run 'virtle <command> --help' for more information on a command.
Project repository: https://github.com/shazow/virtle
`

func runLaunch(options *Options) error {
	if len(options.Launch.Args.RemoteCommand) > 0 && !options.Launch.SSH {
		return fmt.Errorf("remote command arguments require --ssh")
	}

	doc, resolvedPath, err := loadManifestDocument(options.Manifest)
	if err != nil {
		return err
	}
	rootLogger.With("package", "main").Info("loading launch manifest", "path", resolvedPath)
	spec, b, err := manifestapi.LoadDocument(doc)
	if err != nil {
		return fmt.Errorf("load manifest %q: %w", resolvedPath, err)
	}
	if qemuBackend, ok := b.(*qemu.Backend); ok {
		qemuBackend.Logger = rootLogger
		qemuBackend.ConsoleOutput = os.Stderr
	}
	loaded, err := doc.ManifestWithOptions(manifest.ResolveOptions{Logger: rootLogger.With("package", "manifest")})
	if err != nil {
		return fmt.Errorf("load manifest %q: %w", resolvedPath, err)
	}

	return session.Run(context.Background(), b, spec, loaded, session.Options{
		Resume:        options.Launch.Resume,
		SSH:           options.Launch.SSH,
		RemoteCommand: options.Launch.Args.RemoteCommand,
		Logger:        rootLogger,
	})
}

// controlSession loads the manifest, resolves its control socket, and returns
// a signal-aware context for one command against a running VM.
func controlSession(manifestPath string) (context.Context, context.CancelFunc, string, error) {
	mf, err := loadManifest(manifestPath)
	if err != nil {
		return nil, nil, "", err
	}
	socketPath, err := mf.ResolvedControlSocketPath()
	if err != nil {
		return nil, nil, "", err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return ctx, cancel, socketPath, nil
}

func runSuspend(options *Options) error {
	ctx, cancel, socketPath, err := controlSession(options.Manifest)
	if err != nil {
		return err
	}
	defer cancel()
	m, err := control.Dial(ctx, socketPath)
	if err != nil {
		return err
	}
	suspender, ok := m.(backend.Suspender)
	if !ok {
		return fmt.Errorf("control socket machine cannot suspend: %w", errors.ErrUnsupported)
	}
	if err := suspender.Suspend(ctx); err != nil {
		return err
	}
	if err := m.Wait(ctx); err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, "suspended vm")
	return err
}

func runStatus(options *Options) error {
	ctx, cancel, socketPath, err := controlSession(options.Manifest)
	if err != nil {
		return err
	}
	defer cancel()
	m, err := control.Dial(ctx, socketPath)
	if err != nil {
		return err
	}
	reporter, ok := m.(backend.StatusReporter)
	if !ok {
		return fmt.Errorf("control socket machine cannot report status: %w", errors.ErrUnsupported)
	}
	status, err := reporter.Status(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(status)
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
	mf, err := loadManifest(options.Manifest)
	if err != nil {
		return err
	}
	return toml.NewEncoder(os.Stdout).Encode(mf)
}

func runManifestValidate(options *Options) error {
	if _, err := loadManifest(options.Manifest); err != nil {
		return err
	}
	_, err := fmt.Fprintln(os.Stdout, "manifest is valid")
	return err
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
	ctx, cancel, socketPath, err := controlSession(options.Manifest)
	if err != nil {
		return err
	}
	defer cancel()
	params, err := json.Marshal(control.HotplugRequest{ID: options.Hotplug.Args.ID, Detach: options.Hotplug.Detach})
	if err != nil {
		return err
	}
	if _, err := control.Raw(ctx, socketPath, "hotplug", params); err != nil {
		return err
	}
	action := "attached"
	if options.Hotplug.Detach {
		action = "detached"
	}
	_, err = fmt.Fprintf(os.Stdout, "%s hotplug device: %s\n", action, options.Hotplug.Args.ID)
	return err
}

func runRPC(options *Options) error {
	ctx, cancel, socketPath, err := controlSession(options.Manifest)
	if err != nil {
		return err
	}
	defer cancel()

	params := json.RawMessage("{}")
	if options.RPC.Args.Params != "" {
		params = json.RawMessage(options.RPC.Args.Params)
		if !json.Valid(params) {
			return fmt.Errorf("rpc params must be valid JSON")
		}
	}

	result, err := control.Raw(ctx, socketPath, options.RPC.Args.Method, params)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, string(result))
	return err
}

func loadManifest(path string) (*manifest.Manifest, error) {
	doc, resolvedPath, err := loadManifestDocument(path)
	if err != nil {
		return nil, err
	}
	loaded, err := doc.Manifest()
	if err != nil {
		return nil, fmt.Errorf("load manifest %q: %w", resolvedPath, err)
	}
	return loaded, nil
}

func loadManifestDocument(path string) (manifest.Document, string, error) {
	resolvedPath, err := resolveManifestPath(path)
	if err != nil {
		return manifest.Document{}, "", fmt.Errorf("resolve manifest path %q: %w", path, err)
	}

	// The manifest is an input virtle must never modify: os.ReadFile opens it
	// without write intent and nothing writes it back.
	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return manifest.Document{}, "", fmt.Errorf("open manifest %q: %w", resolvedPath, err)
	}
	doc, err := manifest.DecodeDocumentBytes(data, resolvedPath)
	if err != nil {
		return manifest.Document{}, "", fmt.Errorf("load manifest %q: %w", resolvedPath, err)
	}

	// A relative working_dir, including the "." default, resolves against the
	// process working directory, so running virtle from different directories
	// takes different relative paths into effect. This happens in memory only.
	if err := doc.ResolveWorkingDir(); err != nil {
		return manifest.Document{}, "", err
	}

	return doc, resolvedPath, nil
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

func main() {
	if err := run(os.Args[1:]); err != nil {
		var flagsErr *flags.Error
		if errors.As(err, &flagsErr) && flagsErr.Type == flags.ErrHelp {
			fmt.Fprintln(os.Stdout, flagsErr.Message)
			fmt.Fprint(os.Stdout, extraHelp)
			os.Exit(0)
		}

		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(session.ExitCode(err))
	}
}

func run(args []string) error {
	opts := &Options{}
	parser := newParserForOptions(opts)

	if _, err := parser.ParseArgs(args); err != nil {
		var flagsErr *flags.Error
		if errors.As(err, &flagsErr) && flagsErr.Type == flags.ErrCommandRequired {
			if opts.Version {
				// --version stands on its own, so it answers instead of the
				// parser's missing-command complaint.
				return printVersion(os.Stdout)
			}
			// Missing command: show the relevant help instead of only the
			// bare "Please specify one command" message.
			var help bytes.Buffer
			parser.WriteHelp(&help)
			help.WriteString("\n")
			help.WriteString(extraHelp)
			return &flags.Error{Type: flags.ErrCommandRequired, Message: strings.TrimRight(help.String(), "\n")}
		}
		return err
	}

	if opts.Version {
		return printVersion(os.Stdout)
	}
	level := slog.LevelWarn
	if len(opts.Verbose) == 1 {
		level = slog.LevelInfo
	} else if len(opts.Verbose) >= 2 {
		level = slog.LevelDebug
	}
	slog.SetLogLoggerLevel(level)
	rootLogger = slog.Default()

	switch parser.Active.Name {
	case "launch":
		return runLaunch(opts)
	case "suspend":
		return runSuspend(opts)
	case "status":
		return runStatus(opts)
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

func newParserForOptions(opts *Options) *flags.Parser {
	// PrintErrors is deliberately left out of the parser options: main is the
	// single place parse errors get printed, so they never appear twice.
	return flags.NewParser(opts, flags.HelpFlag|flags.PassDoubleDash)
}
