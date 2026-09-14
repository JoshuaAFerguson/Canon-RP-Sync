package watcher_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/cameratest"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/events"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/manifest"
	rpsync "github.com/JoshuaAFerguson/canon-rp-sync/internal/sync"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/watcher"
)

func newWatcher(t *testing.T, cam *cameratest.Camera) (*watcher.Watcher, string, *manifest.Manifest) {
	t.Helper()

	dest := t.TempDir()
	mf, err := manifest.Load(filepath.Join(dest, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	im := rpsync.New(mf, rpsync.Options{Dest: dest, DateSource: rpsync.DateFromImport})

	host, port := cam.HostPort()
	w := watcher.New(watcher.Config{
		Host:           host,
		Port:           port,
		PollInterval:   100 * time.Millisecond,
		RequestTimeout: 5 * time.Second,
	}, im, events.NewBus())
	return w, dest, mf
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWatcherCatchesUpThenImportsNewShots(t *testing.T) {
	cam := cameratest.New(cameratest.Item{Name: "IMG_0001.JPG", Body: []byte("existing")})
	defer cam.Close()

	w, dest, mf := newWatcher(t, cam)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// The backlog on the card is imported on connect.
	waitFor(t, "the existing shot to be imported", func() bool { return mf.Len() == 1 })
	waitFor(t, "the watcher to report a connected camera", func() bool {
		return w.Status().State == "connected" && w.Status().Camera != nil
	})
	if got := w.Status().Camera.Model; got != "Canon EOS RP" {
		t.Errorf("camera model = %q", got)
	}

	// A new frame arrives while the watcher is long-polling.
	cam.Add(cameratest.Item{Name: "IMG_0002.CR3", Body: []byte("fresh")})
	waitFor(t, "the new shot to be imported", func() bool { return mf.Len() == 2 })

	e, ok := mf.Get("sd", "100CANON", "IMG_0002.CR3")
	if !ok {
		t.Fatal("new shot missing from the manifest")
	}
	if b, err := os.ReadFile(e.Dest); err != nil || string(b) != "fresh" {
		t.Errorf("imported bytes = %q, err = %v", b, err)
	}
	if !strings.HasPrefix(e.Dest, dest) {
		t.Errorf("file written outside dest: %s", e.Dest)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop when the context was cancelled")
	}
	if w.Status().State != "stopped" {
		t.Errorf("final state = %q", w.Status().State)
	}
}

func TestSyncNowInterruptsThePoll(t *testing.T) {
	cam := cameratest.New()
	defer cam.Close()

	w, _, mf := newWatcher(t, cam)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	waitFor(t, "the watcher to connect", func() bool { return w.Status().State == "connected" })

	// A new shot arrives and is imported through the event path.
	cam.Add(cameratest.Item{Name: "IMG_0100.JPG"})
	waitFor(t, "the event-driven import", func() bool { return mf.Len() == 1 })

	// An explicit sync request must be honoured even with nothing new.
	w.SyncNow()
	waitFor(t, "the manual sync to complete", func() bool { return !w.Status().LastSync.IsZero() })
}

func TestWatcherRetriesWhenCameraIsAway(t *testing.T) {
	cam := cameratest.New()
	host, port := cam.HostPort()
	cam.Close() // camera is off before the watcher ever starts

	dest := t.TempDir()
	mf, err := manifest.Load(filepath.Join(dest, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	im := rpsync.New(mf, rpsync.Options{Dest: dest, DateSource: rpsync.DateFromImport})
	bus := events.NewBus()
	w := watcher.New(watcher.Config{
		Host: host, Port: port,
		PollInterval:   50 * time.Millisecond,
		RequestTimeout: time.Second,
	}, im, bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	waitFor(t, "an error to be reported for the missing camera", func() bool {
		return w.Status().LastError != ""
	})
	if state := w.Status().State; state != "searching" {
		t.Errorf("state = %q, want searching", state)
	}

	// It must keep retrying rather than exiting.
	time.Sleep(200 * time.Millisecond)
	if w.Status().State == "stopped" {
		t.Error("watcher gave up instead of retrying")
	}
}

func TestWatcherPublishesEvents(t *testing.T) {
	cam := cameratest.New(cameratest.Item{Name: "IMG_0200.JPG"})
	defer cam.Close()

	w, _, _ := newWatcher(t, cam)
	ch, unsubscribe := w.Bus().Subscribe(64)
	defer unsubscribe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	deadline := time.After(5 * time.Second)
	var sawCamera, sawImport bool
	for !sawCamera || !sawImport {
		select {
		case ev := <-ch:
			switch ev.Kind {
			case events.KindCamera:
				sawCamera = true
			case events.KindImport:
				sawImport = true
			}
		case <-deadline:
			t.Fatalf("missing events (camera=%v import=%v)", sawCamera, sawImport)
		}
	}
}

func TestWatcherRequiresHostWhenDiscoveryDisabled(t *testing.T) {
	dest := t.TempDir()
	mf, err := manifest.Load(filepath.Join(dest, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	im := rpsync.New(mf, rpsync.Options{Dest: dest})
	w := watcher.New(watcher.Config{Discovery: false, PollInterval: 50 * time.Millisecond}, im, events.NewBus())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if w.Status().LastError == "" {
		t.Error("want an error explaining that no host is configured")
	}
}
