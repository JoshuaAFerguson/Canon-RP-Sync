// Command rpsync imports photos from a Canon EOS RP over Wi-Fi.
//
// It runs as a foreground command, as a background service on Windows, macOS
// and Linux, or in a container on a NAS.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/config"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/service"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	// When Windows starts rpsync through the Service Control Manager there are
	// no arguments and no console; run the daemon under the SCM handler.
	if cmd == "daemon" || cmd == "service-run" {
		handled, err := service.RunAsService("rpsync", func(ctx context.Context) error {
			return runDaemon(ctx, args)
		})
		if handled {
			exitOn(err)
			return
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "daemon":
		err = runDaemon(ctx, args)
	case "pull":
		err = runPull(ctx, args)
	case "discover":
		err = runDiscover(ctx, args)
	case "pair":
		err = runPair(args)
	case "devices":
		err = runDevices(args)
	case "service":
		err = runService(args)
	case "config":
		err = runConfig(args)
	case "version", "--version", "-v":
		fmt.Printf("rpsync %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "rpsync: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	exitOn(err)
}

func exitOn(err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	fmt.Fprintln(os.Stderr, "rpsync: "+err.Error())
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, `rpsync — wireless photo import for the Canon EOS RP

usage: rpsync <command> [flags]

commands:
  daemon     watch for the camera and import continuously (also serves the API)
  pull       import anything new once, then exit
  discover   find CCAPI cameras on the local network
  pair       generate a pairing code for a phone, tablet or browser
  devices    list or revoke paired devices
  service    install or control the background service
  config     print the effective configuration
  version    print the version

Run "rpsync <command> -h" for the flags of a command.

Configuration is read from rpsync.yaml (see `+"`rpsync config`"+` for the search
path), overridden by RPSYNC_* environment variables, then by flags.
`)
}

// commonFlags are the configuration overrides shared by daemon and pull.
type commonFlags struct {
	configPath string
	host       string
	port       int
	dest       string
	include    string
	del        bool
	layout     string
	dateSource string
	addr       string
	noServer   bool
	remoteMode string
	logLevel   string
	stateDir   string
}

func (c *commonFlags) register(fs *flag.FlagSet, withServer bool) {
	fs.StringVar(&c.configPath, "config", "", "path to rpsync.yaml (default: search standard locations)")
	fs.StringVar(&c.host, "host", "", "camera address (skips discovery)")
	fs.IntVar(&c.port, "port", 0, "camera CCAPI port")
	fs.StringVar(&c.dest, "dest", "", "destination directory for imports")
	fs.StringVar(&c.include, "include", "", "comma-separated extensions to import, e.g. jpg,cr3")
	fs.BoolVar(&c.del, "delete", false, "delete files from the card after import")
	fs.StringVar(&c.layout, "layout", "", "folder layout, as a Go time format (default 2006/2006-01-02)")
	fs.StringVar(&c.dateSource, "date-source", "", `which date decides the folder: "exif" or "import"`)
	fs.StringVar(&c.logLevel, "log-level", "", "debug, info, warn or error")
	fs.StringVar(&c.stateDir, "state-dir", "", "directory for tokens and runtime state")
	if withServer {
		fs.StringVar(&c.addr, "addr", "", "address for the HTTP API and web UI (default :8787)")
		fs.BoolVar(&c.noServer, "no-server", false, "do not serve the HTTP API")
		fs.StringVar(&c.remoteMode, "remote", "", `remote access: "none", "tailscale" or "cloudflare"`)
	}
}

// load reads the configuration and applies the flags that were set. Only flags
// the user actually passed override the file, so an unset flag's zero value
// never silently wins.
func (c *commonFlags) load(fs *flag.FlagSet) (config.Config, error) {
	cfg, err := config.Load(c.configPath)
	if err != nil {
		return cfg, err
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if set["host"] {
		cfg.Camera.Host = c.host
	}
	if set["port"] {
		cfg.Camera.Port = c.port
	}
	if set["dest"] {
		cfg.Storage.Dest = c.dest
		// A new destination implies a manifest inside it unless pinned.
		cfg.Storage.Manifest = ""
	}
	if set["include"] {
		cfg.Import.Include = strings.Split(c.include, ",")
	}
	if set["delete"] {
		cfg.Import.DeleteAfterImport = c.del
	}
	if set["layout"] {
		cfg.Storage.Layout = c.layout
	}
	if set["date-source"] {
		cfg.Storage.DateSource = c.dateSource
	}
	if set["state-dir"] {
		cfg.Storage.StateDir = c.stateDir
	}
	if set["addr"] {
		cfg.Server.Addr = c.addr
	}
	if set["no-server"] {
		cfg.Server.Enabled = !c.noServer
	}
	if set["remote"] {
		cfg.Remote.Mode = c.remoteMode
	}
	if set["log-level"] {
		cfg.Log.Level = c.logLevel
	}

	cfg.Normalize()
	return cfg, cfg.Validate()
}

// newLogger builds the structured logger used across the daemon.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
