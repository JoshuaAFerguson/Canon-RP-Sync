package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/api"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/ccapi"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/config"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/events"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/manifest"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/remote"
	rpsync "github.com/JoshuaAFerguson/canon-rp-sync/internal/sync"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/watcher"
)

// daemon holds the pieces the long-running process wires together.
type daemon struct {
	cfg      config.Config
	manifest *manifest.Manifest
	importer *rpsync.Importer
	bus      *events.Bus
	devices  *api.DeviceStore
	watcher  *watcher.Watcher
	remote   *remote.Manager
}

// setup prepares storage, state and the sync core from a configuration.
func setup(cfg config.Config) (*daemon, error) {
	if err := os.MkdirAll(cfg.Storage.Dest, 0o755); err != nil {
		return nil, fmt.Errorf("create destination %s: %w", cfg.Storage.Dest, err)
	}
	if err := os.MkdirAll(cfg.Storage.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory %s: %w", cfg.Storage.StateDir, err)
	}
	// Anything left in staging is from an interrupted run and will be fetched
	// again; clearing it stops a crash loop from filling the disk.
	if err := rpsync.CleanStaging(cfg.Storage.Dest); err != nil {
		return nil, fmt.Errorf("clean staging directory: %w", err)
	}

	mf, err := manifest.Load(cfg.Storage.Manifest)
	if err != nil {
		return nil, err
	}
	devices, err := api.LoadDeviceStore(filepath.Join(cfg.Storage.StateDir, "devices.json"))
	if err != nil {
		return nil, err
	}

	importer := rpsync.New(mf, rpsync.Options{
		Dest:              cfg.Storage.Dest,
		Layout:            cfg.Storage.Layout,
		Include:           cfg.IncludeSet(),
		DeleteAfterImport: cfg.Import.DeleteAfterImport,
		DateSource:        rpsync.DateSource(cfg.Storage.DateSource),
	})

	return &daemon{
		cfg:      cfg,
		manifest: mf,
		importer: importer,
		bus:      events.NewBus(),
		devices:  devices,
	}, nil
}

// runDaemon watches the camera, serves the API and keeps remote access up until
// the process is asked to stop.
func runDaemon(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	var flags commonFlags
	flags.register(fs, true)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := flags.load(fs)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log.Level)

	d, err := setup(cfg)
	if err != nil {
		return err
	}

	log.Info("starting rpsync",
		"version", version,
		"dest", cfg.Storage.Dest,
		"state", cfg.Storage.StateDir,
		"config", orNone(cfg.Path()),
		"imported", d.manifest.Len(),
	)

	d.watcher = watcher.New(watcher.Config{
		Host:             cfg.Camera.Host,
		Port:             cfg.Camera.Port,
		Discovery:        cfg.Camera.Discovery,
		DiscoveryTimeout: cfg.Camera.DiscoveryTimeout.D(),
		PollInterval:     cfg.Camera.PollInterval.D(),
		RequestTimeout:   cfg.Camera.RequestTimeout.D(),
	}, d.importer, d.bus)

	port := portFromAddr(cfg.Server.Addr)
	cloudflareToken, err := cfg.CloudflareToken()
	if err != nil {
		// A missing token file should not stop imports; remote access degrades
		// to a quick tunnel or reports itself unavailable.
		log.Warn("cloudflare token unavailable", "error", err)
	}
	d.remote = remote.New(remote.Config{
		Mode:              cfg.Remote.Mode,
		Port:              port,
		AdvertiseURL:      cfg.Server.AdvertiseURL,
		TailscaleCLI:      cfg.Remote.Tailscale.CLI,
		TailscaleHostname: cfg.Remote.Tailscale.Hostname,
		CloudflaredBinary: cfg.Remote.Cloudflare.Binary,
		CloudflareToken:   cloudflareToken,
		CloudflareHost:    cfg.Remote.Cloudflare.Hostname,
		CloudflareManaged: cfg.Remote.Cloudflare.Managed,
		Bus:               d.bus,
		Logger:            log,
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	run := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(runCtx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("component stopped", "component", name, "error", err)
			}
		}()
	}

	run("watcher", d.watcher.Run)
	if cfg.Remote.Mode != remote.ModeNone {
		run("remote", d.remote.Run)
	}

	if cfg.Server.Enabled {
		srv := api.New(api.Config{
			Devices:                   d.devices,
			Manifest:                  d.manifest,
			Importer:                  d.importer,
			Camera:                    d.watcher,
			Bus:                       d.bus,
			Logger:                    log,
			ThumbDir:                  filepath.Join(cfg.Storage.StateDir, "thumbnails"),
			MaxUploadBytes:            cfg.Server.MaxUploadBytes,
			AllowLocalUnauthenticated: cfg.Server.AllowLocalUnauthenticated,
			AdvertiseURL:              cfg.Server.AdvertiseURL,
			Version:                   version,
			Remote: func() api.RemoteStatus {
				s := d.remote.Status()
				return api.RemoteStatus{Mode: s.Mode, State: s.State, URL: s.URL, Message: s.Message}
			},
		})
		run("http", func(ctx context.Context) error { return srv.Serve(ctx, cfg.Server.Addr) })

		printAccessInfo(log, cfg, port)
		if err := offerFirstPairing(log, d.devices); err != nil {
			log.Warn("could not generate a first pairing code", "error", err)
		}
	}

	<-runCtx.Done()
	log.Info("shutting down")
	cancel()
	wg.Wait()
	return nil
}

// printAccessInfo tells the user where to point a browser or a companion app.
func printAccessInfo(log logger, cfg config.Config, port int) {
	for _, url := range remote.LocalURLs(port) {
		log.Info("web UI", "url", url)
	}
	if cfg.Remote.Mode != remote.ModeNone {
		log.Info("remote access enabled", "mode", cfg.Remote.Mode)
	}
}

// offerFirstPairing mints a code on a fresh install, so the first device can be
// paired straight from the startup log — which is the only practical route on a
// NAS where there is no terminal to run `rpsync pair` in.
func offerFirstPairing(log logger, devices *api.DeviceStore) error {
	if len(devices.Devices()) > 0 {
		return nil
	}
	if active, _ := devices.PairingActive(); active {
		return nil
	}

	code, expires, err := devices.NewPairingCode(api.DefaultPairingTTL)
	if err != nil {
		return err
	}
	log.Info("no devices paired yet — open the web UI and enter this code",
		"pairing_code", code, "expires", expires.Format(time.Kitchen))
	return nil
}

// runPull imports once and exits, for cron jobs and manual runs.
func runPull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	var flags commonFlags
	flags.register(fs, false)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := flags.load(fs)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log.Level)

	d, err := setup(cfg)
	if err != nil {
		return err
	}

	cam, err := connectCamera(ctx, cfg, log)
	if err != nil {
		return err
	}

	d.importer.Notify = func(p rpsync.Progress) {
		switch p.Phase {
		case "importing":
			log.Info("importing", "file", p.File, "progress", fmt.Sprintf("%d/%d", p.Done+1, p.Total))
		case "error":
			log.Error("import failed", "file", p.File, "error", p.Err)
		}
	}

	res, err := d.importer.Pull(ctx, cam)
	log.Info("import finished",
		"imported", res.Imported, "skipped", res.Skipped, "bytes", res.Bytes, "errors", len(res.Errors))
	if err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("%d file(s) failed to import; the first was: %w", len(res.Errors), res.Errors[0])
	}
	return nil
}

// connectCamera resolves and reaches the camera for one-shot commands.
func connectCamera(ctx context.Context, cfg config.Config, log logger) (*ccapi.Client, error) {
	host, port := cfg.Camera.Host, cfg.Camera.Port
	if host == "" {
		cams, err := discoverCameras(ctx, cfg)
		if err != nil {
			return nil, err
		}
		host, port = cams[0].Host, cams[0].Port
	}

	cam := ccapi.New(host, ccapi.Options{Port: port, Timeout: cfg.Camera.RequestTimeout.D()})
	info, err := cam.DeviceInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("reach the camera at %s:%d: %w", host, port, err)
	}
	log.Info("connected", "camera", info.ProductName, "host", host, "firmware", info.FirmwareVersion)
	return cam, nil
}

// logger is the subset of slog.Logger the commands use, kept small so helpers
// stay easy to call from tests.
type logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

func orNone(s string) string {
	if s == "" {
		return "(defaults)"
	}
	return s
}
