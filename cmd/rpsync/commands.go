package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/api"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/ccapi"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/config"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/discovery"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/remote"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/service"
)

// runDiscover finds cameras and prints what each one says about itself.
func runDiscover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to rpsync.yaml")
	timeout := fs.Duration("timeout", 0, "how long to listen for SSDP replies")
	scan := fs.Bool("scan", true, "probe the local subnets when SSDP finds nothing")
	jsonOut := fs.Bool("json", false, "print results as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *timeout > 0 {
		cfg.Camera.DiscoveryTimeout = config.Duration(*timeout)
	}

	cams, err := discovery.Discover(ctx, discovery.Options{
		Timeout: cfg.Camera.DiscoveryTimeout.D(),
		Port:    cfg.Camera.Port,
		Scan:    *scan,
	})
	if err != nil {
		return err
	}

	type found struct {
		discovery.Camera
		Model    string `json:"model,omitempty"`
		Serial   string `json:"serial_number,omitempty"`
		Firmware string `json:"firmware_version,omitempty"`
		Error    string `json:"error,omitempty"`
	}

	results := make([]found, 0, len(cams))
	for _, c := range cams {
		f := found{Camera: c}
		client := ccapi.New(c.Host, ccapi.Options{Port: c.Port, Timeout: cfg.Camera.RequestTimeout.D()})
		if info, err := client.DeviceInfo(ctx); err != nil {
			f.Error = err.Error()
		} else {
			f.Model, f.Serial, f.Firmware = info.ProductName, info.SerialNumber, info.FirmwareVersion
		}
		results = append(results, f)
	}

	if *jsonOut {
		return json.NewEncoder(os.Stdout).Encode(results)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ADDRESS\tMODEL\tFIRMWARE\tSERIAL\tVIA")
	for _, f := range results {
		model := f.Model
		if f.Error != "" {
			model = "(" + f.Error + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", f.Addr(), model, f.Firmware, f.Serial, f.Source)
	}
	return tw.Flush()
}

// discoverCameras is the shared "find a camera or explain why not" helper.
func discoverCameras(ctx context.Context, cfg config.Config) ([]discovery.Camera, error) {
	if !cfg.Camera.Discovery {
		return nil, errors.New("no camera host configured and discovery is disabled")
	}
	cams, err := discovery.Discover(ctx, discovery.Options{
		Timeout: cfg.Camera.DiscoveryTimeout.D(),
		Port:    cfg.Camera.Port,
		Scan:    true,
	})
	if err != nil {
		return nil, fmt.Errorf("%w\n\nCheck that the camera is awake, on the same network, and that CCAPI is activated", err)
	}
	return cams, nil
}

// runPair mints a pairing code for a phone, tablet or browser.
func runPair(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to rpsync.yaml")
	ttl := fs.Duration("ttl", api.DefaultPairingTTL, "how long the code stays valid")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	devices, err := loadDevices(cfg)
	if err != nil {
		return err
	}

	code, expires, err := devices.NewPairingCode(*ttl)
	if err != nil {
		return err
	}

	fmt.Printf("\n  Pairing code:  %s\n", code)
	fmt.Printf("  Valid until:   %s (%s)\n\n", expires.Format(time.Kitchen), ttl.String())

	fmt.Println("  Enter it in the companion app or the web UI at one of:")
	port := portFromAddr(cfg.Server.Addr)
	if cfg.Server.AdvertiseURL != "" {
		fmt.Printf("    %s\n", cfg.Server.AdvertiseURL)
	}
	for _, url := range remote.LocalURLs(port) {
		fmt.Printf("    %s\n", url)
	}
	fmt.Println("\n  The code works once. A running daemon picks it up immediately.")
	return nil
}

// runDevices lists or revokes paired devices.
func runDevices(args []string) error {
	fs := flag.NewFlagSet("devices", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to rpsync.yaml")
	revoke := fs.String("revoke", "", "revoke the device with this id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Also accept the friendlier "rpsync devices revoke <id>".
	if rest := fs.Args(); len(rest) == 2 && rest[0] == "revoke" {
		*revoke = rest[1]
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	devices, err := loadDevices(cfg)
	if err != nil {
		return err
	}

	if *revoke != "" {
		removed, err := devices.Revoke(*revoke)
		if err != nil {
			return err
		}
		if !removed {
			return fmt.Errorf("no paired device with id %q", *revoke)
		}
		fmt.Printf("revoked %s\n", *revoke)
		return nil
	}

	list := devices.Devices()
	if len(list) == 0 {
		fmt.Println("No devices paired. Run `rpsync pair` to add one.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tPLATFORM\tPAIRED\tLAST SEEN")
	for _, d := range list {
		lastSeen := "never"
		if !d.LastSeen.IsZero() {
			lastSeen = d.LastSeen.Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			d.ID, d.Name, orDash(d.Platform), d.CreatedAt.Format(time.RFC3339), lastSeen)
	}
	return tw.Flush()
}

func loadDevices(cfg config.Config) (*api.DeviceStore, error) {
	if err := os.MkdirAll(cfg.Storage.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory %s: %w", cfg.Storage.StateDir, err)
	}
	return api.LoadDeviceStore(filepath.Join(cfg.Storage.StateDir, "devices.json"))
}

// runConfig prints the effective configuration and where it came from.
func runConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to rpsync.yaml")
	showPath := fs.Bool("path", false, "print only the config file search path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showPath {
		for _, p := range config.ConfigSearchPath() {
			marker := " "
			if _, err := os.Stat(p); err == nil {
				marker = "*"
			}
			fmt.Printf("%s %s\n", marker, p)
		}
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	out, err := cfg.Marshal()
	if err != nil {
		return err
	}
	fmt.Printf("# effective configuration (source: %s)\n", orNone(cfg.Path()))
	fmt.Print(string(out))
	return nil
}

// runService installs or controls the background service.
func runService(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: rpsync service <install|uninstall|start|stop|status> [flags]")
	}
	action, rest := args[0], args[1:]

	fs := flag.NewFlagSet("service "+action, flag.ContinueOnError)
	configPath := fs.String("config", "", "path to rpsync.yaml, passed to the service")
	name := fs.String("name", service.DefaultName, "service name")
	systemWide := fs.Bool("system", false, "install for all users (needs root or administrator)")
	user := fs.String("user", "", "run a system-wide Linux service as this user")
	dest := fs.String("dest", "", "destination directory, passed to the service")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	cfg := service.Config{Name: *name, SystemWide: *systemWide, User: *user}
	cfg.Args = []string{"daemon"}
	if *configPath != "" {
		abs, err := filepath.Abs(*configPath)
		if err != nil {
			return err
		}
		cfg.Args = append(cfg.Args, "--config", abs)
	}
	if *dest != "" {
		abs, err := filepath.Abs(*dest)
		if err != nil {
			return err
		}
		cfg.Args = append(cfg.Args, "--dest", abs)
	}

	switch action {
	case "install":
		res, err := service.Install(cfg)
		if err != nil {
			return err
		}
		fmt.Printf("installed %q via %s\n", cfg.Name, res.Manager)
		if res.Path != "" {
			fmt.Printf("  unit file: %s\n", res.Path)
		}
		for _, line := range res.Instructions {
			fmt.Printf("  %s\n", line)
		}
		return nil

	case "uninstall":
		if err := service.Uninstall(cfg); err != nil {
			if errors.Is(err, service.ErrNotInstalled) {
				fmt.Println("not installed")
				return nil
			}
			return err
		}
		fmt.Printf("removed %q\n", cfg.Name)
		return nil

	case "start":
		if err := service.Start(cfg); err != nil {
			return err
		}
		fmt.Printf("started %q\n", cfg.Name)
		return nil

	case "stop":
		if err := service.Stop(cfg); err != nil {
			return err
		}
		fmt.Printf("stopped %q\n", cfg.Name)
		return nil

	case "status":
		st, err := service.Query(cfg)
		if err != nil {
			return err
		}
		fmt.Printf("manager:   %s\n", st.Manager)
		fmt.Printf("installed: %t\n", st.Installed)
		fmt.Printf("running:   %t\n", st.Running)
		if st.Detail != "" {
			fmt.Printf("detail:    %s\n", st.Detail)
		}
		return nil

	default:
		return fmt.Errorf("unknown service action %q", action)
	}
}

// portFromAddr extracts the port from a listen address such as ":8787".
func portFromAddr(addr string) int {
	_, portStr, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return 8787
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port == 0 {
		return 8787
	}
	return port
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
