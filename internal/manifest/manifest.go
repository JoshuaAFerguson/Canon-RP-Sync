// Package manifest records which camera files have already been imported, so
// repeated runs only pull deltas and a photo handed over by a phone is not
// downloaded a second time from the card.
//
// The manifest is a single JSON file written atomically next to the imported
// photos. It is the daemon's only persistent state about imports, which keeps
// backup and migration a matter of copying one directory.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Version is the on-disk schema version. Version 1 files (which had no version
// field and fewer entry fields) load without conversion.
const Version = 2

// Source describes where an imported file came from.
const (
	SourceCamera = "camera"
	// SourceDevicePrefix is followed by the paired device's name, e.g.
	// "device:Josh's iPad".
	SourceDevicePrefix = "device:"
)

// Entry describes one imported file.
type Entry struct {
	// ID is a stable short identifier derived from the camera-side key. The
	// HTTP API uses it to address photos.
	ID string `json:"id"`

	Card string `json:"card"`
	Dir  string `json:"dir"`
	Name string `json:"name"`

	// Dest is the absolute path the file was written to.
	Dest string `json:"dest"`
	Ext  string `json:"ext"`
	Size int64  `json:"size"`
	// SHA256 is the hex digest of the imported bytes. It lets an upload from a
	// companion app be recognised as a file we already hold, even when the
	// device renamed it.
	SHA256 string `json:"sha256,omitempty"`

	Source     string    `json:"source"`
	CapturedAt time.Time `json:"captured_at,omitempty"`
	ImportedAt time.Time `json:"imported_at"`
}

// Key is the camera-side identity of the file.
func (e Entry) Key() string { return Key(e.Card, e.Dir, e.Name) }

// Key builds the manifest key for a file on a card.
func Key(card, dir, name string) string { return card + "/" + dir + "/" + name }

// IDFor derives the stable public identifier for a camera-side key.
func IDFor(card, dir, name string) string {
	sum := sha256.Sum256([]byte(Key(card, dir, name)))
	return hex.EncodeToString(sum[:])[:16]
}

// Manifest is a JSON-backed set of imported files.
type Manifest struct {
	path string

	mu      sync.RWMutex
	entries map[string]Entry // key -> entry
	byID    map[string]Entry
	byHash  map[string]Entry
}

// file is the on-disk representation.
type file struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

// Load reads the manifest at path, returning an empty one if it does not exist.
func Load(path string) (*Manifest, error) {
	m := &Manifest{
		path:    path,
		entries: map[string]Entry{},
		byID:    map[string]Entry{},
		byHash:  map[string]Entry{},
	}

	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	for key, e := range f.Entries {
		m.entries[key] = upgrade(key, e)
	}
	m.reindexLocked()
	return m, nil
}

// upgrade fills in fields added after schema version 1.
func upgrade(key string, e Entry) Entry {
	if e.Card == "" || e.Name == "" {
		parts := strings.Split(key, "/")
		if len(parts) == 3 {
			e.Card, e.Dir, e.Name = parts[0], parts[1], parts[2]
		}
	}
	if e.ID == "" {
		e.ID = IDFor(e.Card, e.Dir, e.Name)
	}
	if e.Ext == "" {
		e.Ext = strings.ToLower(strings.TrimPrefix(path.Ext(e.Name), "."))
	}
	if e.Source == "" {
		e.Source = SourceCamera
	}
	return e
}

// Path reports where the manifest is stored.
func (m *Manifest) Path() string { return m.path }

// Has reports whether the camera-side file has already been imported.
func (m *Manifest) Has(card, dir, name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.entries[Key(card, dir, name)]
	return ok
}

// Get returns the entry for a camera-side file.
func (m *Manifest) Get(card, dir, name string) (Entry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[Key(card, dir, name)]
	return e, ok
}

// ByID returns the entry with the given public identifier.
func (m *Manifest) ByID(id string) (Entry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.byID[id]
	return e, ok
}

// ByHash returns the entry whose contents match the given hex digest. It is how
// an upload from a companion app is recognised as already imported.
func (m *Manifest) ByHash(sha string) (Entry, bool) {
	if sha == "" {
		return Entry{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.byHash[strings.ToLower(sha)]
	return e, ok
}

// Add records an import and persists the manifest.
func (m *Manifest) Add(e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addLocked(e)
	return m.saveLocked()
}

// AddAll records several imports with a single write.
func (m *Manifest) AddAll(entries []Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		m.addLocked(e)
	}
	return m.saveLocked()
}

func (m *Manifest) addLocked(e Entry) {
	if e.ImportedAt.IsZero() {
		e.ImportedAt = time.Now()
	}
	if e.ID == "" {
		e.ID = IDFor(e.Card, e.Dir, e.Name)
	}
	if e.Ext == "" {
		e.Ext = strings.ToLower(strings.TrimPrefix(path.Ext(e.Name), "."))
	}
	if e.Source == "" {
		e.Source = SourceCamera
	}
	e.SHA256 = strings.ToLower(e.SHA256)

	m.entries[e.Key()] = e
	m.byID[e.ID] = e
	if e.SHA256 != "" {
		m.byHash[e.SHA256] = e
	}
}

// Remove forgets an entry, e.g. when the imported file has been deleted and
// should be pulled again.
func (m *Manifest) Remove(card, dir, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, Key(card, dir, name))
	m.reindexLocked()
	return m.saveLocked()
}

// Len reports how many files have been imported.
func (m *Manifest) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

// Query filters a listing.
type Query struct {
	// Since restricts results to files imported after this time.
	Since time.Time
	// Ext restricts results to a single lower-case extension.
	Ext string
	// Limit caps the number of results. 0 means no cap.
	Limit int
	// Offset skips results, for paging.
	Offset int
}

// List returns entries newest first, by capture time where known and import
// time otherwise.
func (m *Manifest) List(q Query) []Entry {
	m.mu.RLock()
	out := make([]Entry, 0, len(m.entries))
	for _, e := range m.entries {
		if !q.Since.IsZero() && !e.ImportedAt.After(q.Since) {
			continue
		}
		if q.Ext != "" && e.Ext != strings.ToLower(q.Ext) {
			continue
		}
		out = append(out, e)
	}
	m.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		ti, tj := sortTime(out[i]), sortTime(out[j])
		if ti.Equal(tj) {
			return out[i].Name > out[j].Name
		}
		return ti.After(tj)
	})

	if q.Offset > 0 {
		if q.Offset >= len(out) {
			return nil
		}
		out = out[q.Offset:]
	}
	if q.Limit > 0 && q.Limit < len(out) {
		out = out[:q.Limit]
	}
	return out
}

func sortTime(e Entry) time.Time {
	if !e.CapturedAt.IsZero() {
		return e.CapturedAt
	}
	return e.ImportedAt
}

// Stats summarises the manifest for the status endpoint.
type Stats struct {
	Files      int       `json:"files"`
	Bytes      int64     `json:"bytes"`
	LastImport time.Time `json:"last_import,omitempty"`
}

// Stats computes the summary.
func (m *Manifest) Stats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var s Stats
	s.Files = len(m.entries)
	for _, e := range m.entries {
		s.Bytes += e.Size
		if e.ImportedAt.After(s.LastImport) {
			s.LastImport = e.ImportedAt
		}
	}
	return s
}

func (m *Manifest) reindexLocked() {
	m.byID = make(map[string]Entry, len(m.entries))
	m.byHash = make(map[string]Entry, len(m.entries))
	for _, e := range m.entries {
		m.byID[e.ID] = e
		if e.SHA256 != "" {
			m.byHash[e.SHA256] = e
		}
	}
}

// Save writes the manifest to disk.
func (m *Manifest) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveLocked()
}

// saveLocked writes through a temporary file so an interrupted write cannot
// corrupt the ledger.
func (m *Manifest) saveLocked() error {
	if m.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(file{Version: Version, Entries: m.entries}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
