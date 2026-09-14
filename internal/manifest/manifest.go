// Package manifest records which camera files have already been imported so
// repeated runs only pull deltas.
package manifest

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// Entry describes one imported file.
type Entry struct {
	Card       string    `json:"card"`
	Dir        string    `json:"dir"`
	Name       string    `json:"name"`
	Dest       string    `json:"dest"`
	ImportedAt time.Time `json:"imported_at"`
}

// Manifest is a JSON-backed set of imported files keyed by card/dir/name.
type Manifest struct {
	path    string
	mu      sync.Mutex
	Entries map[string]Entry `json:"entries"`
}

// Load reads the manifest at path, returning an empty one if it doesn't exist.
func Load(path string) (*Manifest, error) {
	m := &Manifest{path: path, Entries: map[string]Entry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, err
	}
	return m, nil
}

func key(card, dir, name string) string { return card + "/" + dir + "/" + name }

// Has reports whether the file has already been imported.
func (m *Manifest) Has(card, dir, name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.Entries[key(card, dir, name)]
	return ok
}

// Add records an import and persists the manifest.
func (m *Manifest) Add(e Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.ImportedAt = time.Now()
	m.Entries[key(e.Card, e.Dir, e.Name)] = e
	return m.saveLocked()
}

func (m *Manifest) saveLocked() error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}
