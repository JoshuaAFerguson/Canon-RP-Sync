// Package watcher keeps an eye on the camera: it finds it on the LAN, catches
// up on anything shot while it was away, then blocks on CCAPI's event stream
// and imports each new frame as it lands.
//
// The watcher owns the camera connection. Everything else — the HTTP API, the
// CLI — asks it for status or nudges it to sync, and never talks to the camera
// directly. That matters because the body accepts only a couple of simultaneous
// HTTP connections: the watcher makes sure a long poll is never left running
// while files are downloading.
package watcher

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/ccapi"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/discovery"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/events"
	rpsync "github.com/JoshuaAFerguson/canon-rp-sync/internal/sync"
)

// State is the watcher's connection state.
type State string

const (
	// StateSearching means no camera is reachable and we are looking for one.
	StateSearching State = "searching"
	// StateConnected means the camera answered and we are watching for shots.
	StateConnected State = "connected"
	// StateImporting means a transfer is in progress.
	StateImporting State = "importing"
	// StateStopped means the watcher is not running.
	StateStopped State = "stopped"
)

// CameraInfo identifies the connected body.
type CameraInfo struct {
	Host            string `json:"host"`
	Port            int    `json:"port"`
	Model           string `json:"model,omitempty"`
	SerialNumber    string `json:"serial_number,omitempty"`
	FirmwareVersion string `json:"firmware_version,omitempty"`
	// Source is how the camera was found: "config", "ssdp" or "scan".
	Source string `json:"source,omitempty"`
}

// Status is a snapshot of what the watcher is doing.
type Status struct {
	State  State       `json:"state"`
	Since  time.Time   `json:"since"`
	Camera *CameraInfo `json:"camera,omitempty"`

	LastSync     time.Time `json:"last_sync,omitempty"`
	LastImported int       `json:"last_imported"`
	// TotalImported counts files imported since the process started.
	TotalImported int    `json:"total_imported"`
	LastError     string `json:"last_error,omitempty"`
	// Progress describes an in-flight transfer.
	Progress *Progress `json:"progress,omitempty"`
}

// Progress describes the file currently being transferred.
type Progress struct {
	File  string `json:"file"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

// Config configures a watcher.
type Config struct {
	// Host pins the camera address. Empty means discover it.
	Host string
	Port int
	// Discovery enables SSDP/scan discovery when Host is empty.
	Discovery bool
	// DiscoveryTimeout bounds one search.
	DiscoveryTimeout time.Duration
	// PollInterval is how long to wait before looking again after a failure.
	PollInterval time.Duration
	// RequestTimeout bounds ordinary CCAPI requests.
	RequestTimeout time.Duration
}

func (c *Config) setDefaults() {
	if c.Port == 0 {
		c.Port = ccapi.DefaultPort
	}
	if c.DiscoveryTimeout <= 0 {
		c.DiscoveryTimeout = 3 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 15 * time.Second
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 60 * time.Second
	}
}

// Watcher runs the camera loop.
type Watcher struct {
	cfg      Config
	importer *rpsync.Importer
	bus      *events.Bus

	mu     sync.RWMutex
	status Status
	client *ccapi.Client

	syncNow chan struct{}
}

// New returns a watcher that imports through im and reports on bus.
func New(cfg Config, im *rpsync.Importer, bus *events.Bus) *Watcher {
	cfg.setDefaults()
	if bus == nil {
		bus = events.NewBus()
	}
	w := &Watcher{
		cfg:      cfg,
		importer: im,
		bus:      bus,
		status:   Status{State: StateStopped, Since: time.Now()},
		syncNow:  make(chan struct{}, 1),
	}
	im.Notify = w.onProgress
	return w
}

// Status returns a snapshot of the watcher's state.
func (w *Watcher) Status() Status {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.status
}

// Bus exposes the event bus, so the HTTP API can stream what happens.
func (w *Watcher) Bus() *events.Bus { return w.bus }

// SyncNow asks the watcher to catch up immediately. It never blocks: if a sync
// is already pending or running, the request is folded into it.
func (w *Watcher) SyncNow() {
	select {
	case w.syncNow <- struct{}{}:
	default:
	}
}

// Client returns the current camera client, or nil when disconnected. It is
// exposed so the API can serve thumbnails straight from the card.
func (w *Watcher) Client() *ccapi.Client {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.client
}

// Run drives the watcher until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	defer w.setState(StateStopped, nil)

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		cam, info, err := w.connect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.setError(err)
			if !sleep(ctx, w.cfg.PollInterval) {
				return nil
			}
			continue
		}

		w.mu.Lock()
		w.client = cam
		w.status.Camera = info
		w.status.LastError = ""
		w.mu.Unlock()
		w.setState(StateConnected, info)
		w.bus.Publishf(events.KindCamera, "connected to %s at %s", info.Model, info.Host)

		if err := w.watch(ctx, cam); err != nil && ctx.Err() == nil {
			w.setError(err)
		}

		w.mu.Lock()
		w.client = nil
		w.status.Camera = nil
		w.mu.Unlock()
		if ctx.Err() != nil {
			return nil
		}
		w.setState(StateSearching, nil)
		w.bus.Publishf(events.KindCamera, "camera unreachable; retrying in %s", w.cfg.PollInterval)
		if !sleep(ctx, w.cfg.PollInterval) {
			return nil
		}
	}
}

// connect resolves and reaches the camera.
func (w *Watcher) connect(ctx context.Context) (*ccapi.Client, *CameraInfo, error) {
	w.setState(StateSearching, nil)

	host, port, source := w.cfg.Host, w.cfg.Port, "config"
	if host == "" {
		if !w.cfg.Discovery {
			return nil, nil, errors.New("no camera host configured and discovery is disabled")
		}
		searchCtx, cancel := context.WithTimeout(ctx, w.cfg.DiscoveryTimeout+30*time.Second)
		cams, err := discovery.Discover(searchCtx, discovery.Options{
			Timeout: w.cfg.DiscoveryTimeout,
			Port:    w.cfg.Port,
			Scan:    true,
		})
		cancel()
		if err != nil {
			return nil, nil, err
		}
		host, port, source = cams[0].Host, cams[0].Port, cams[0].Source
	}

	cam := ccapi.New(host, ccapi.Options{Port: port, Timeout: w.cfg.RequestTimeout})
	info, err := cam.DeviceInfo(ctx)
	if err != nil {
		return nil, nil, err
	}
	return cam, &CameraInfo{
		Host:            host,
		Port:            port,
		Model:           info.ProductName,
		SerialNumber:    info.SerialNumber,
		FirmwareVersion: info.FirmwareVersion,
		Source:          source,
	}, nil
}

// watch catches up, then alternates between long-polling for shots and
// importing them. It returns when the camera goes away or ctx is cancelled.
func (w *Watcher) watch(ctx context.Context, cam *ccapi.Client) error {
	if err := w.sync(ctx, cam, nil); err != nil {
		return err
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		// The poll holds a connection open, so it gets its own context that is
		// cancelled before any download starts.
		pollCtx, cancelPoll := context.WithCancel(ctx)
		type pollResult struct {
			ev  *ccapi.Event
			err error
		}
		results := make(chan pollResult, 1)
		go func() {
			ev, err := cam.PollEvents(pollCtx)
			results <- pollResult{ev, err}
		}()

		select {
		case <-ctx.Done():
			cancelPoll()
			<-results
			return nil

		case <-w.syncNow:
			cancelPoll()
			<-results
			if err := w.sync(ctx, cam, nil); err != nil {
				return err
			}

		case r := <-results:
			cancelPoll()
			if r.err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return r.err
			}
			added := r.ev.AddedFiles()
			if len(added) == 0 {
				continue
			}
			if err := w.sync(ctx, cam, added); err != nil {
				return err
			}
		}
	}
}

// sync imports either the whole card (files == nil) or the named files.
func (w *Watcher) sync(ctx context.Context, cam *ccapi.Client, files []ccapi.File) error {
	w.setState(StateImporting, nil)
	defer func() {
		w.mu.Lock()
		w.status.Progress = nil
		w.mu.Unlock()
		w.setState(StateConnected, nil)
	}()

	var (
		res rpsync.Result
		err error
	)
	if files == nil {
		res, err = w.importer.Pull(ctx, cam)
	} else {
		res, err = w.importer.PullFiles(ctx, cam, files)
	}

	w.mu.Lock()
	w.status.LastSync = time.Now()
	w.status.LastImported = res.Imported
	w.status.TotalImported += res.Imported
	if err != nil {
		w.status.LastError = err.Error()
	} else if len(res.Errors) > 0 {
		w.status.LastError = res.Errors[len(res.Errors)-1].Error()
	}
	w.mu.Unlock()

	if res.Imported > 0 {
		w.bus.Publishf(events.KindImport, "imported %d file(s)", res.Imported)
	}
	if err != nil {
		// A failure here means the camera went away mid-run; the caller
		// reconnects rather than treating it as fatal.
		return err
	}
	return nil
}

// onProgress bridges importer progress into watcher status and the event bus.
func (w *Watcher) onProgress(p rpsync.Progress) {
	switch p.Phase {
	case "importing":
		w.mu.Lock()
		w.status.Progress = &Progress{File: p.File, Done: p.Done, Total: p.Total}
		w.mu.Unlock()
	case "imported":
		w.mu.Lock()
		w.status.Progress = nil
		w.mu.Unlock()
		w.bus.Publish(events.Event{Kind: events.KindImport, Message: "imported " + p.File, Data: p.Entry})
	case "error":
		if p.Err != nil {
			w.bus.Publish(events.Event{Kind: events.KindError, Message: p.Err.Error()})
		}
	}
}

func (w *Watcher) setState(s State, info *CameraInfo) {
	w.mu.Lock()
	changed := w.status.State != s
	w.status.State = s
	if changed {
		w.status.Since = time.Now()
	}
	if info != nil {
		w.status.Camera = info
	}
	w.mu.Unlock()
}

func (w *Watcher) setError(err error) {
	w.mu.Lock()
	w.status.LastError = err.Error()
	w.mu.Unlock()
	w.bus.Publish(events.Event{Kind: events.KindError, Message: err.Error()})
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
