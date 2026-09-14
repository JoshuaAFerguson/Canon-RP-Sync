package sync_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/cameratest"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/ccapi"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/manifest"
	rpsync "github.com/JoshuaAFerguson/canon-rp-sync/internal/sync"
)

type harness struct {
	cam      *cameratest.Camera
	client   *ccapi.Client
	importer *rpsync.Importer
	mf       *manifest.Manifest
	dest     string
}

func newHarness(t *testing.T, opt rpsync.Options, items ...cameratest.Item) *harness {
	t.Helper()

	cam := cameratest.New(items...)
	t.Cleanup(cam.Close)
	host, port := cam.HostPort()

	dest := t.TempDir()
	if opt.Dest == "" {
		opt.Dest = dest
	}
	mf, err := manifest.Load(filepath.Join(opt.Dest, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &harness{
		cam:      cam,
		client:   ccapi.New(host, ccapi.Options{Port: port, Timeout: 5 * time.Second}),
		importer: rpsync.New(mf, opt),
		mf:       mf,
		dest:     opt.Dest,
	}
}

func TestPullImportsAndSkipsAlreadyImported(t *testing.T) {
	h := newHarness(t, rpsync.Options{
		Include:    map[string]bool{"jpg": true, "cr3": true},
		DateSource: rpsync.DateFromImport,
	},
		cameratest.Item{Name: "IMG_0001.JPG", Body: []byte("one")},
		cameratest.Item{Name: "IMG_0002.CR3", Body: []byte("two")},
		cameratest.Item{Name: "MVI_0003.MP4", Body: []byte("movie")}, // excluded
	)

	res, err := h.importer.Pull(context.Background(), h.client)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Imported != 2 {
		t.Errorf("Imported = %d, want 2", res.Imported)
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want the excluded movie", res.Skipped)
	}
	if res.Bytes != int64(len("one")+len("two")) {
		t.Errorf("Bytes = %d", res.Bytes)
	}

	// Files land under the dated layout with their original names.
	day := time.Now().Format("2006/2006-01-02")
	for _, name := range []string{"IMG_0001.JPG", "IMG_0002.CR3"} {
		p := filepath.Join(h.dest, filepath.FromSlash(day), name)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s: %v", p, err)
		}
	}

	// A second run must be a no-op.
	res2, err := h.importer.Pull(context.Background(), h.client)
	if err != nil {
		t.Fatalf("second Pull: %v", err)
	}
	if res2.Imported != 0 {
		t.Errorf("second run imported %d files, want 0", res2.Imported)
	}
	if h.mf.Len() != 2 {
		t.Errorf("manifest has %d entries", h.mf.Len())
	}
}

func TestPullFilesByCaptureDate(t *testing.T) {
	captured := time.Date(2023, 7, 14, 9, 15, 0, 0, time.UTC)
	h := newHarness(t, rpsync.Options{DateSource: rpsync.DateFromEXIF},
		// No readable EXIF, so the camera's own timestamp decides the folder.
		cameratest.Item{Name: "IMG_0010.JPG", Body: []byte("bytes"), Captured: captured},
	)

	if _, err := h.importer.Pull(context.Background(), h.client); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	want := filepath.Join(h.dest, "2023", "2023-07-14", "IMG_0010.JPG")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected the file under its capture date at %s: %v", want, err)
	}

	e, ok := h.mf.Get("sd", "100CANON", "IMG_0010.JPG")
	if !ok {
		t.Fatal("entry missing")
	}
	if !e.CapturedAt.Equal(captured) {
		t.Errorf("CapturedAt = %s, want %s", e.CapturedAt, captured)
	}
	if e.SHA256 == "" {
		t.Error("imports should be hashed")
	}
	if e.ID == "" {
		t.Error("imported entries should carry an ID")
	}
}

func TestPullCustomLayout(t *testing.T) {
	captured := time.Date(2022, 12, 25, 8, 0, 0, 0, time.UTC)
	h := newHarness(t, rpsync.Options{Layout: "2006-01", DateSource: rpsync.DateFromEXIF},
		cameratest.Item{Name: "IMG_0020.JPG", Captured: captured},
	)
	if _, err := h.importer.Pull(context.Background(), h.client); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.dest, "2022-12", "IMG_0020.JPG")); err != nil {
		t.Errorf("custom layout not honoured: %v", err)
	}
}

func TestDeleteAfterImportIsOptIn(t *testing.T) {
	h := newHarness(t, rpsync.Options{DateSource: rpsync.DateFromImport},
		cameratest.Item{Name: "IMG_0030.JPG"},
	)
	if _, err := h.importer.Pull(context.Background(), h.client); err != nil {
		t.Fatal(err)
	}
	if got := h.cam.Deleted(); len(got) != 0 {
		t.Fatalf("card was modified without opt-in: %v", got)
	}

	h2 := newHarness(t, rpsync.Options{DateSource: rpsync.DateFromImport, DeleteAfterImport: true},
		cameratest.Item{Name: "IMG_0031.JPG"},
	)
	if _, err := h2.importer.Pull(context.Background(), h2.client); err != nil {
		t.Fatal(err)
	}
	if got := h2.cam.Deleted(); len(got) != 1 {
		t.Fatalf("delete_after_import did not delete: %v", got)
	}
}

func TestPullReportsProgress(t *testing.T) {
	h := newHarness(t, rpsync.Options{DateSource: rpsync.DateFromImport},
		cameratest.Item{Name: "IMG_0040.JPG"},
	)
	var phases []string
	h.importer.Notify = func(p rpsync.Progress) { phases = append(phases, p.Phase) }

	if _, err := h.importer.Pull(context.Background(), h.client); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(phases, ",")
	for _, want := range []string{"listing", "importing", "imported"} {
		if !strings.Contains(joined, want) {
			t.Errorf("progress %q missing phase %q", joined, want)
		}
	}
}

func TestNameCollisionKeepsBothPhotos(t *testing.T) {
	h := newHarness(t, rpsync.Options{DateSource: rpsync.DateFromImport},
		cameratest.Item{Name: "IMG_0001.JPG", Dir: "100CANON", Body: []byte("first")},
		cameratest.Item{Name: "IMG_0001.JPG", Dir: "101CANON", Body: []byte("second")},
	)
	res, err := h.importer.Pull(context.Background(), h.client)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 2 {
		t.Fatalf("Imported = %d, want both files", res.Imported)
	}

	day := filepath.Join(h.dest, filepath.FromSlash(time.Now().Format("2006/2006-01-02")))
	first, err := os.ReadFile(filepath.Join(day, "IMG_0001.JPG"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(day, "IMG_0001_1.JPG"))
	if err != nil {
		t.Fatalf("the colliding file should be kept under a suffixed name: %v", err)
	}
	if string(first) == string(second) {
		t.Error("collision resolution overwrote one of the photos")
	}
}

func TestIdenticalFileAtDestinationIsReused(t *testing.T) {
	h := newHarness(t, rpsync.Options{DateSource: rpsync.DateFromImport},
		cameratest.Item{Name: "IMG_0050.JPG", Body: []byte("same-bytes")},
	)
	day := filepath.Join(h.dest, filepath.FromSlash(time.Now().Format("2006/2006-01-02")))
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pretend a previous run wrote the file but lost its manifest.
	if err := os.WriteFile(filepath.Join(day, "IMG_0050.JPG"), []byte("same-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := h.importer.Pull(context.Background(), h.client); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(day)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("identical content should not produce a duplicate file: %v", names(entries))
	}
}

func TestStagingIsCleanedUp(t *testing.T) {
	h := newHarness(t, rpsync.Options{DateSource: rpsync.DateFromImport},
		cameratest.Item{Name: "IMG_0060.JPG"},
	)
	if _, err := h.importer.Pull(context.Background(), h.client); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(h.dest, ".rpsync-incoming")
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatalf("staging dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("staging left behind %v", names(entries))
	}
}

func TestPullStopsOnCancellation(t *testing.T) {
	var items []cameratest.Item
	for i := 0; i < 5; i++ {
		items = append(items, cameratest.Item{Name: "IMG_010" + string(rune('0'+i)) + ".JPG"})
	}
	h := newHarness(t, rpsync.Options{DateSource: rpsync.DateFromImport}, items...)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.importer.Pull(ctx, h.client); err == nil {
		t.Fatal("a cancelled context should stop the run")
	}
}

func TestIngestFromCompanionApp(t *testing.T) {
	h := newHarness(t, rpsync.Options{DateSource: rpsync.DateFromImport})
	captured := time.Date(2024, 8, 1, 14, 0, 0, 0, time.UTC)

	entry, imported, err := h.importer.Ingest(context.Background(),
		bytes.NewReader([]byte("from-ipad")),
		rpsync.IngestMeta{Name: "IMG_0500.CR3", Card: "sd", Dir: "100CANON",
			CapturedAt: captured, Device: "Josh's iPad"},
	)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !imported {
		t.Fatal("the first upload should be imported")
	}
	if entry.Source != "device:Josh's iPad" {
		t.Errorf("Source = %q", entry.Source)
	}
	// Clients address photos by id, so the returned entry must carry one.
	if entry.ID == "" {
		t.Error("the returned entry has no ID")
	}
	if _, ok := h.mf.ByID(entry.ID); !ok {
		t.Errorf("ID %q does not resolve in the manifest", entry.ID)
	}
	if b, err := os.ReadFile(entry.Dest); err != nil || string(b) != "from-ipad" {
		t.Errorf("uploaded bytes = %q, err = %v", b, err)
	}

	// Uploading the same file again is a no-op.
	_, imported, err = h.importer.Ingest(context.Background(),
		bytes.NewReader([]byte("from-ipad")),
		rpsync.IngestMeta{Name: "IMG_0500.CR3", Card: "sd", Dir: "100CANON", Device: "Josh's iPad"})
	if err != nil {
		t.Fatal(err)
	}
	if imported {
		t.Error("re-uploading the same file should be a no-op")
	}

	// The same bytes under a different name are recognised by content hash.
	_, imported, err = h.importer.Ingest(context.Background(),
		bytes.NewReader([]byte("from-ipad")),
		rpsync.IngestMeta{Name: "renamed.CR3", Device: "Josh's iPad"})
	if err != nil {
		t.Fatal(err)
	}
	if imported {
		t.Error("content-identical upload should be deduplicated")
	}
}

func TestIngestThenPullDoesNotDownloadAgain(t *testing.T) {
	h := newHarness(t, rpsync.Options{DateSource: rpsync.DateFromImport},
		cameratest.Item{Name: "IMG_0600.JPG", Body: []byte("shot")},
	)
	// The phone got there first, over the camera's hotspot.
	if _, _, err := h.importer.Ingest(context.Background(), bytes.NewReader([]byte("shot")),
		rpsync.IngestMeta{Name: "IMG_0600.JPG", Card: "sd", Dir: "100CANON", Device: "iPad"}); err != nil {
		t.Fatal(err)
	}

	res, err := h.importer.Pull(context.Background(), h.client)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 0 {
		t.Errorf("Imported = %d, want 0: the daemon should not re-download a handed-over shot", res.Imported)
	}
}

func TestIngestRejectsMissingName(t *testing.T) {
	h := newHarness(t, rpsync.Options{})
	if _, _, err := h.importer.Ingest(context.Background(), bytes.NewReader(nil), rpsync.IngestMeta{}); err == nil {
		t.Fatal("want an error for a nameless upload")
	}
}

func TestIngestSanitizesPathTraversal(t *testing.T) {
	h := newHarness(t, rpsync.Options{DateSource: rpsync.DateFromImport})
	entry, _, err := h.importer.Ingest(context.Background(), bytes.NewReader([]byte("x")),
		rpsync.IngestMeta{Name: "../../../etc/passwd", Device: "phone"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(entry.Dest, h.dest) {
		t.Fatalf("upload escaped the destination tree: %s", entry.Dest)
	}
	if filepath.Base(entry.Dest) != "passwd" {
		t.Errorf("name = %q", filepath.Base(entry.Dest))
	}
}

func TestCleanStaging(t *testing.T) {
	dest := t.TempDir()
	staging := filepath.Join(dest, ".rpsync-incoming")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "leftover.part"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rpsync.CleanStaging(dest); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("leftovers remain: %v", names(entries))
	}
	// Absent directory is fine.
	if err := rpsync.CleanStaging(t.TempDir()); err != nil {
		t.Errorf("CleanStaging on a fresh dest: %v", err)
	}
}

func names(entries []os.DirEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
