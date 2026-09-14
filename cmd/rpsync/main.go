// rpsync is the CLI and daemon for canon-rp-sync.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/ccapi"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/discovery"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/manifest"
	rpsync "github.com/JoshuaAFerguson/canon-rp-sync/internal/sync"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "discover":
		err = runDiscover(ctx)
	case "pull":
		err = runPull(ctx, args, false)
	case "daemon":
		err = runPull(ctx, args, true)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: rpsync <command> [flags]

commands:
  discover   find CCAPI cameras on the LAN and print device info
  pull       import new files once and exit
  daemon     keep running, importing as the camera appears / shoots

pull/daemon flags:
  --host      camera IP (skip discovery)
  --dest      destination directory (default ./photos)
  --include   comma-separated extensions (default jpg,cr3)
  --delete    delete files from the card after import (default false)`)
}

func runDiscover(ctx context.Context) error {
	cams, err := discovery.Discover(ctx, 3*time.Second)
	if err != nil {
		return err
	}
	for _, c := range cams {
		info, err := ccapi.New(c.Host, c.Port).DeviceInfo(ctx)
		if err != nil {
			fmt.Printf("%s:%d  (error: %v)\n", c.Host, c.Port, err)
			continue
		}
		fmt.Printf("%s:%d  %s %s  fw %s  serial %s\n",
			c.Host, c.Port, info.Manufacturer, info.ProductName, info.FirmwareVersion, info.SerialNumber)
	}
	return nil
}

func runPull(ctx context.Context, args []string, daemon bool) error {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	host := fs.String("host", "", "camera IP")
	dest := fs.String("dest", "./photos", "destination directory")
	include := fs.String("include", "jpg,cr3", "extensions to import")
	del := fs.Bool("delete", false, "delete from card after import")
	interval := fs.Duration("interval", 10*time.Second, "daemon: retry interval when camera is offline")
	_ = fs.Parse(args)

	opt := rpsync.Options{Dest: *dest, Include: map[string]bool{}, DeleteAfterImport: *del}
	for _, e := range strings.Split(*include, ",") {
		if e = strings.TrimSpace(strings.ToLower(e)); e != "" {
			opt.Include[e] = true
		}
	}

	if err := os.MkdirAll(*dest, 0o755); err != nil {
		return err
	}
	mf, err := manifest.Load(filepath.Join(*dest, ".rpsync-manifest.json"))
	if err != nil {
		return err
	}

	for {
		cam, err := connect(ctx, *host)
		if err != nil {
			if !daemon {
				return err
			}
			log.Printf("camera not reachable (%v); retrying in %s", err, *interval)
			if !sleep(ctx, *interval) {
				return ctx.Err()
			}
			continue
		}

		n, err := rpsync.Run(ctx, cam, mf, opt)
		log.Printf("imported %d file(s)", n)
		if !daemon {
			return err
		}
		if err != nil {
			log.Printf("sync error: %v", err)
			if !sleep(ctx, *interval) {
				return ctx.Err()
			}
			continue
		}

		// Camera is up and we're caught up: block on events until something changes.
		for {
			ev, err := cam.PollEvents(ctx)
			if err != nil {
				log.Printf("event poll ended: %v", err)
				break
			}
			if len(ev.AddedContents) > 0 {
				n, err := rpsync.Run(ctx, cam, mf, opt)
				log.Printf("imported %d file(s)", n)
				if err != nil {
					log.Printf("sync error: %v", err)
					break
				}
			}
		}
	}
}

func connect(ctx context.Context, host string) (*ccapi.Client, error) {
	if host == "" {
		cams, err := discovery.Discover(ctx, 3*time.Second)
		if err != nil {
			return nil, err
		}
		host = cams[0].Host
	}
	cam := ccapi.New(host, ccapi.DefaultPort)
	info, err := cam.DeviceInfo(ctx)
	if err != nil {
		return nil, err
	}
	log.Printf("connected to %s (%s)", info.ProductName, host)
	return cam, nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
