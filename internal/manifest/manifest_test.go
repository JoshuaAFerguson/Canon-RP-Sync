package manifest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/manifest"
)

func newManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Load(filepath.Join(t.TempDir(), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAddHasAndPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "manifest.json")

	m, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Has("sd", "100CANON", "IMG_0001.JPG") {
		t.Fatal("empty manifest should not report a file")
	}

	entry := manifest.Entry{
		Card: "sd", Dir: "100CANON", Name: "IMG_0001.JPG",
		Dest: filepath.Join(dir, "IMG_0001.JPG"), Size: 1234,
		SHA256:     "ABCDEF",
		CapturedAt: time.Date(2024, 5, 4, 10, 0, 0, 0, time.UTC),
	}
	if err := m.Add(entry); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !m.Has("sd", "100CANON", "IMG_0001.JPG") {
		t.Error("Has should report the added file")
	}

	// Reload from disk: the ledger must survive a restart.
	reloaded, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reloaded.Get("sd", "100CANON", "IMG_0001.JPG")
	if !ok {
		t.Fatal("entry missing after reload")
	}
	if got.Size != 1234 || got.Ext != "jpg" || got.Source != manifest.SourceCamera {
		t.Errorf("entry = %+v", got)
	}
	if got.ID == "" || got.ID != manifest.IDFor("sd", "100CANON", "IMG_0001.JPG") {
		t.Errorf("ID = %q", got.ID)
	}
	if _, ok := reloaded.ByID(got.ID); !ok {
		t.Error("ByID should find the entry after reload")
	}
	// Hashes are indexed case-insensitively.
	if _, ok := reloaded.ByHash("abcdef"); !ok {
		t.Error("ByHash should find the entry regardless of case")
	}
}

func TestByHashDedupesUploads(t *testing.T) {
	m := newManifest(t)
	if err := m.Add(manifest.Entry{
		Card: "sd", Dir: "100CANON", Name: "IMG_0100.CR3", SHA256: "deadbeef", Size: 10,
	}); err != nil {
		t.Fatal(err)
	}

	// The same bytes arriving from a phone under a different name.
	if e, ok := m.ByHash("deadbeef"); !ok || e.Name != "IMG_0100.CR3" {
		t.Fatalf("ByHash = %+v, %v", e, ok)
	}
	if _, ok := m.ByHash(""); ok {
		t.Error("an empty hash must never match")
	}
}

func TestListOrdersNewestFirstAndFilters(t *testing.T) {
	m := newManifest(t)
	base := time.Date(2024, 5, 4, 12, 0, 0, 0, time.UTC)
	add := func(name, ext string, captured time.Time) {
		t.Helper()
		if err := m.Add(manifest.Entry{
			Card: "sd", Dir: "100CANON", Name: name, Ext: ext, CapturedAt: captured, Size: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("IMG_0001.JPG", "jpg", base)
	add("IMG_0002.JPG", "jpg", base.Add(time.Hour))
	add("IMG_0003.CR3", "cr3", base.Add(2*time.Hour))

	all := m.List(manifest.Query{})
	if len(all) != 3 {
		t.Fatalf("got %d entries", len(all))
	}
	if all[0].Name != "IMG_0003.CR3" {
		t.Errorf("first entry = %q, want the newest capture", all[0].Name)
	}

	jpgs := m.List(manifest.Query{Ext: "JPG"})
	if len(jpgs) != 2 {
		t.Errorf("ext filter returned %d entries", len(jpgs))
	}

	if page := m.List(manifest.Query{Limit: 2}); len(page) != 2 {
		t.Errorf("limit returned %d entries", len(page))
	}
	page := m.List(manifest.Query{Limit: 2, Offset: 2})
	if len(page) != 1 || page[0].Name != "IMG_0001.JPG" {
		t.Errorf("offset page = %+v", page)
	}
	if over := m.List(manifest.Query{Offset: 99}); over != nil {
		t.Errorf("offset past the end should be empty, got %+v", over)
	}
}

func TestStats(t *testing.T) {
	m := newManifest(t)
	now := time.Now().Truncate(time.Second)
	if err := m.AddAll([]manifest.Entry{
		{Card: "sd", Dir: "100CANON", Name: "a.jpg", Size: 100, ImportedAt: now.Add(-time.Hour)},
		{Card: "sd", Dir: "100CANON", Name: "b.jpg", Size: 250, ImportedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	s := m.Stats()
	if s.Files != 2 || s.Bytes != 350 {
		t.Errorf("stats = %+v", s)
	}
	if !s.LastImport.Equal(now) {
		t.Errorf("LastImport = %s, want %s", s.LastImport, now)
	}
}

func TestRemove(t *testing.T) {
	m := newManifest(t)
	e := manifest.Entry{Card: "sd", Dir: "100CANON", Name: "IMG_1.JPG", SHA256: "aa"}
	if err := m.Add(e); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove("sd", "100CANON", "IMG_1.JPG"); err != nil {
		t.Fatal(err)
	}
	if m.Has("sd", "100CANON", "IMG_1.JPG") {
		t.Error("entry should be gone")
	}
	if _, ok := m.ByHash("aa"); ok {
		t.Error("hash index should be rebuilt on removal")
	}
	if m.Len() != 0 {
		t.Errorf("Len = %d", m.Len())
	}
}

func TestLoadsVersion1File(t *testing.T) {
	// The original scaffold wrote entries without id/ext/source fields.
	path := filepath.Join(t.TempDir(), "manifest.json")
	legacy := map[string]any{
		"entries": map[string]any{
			"sd/100CANON/IMG_0009.JPG": map[string]any{
				"card":        "sd",
				"dir":         "100CANON",
				"name":        "IMG_0009.JPG",
				"dest":        "/photos/2024/2024-05-04/IMG_0009.JPG",
				"imported_at": "2024-05-04T12:00:00Z",
			},
		},
	}
	b, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := manifest.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !m.Has("sd", "100CANON", "IMG_0009.JPG") {
		t.Fatal("legacy entry not loaded")
	}
	e, _ := m.Get("sd", "100CANON", "IMG_0009.JPG")
	if e.ID == "" {
		t.Error("ID should be derived for legacy entries")
	}
	if e.Ext != "jpg" {
		t.Errorf("Ext = %q, want it derived from the name", e.Ext)
	}
	if e.Source != manifest.SourceCamera {
		t.Errorf("Source = %q, want %q", e.Source, manifest.SourceCamera)
	}
}

func TestLoadRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Load(path); err == nil {
		t.Fatal("a corrupt manifest must be reported, not silently discarded")
	}
}
