package ccapi_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/cameratest"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/ccapi"
)

func newClient(t *testing.T, cam *cameratest.Camera) *ccapi.Client {
	t.Helper()
	t.Cleanup(cam.Close)
	host, port := cam.HostPort()
	return ccapi.New(host, ccapi.Options{Port: port, Timeout: 5 * time.Second})
}

func TestDeviceInfo(t *testing.T) {
	c := newClient(t, cameratest.New())
	info, err := c.DeviceInfo(context.Background())
	if err != nil {
		t.Fatalf("DeviceInfo: %v", err)
	}
	if info.ProductName != "Canon EOS RP" {
		t.Errorf("ProductName = %q", info.ProductName)
	}
	if info.SerialNumber != "0123456789" {
		t.Errorf("SerialNumber = %q", info.SerialNumber)
	}
}

func TestListFilesAcrossDirectoriesAndPages(t *testing.T) {
	var items []cameratest.Item
	for i := 0; i < cameratest.PageSize+5; i++ {
		items = append(items, cameratest.Item{Name: nameFor(i), Dir: "100CANON"})
	}
	items = append(items, cameratest.Item{Name: "IMG_9001.CR3", Dir: "101CANON"})

	c := newClient(t, cameratest.New(items...))
	files, err := c.ListFiles(context.Background())
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != len(items) {
		t.Fatalf("got %d files, want %d", len(files), len(items))
	}

	byName := map[string]ccapi.File{}
	for _, f := range files {
		byName[f.Name] = f
	}
	cr3, ok := byName["IMG_9001.CR3"]
	if !ok {
		t.Fatal("file in the second directory was not listed")
	}
	if cr3.Card != "sd" || cr3.Dir != "101CANON" {
		t.Errorf("parsed file = %+v", cr3)
	}
	if cr3.Ext() != "cr3" {
		t.Errorf("Ext() = %q", cr3.Ext())
	}
	if cr3.Stem() != "IMG_9001" {
		t.Errorf("Stem() = %q", cr3.Stem())
	}
}

func TestDownloadAndDelete(t *testing.T) {
	cam := cameratest.New(cameratest.Item{Name: "IMG_0001.JPG", Body: []byte("hello-jpeg")})
	c := newClient(t, cam)
	ctx := context.Background()

	files, err := c.ListFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "nested", "IMG_0001.JPG")
	n, err := c.Download(ctx, files[0], dest)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if n != int64(len("hello-jpeg")) {
		t.Errorf("wrote %d bytes, want %d", n, len("hello-jpeg"))
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello-jpeg" {
		t.Errorf("content = %q", got)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error("temporary .part file should be gone after a successful download")
	}

	thumb := filepath.Join(t.TempDir(), "thumb.jpg")
	if _, err := c.DownloadThumbnail(ctx, files[0], thumb); err != nil {
		t.Fatalf("DownloadThumbnail: %v", err)
	}
	if b, _ := os.ReadFile(thumb); string(b) != "thumb:IMG_0001.JPG" {
		t.Errorf("thumbnail = %q", b)
	}

	if err := c.Delete(ctx, files[0]); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if d := cam.Deleted(); len(d) != 1 || d[0] != "sd/100CANON/IMG_0001.JPG" {
		t.Errorf("deleted = %v", d)
	}
}

func TestFileInfo(t *testing.T) {
	captured := time.Date(2024, 5, 4, 10, 30, 0, 0, time.FixedZone("JST", 9*3600))
	cam := cameratest.New(cameratest.Item{Name: "IMG_0002.JPG", Body: []byte("12345"), Captured: captured})
	c := newClient(t, cam)

	files, err := c.ListFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.FileInfo(context.Background(), files[0])
	if err != nil {
		t.Fatalf("FileInfo: %v", err)
	}
	if info.FileSize != 5 {
		t.Errorf("FileSize = %d", info.FileSize)
	}
	if !info.CapturedAt.Equal(captured) {
		t.Errorf("CapturedAt = %s, want %s", info.CapturedAt, captured)
	}
}

func TestPollEventsBlocksUntilShot(t *testing.T) {
	cam := cameratest.New()
	c := newClient(t, cam)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan *ccapi.Event, 1)
	errc := make(chan error, 1)
	go func() {
		ev, err := c.PollEvents(ctx)
		if err != nil {
			errc <- err
			return
		}
		done <- ev
	}()

	// Give the poll a moment to arrive, then "take a shot".
	time.Sleep(50 * time.Millisecond)
	cam.Add(cameratest.Item{Name: "IMG_0003.CR3"})

	select {
	case err := <-errc:
		t.Fatalf("PollEvents: %v", err)
	case ev := <-done:
		files := ev.AddedFiles()
		if len(files) != 1 || files[0].Name != "IMG_0003.CR3" {
			t.Fatalf("added files = %+v", files)
		}
	case <-ctx.Done():
		t.Fatal("PollEvents did not return after a file was added")
	}
}

func TestPollEventsRespectsCancellation(t *testing.T) {
	c := newClient(t, cameratest.New())
	ctx, cancel := context.WithCancel(context.Background())

	errc := make(chan error, 1)
	go func() {
		_, err := c.PollEvents(ctx)
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the context did not end the poll")
	}
}

func TestEndpointVersionNegotiation(t *testing.T) {
	cam := cameratest.New(cameratest.Item{Name: "IMG_0004.JPG"})
	cam.Versions = []string{"ver100", "ver110"}
	cam.ServeVersion = "ver110"
	c := newClient(t, cam)

	// /contents is advertised only under ver110, so the client must not fall
	// back to the ver100 path.
	if _, err := c.ListFiles(context.Background()); err != nil {
		t.Fatalf("ListFiles with negotiated version: %v", err)
	}
}

func TestHTTPErrorCarriesCameraMessage(t *testing.T) {
	cam := cameratest.New()
	cam.Fail("/ccapi/ver110/deviceinformation", 503)
	c := newClient(t, cam)

	_, err := c.DeviceInfo(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	var httpErr *ccapi.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %v, want *ccapi.HTTPError", err)
	}
	if httpErr.StatusCode != 503 {
		t.Errorf("StatusCode = %d", httpErr.StatusCode)
	}
	if httpErr.Message != "forced failure" {
		t.Errorf("Message = %q", httpErr.Message)
	}
}

func nameFor(i int) string {
	const digits = "0123456789"
	return "IMG_" + string([]byte{
		digits[(i/1000)%10], digits[(i/100)%10], digits[(i/10)%10], digits[i%10],
	}) + ".JPG"
}
