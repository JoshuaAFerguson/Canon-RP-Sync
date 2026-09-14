package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Pairing errors returned to callers of Redeem.
var (
	// ErrNoPairing means no pairing code is currently active.
	ErrNoPairing = errors.New("no pairing code is active")
	// ErrBadCode means the supplied code did not match the active one.
	ErrBadCode = errors.New("pairing code is incorrect")
	// ErrCodeExpired means the code was valid but has timed out.
	ErrCodeExpired = errors.New("pairing code has expired")
	// ErrTooManyAttempts means pairing has been locked after repeated failures.
	ErrTooManyAttempts = errors.New("too many incorrect pairing attempts; generate a new code")
)

// DefaultPairingTTL is how long a pairing code stays valid.
const DefaultPairingTTL = 10 * time.Minute

// maxPairingAttempts is how many wrong guesses burn the active code. The
// pairing endpoint is the one route reachable without a token, and a tunnel can
// put it on the public internet, so a wrong code is cheap but guessing is not.
const maxPairingAttempts = 5

// codeAlphabet omits characters that are easy to confuse when read off a screen.
const codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// Device is a paired client: a phone, a tablet, or a browser.
type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform,omitempty"`
	// TokenHash is the SHA-256 of the bearer token. The token itself is shown
	// once, at pairing time, and never stored.
	TokenHash string    `json:"token_hash"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
}

// Public returns the device without its credential material.
func (d Device) Public() Device {
	d.TokenHash = ""
	return d
}

// DeviceStore holds paired devices and the active pairing code.
//
// Both live on disk with owner-only permissions, which is what lets
// `rpsync pair` mint a code in one process and the running daemon redeem it in
// another — and lets `rpsync devices revoke` take effect in a daemon that is
// already running.
type DeviceStore struct {
	path        string
	pairingPath string

	mu      sync.RWMutex
	devices map[string]Device // id -> device
	byHash  map[string]string // token hash -> device id
	// modTime is the devices file as last read, so an external change is
	// noticed without re-reading the file on every request.
	modTime time.Time
}

// pairingCode is the on-disk pairing state, shared between processes.
type pairingCode struct {
	Hash      string    `json:"hash"`
	ExpiresAt time.Time `json:"expires_at"`
	Attempts  int       `json:"attempts"`
}

type storeFile struct {
	Devices map[string]Device `json:"devices"`
}

// LoadDeviceStore opens (or creates) the device store at path.
func LoadDeviceStore(path string) (*DeviceStore, error) {
	s := &DeviceStore{
		path:        path,
		pairingPath: filepath.Join(filepath.Dir(path), "pairing.json"),
		devices:     map[string]Device{},
		byHash:      map[string]string{},
	}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// reload reads the devices file, replacing the in-memory view.
func (s *DeviceStore) reload() error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read device store: %w", err)
	}
	var f storeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("parse device store %s: %w", s.path, err)
	}

	devices := make(map[string]Device, len(f.Devices))
	byHash := make(map[string]string, len(f.Devices))
	for id, d := range f.Devices {
		d.ID = id
		devices[id] = d
		if d.TokenHash != "" {
			byHash[d.TokenHash] = id
		}
	}

	s.mu.Lock()
	s.devices = devices
	s.byHash = byHash
	if info, err := os.Stat(s.path); err == nil {
		s.modTime = info.ModTime()
	}
	s.mu.Unlock()
	return nil
}

// refresh re-reads the devices file when another process has changed it, so a
// token revoked by the CLI stops working in the running daemon.
func (s *DeviceStore) refresh() {
	info, err := os.Stat(s.path)
	if err != nil {
		return
	}
	s.mu.RLock()
	unchanged := info.ModTime().Equal(s.modTime)
	s.mu.RUnlock()
	if unchanged {
		return
	}
	_ = s.reload()
}

// Devices lists paired devices, newest first, without credential material.
func (s *DeviceStore) Devices() []Device {
	s.refresh()

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d.Public())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Authenticate resolves a bearer token to a device and records the sighting.
func (s *DeviceStore) Authenticate(token string) (Device, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Device{}, false
	}
	s.refresh()
	hash := hashToken(token)

	s.mu.Lock()
	id, ok := s.byHash[hash]
	if !ok {
		s.mu.Unlock()
		return Device{}, false
	}
	d := s.devices[id]
	d.LastSeen = time.Now()
	s.devices[id] = d
	s.mu.Unlock()

	// A failed write here must not deny access; the timestamp is cosmetic.
	_ = s.Save()
	return d.Public(), true
}

// Revoke removes a device. Its token stops working immediately.
func (s *DeviceStore) Revoke(id string) (bool, error) {
	s.refresh()

	s.mu.Lock()
	d, ok := s.devices[id]
	if ok {
		delete(s.devices, id)
		delete(s.byHash, d.TokenHash)
	}
	s.mu.Unlock()

	if !ok {
		return false, nil
	}
	return true, s.Save()
}

// NewPairingCode generates a short code a device can exchange for a token. Only
// one code is active at a time; generating a new one invalidates the previous.
//
// The code is written to disk (hashed) so that `rpsync pair` on the host and
// the running daemon are talking about the same code.
func (s *DeviceStore) NewPairingCode(ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 {
		ttl = DefaultPairingTTL
	}
	code, err := randomCode(8)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := time.Now().Add(ttl)

	if err := s.writePairing(&pairingCode{Hash: hashToken(code), ExpiresAt: expires}); err != nil {
		return "", time.Time{}, err
	}
	return FormatCode(code), expires, nil
}

// PairingActive reports whether a code is currently redeemable, and when it
// expires.
func (s *DeviceStore) PairingActive() (bool, time.Time) {
	pc, err := s.readPairing()
	if err != nil || pc == nil || time.Now().After(pc.ExpiresAt) {
		return false, time.Time{}
	}
	return true, pc.ExpiresAt
}

// CancelPairing invalidates the active code.
func (s *DeviceStore) CancelPairing() {
	s.clearPairing()
}

// Redeem exchanges a pairing code for a device token. The code is single-use,
// and is destroyed after a handful of wrong guesses.
func (s *DeviceStore) Redeem(code, name, platform string) (string, Device, error) {
	code = NormalizeCode(code)
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Unnamed device"
	}

	pc, err := s.readPairing()
	if err != nil {
		return "", Device{}, err
	}
	switch {
	case pc == nil:
		return "", Device{}, ErrNoPairing
	case time.Now().After(pc.ExpiresAt):
		s.clearPairing()
		return "", Device{}, ErrCodeExpired
	case pc.Attempts >= maxPairingAttempts:
		s.clearPairing()
		return "", Device{}, ErrTooManyAttempts
	}

	if subtle.ConstantTimeCompare([]byte(hashToken(code)), []byte(pc.Hash)) != 1 {
		pc.Attempts++
		if pc.Attempts >= maxPairingAttempts {
			s.clearPairing()
			return "", Device{}, ErrTooManyAttempts
		}
		if err := s.writePairing(pc); err != nil {
			return "", Device{}, err
		}
		return "", Device{}, ErrBadCode
	}

	token, err := randomToken()
	if err != nil {
		return "", Device{}, err
	}
	device := Device{
		ID:        newID(),
		Name:      name,
		Platform:  strings.TrimSpace(platform),
		TokenHash: hashToken(token),
		CreatedAt: time.Now(),
	}

	s.refresh() // do not drop devices another process added
	s.mu.Lock()
	s.devices[device.ID] = device
	s.byHash[device.TokenHash] = device.ID
	s.mu.Unlock()

	s.clearPairing() // single use
	if err := s.Save(); err != nil {
		return "", Device{}, err
	}
	return token, device.Public(), nil
}

// readPairing loads the shared pairing state, treating a missing or unreadable
// file as "no code active".
func (s *DeviceStore) readPairing() (*pairingCode, error) {
	if s.pairingPath == "" {
		return nil, nil
	}
	b, err := os.ReadFile(s.pairingPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pairing state: %w", err)
	}
	var pc pairingCode
	if err := json.Unmarshal(b, &pc); err != nil {
		// A corrupt file means no usable code, not a broken daemon.
		s.clearPairing()
		return nil, nil
	}
	return &pc, nil
}

func (s *DeviceStore) writePairing(pc *pairingCode) error {
	if s.pairingPath == "" {
		return nil
	}
	b, err := json.Marshal(pc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.pairingPath), 0o700); err != nil {
		return err
	}
	tmp := s.pairingPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.pairingPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (s *DeviceStore) clearPairing() {
	if s.pairingPath != "" {
		_ = os.Remove(s.pairingPath)
	}
}

// Save persists the store.
func (s *DeviceStore) Save() error {
	if s.path == "" {
		return nil
	}
	s.mu.RLock()
	b, err := json.MarshalIndent(storeFile{Devices: s.devices}, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	if info, err := os.Stat(s.path); err == nil {
		s.mu.Lock()
		s.modTime = info.ModTime()
		s.mu.Unlock()
	}
	return nil
}

// FormatCode renders a raw code as XXXX-XXXX for reading aloud.
func FormatCode(code string) string {
	if len(code) != 8 {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// NormalizeCode strips formatting so "abcd-efgh" and "ABCDEFGH" both work.
func NormalizeCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(code)) {
		if strings.ContainsRune(codeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func hashToken(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "rps_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func randomCode(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = codeAlphabet[int(b)%len(codeAlphabet)]
	}
	return string(out), nil
}

func newID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("dev-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
