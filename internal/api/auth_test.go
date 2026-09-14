package api_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/api"
)

func newStore(t *testing.T) *api.DeviceStore {
	t.Helper()
	s, err := api.LoadDeviceStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPairingRoundTrip(t *testing.T) {
	s := newStore(t)

	code, expires, err := s.NewPairingCode(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(code, "-") || len(code) != 9 {
		t.Errorf("code = %q, want XXXX-XXXX", code)
	}
	if time.Until(expires) > time.Minute+time.Second {
		t.Errorf("expiry = %s", expires)
	}
	if active, _ := s.PairingActive(); !active {
		t.Error("a fresh code should be active")
	}

	token, device, err := s.Redeem(code, "Josh's iPad", "ios")
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if !strings.HasPrefix(token, "rps_") {
		t.Errorf("token = %q", token)
	}
	if device.TokenHash != "" {
		t.Error("the public device record must not carry the token hash")
	}

	got, ok := s.Authenticate(token)
	if !ok {
		t.Fatal("the issued token should authenticate")
	}
	if got.Name != "Josh's iPad" {
		t.Errorf("Name = %q", got.Name)
	}

	// Codes are single use.
	if _, _, err := s.Redeem(code, "Another", "android"); !errors.Is(err, api.ErrNoPairing) {
		t.Errorf("second redemption err = %v, want ErrNoPairing", err)
	}
}

func TestPairingCodeIsCaseAndFormatInsensitive(t *testing.T) {
	s := newStore(t)
	code, _, err := s.NewPairingCode(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	messy := " " + strings.ToLower(strings.ReplaceAll(code, "-", " ")) + " "
	if _, _, err := s.Redeem(messy, "iPad", "ios"); err != nil {
		t.Fatalf("Redeem(%q): %v", messy, err)
	}
}

func TestPairingRejectsWrongCodeAndLocksOut(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.NewPairingCode(time.Minute); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ {
		if _, _, err := s.Redeem("AAAA-BBBB", "attacker", ""); !errors.Is(err, api.ErrBadCode) {
			t.Fatalf("attempt %d: err = %v, want ErrBadCode", i, err)
		}
	}
	// The fifth failure burns the code entirely.
	if _, _, err := s.Redeem("AAAA-BBBB", "attacker", ""); !errors.Is(err, api.ErrTooManyAttempts) {
		t.Fatalf("err = %v, want ErrTooManyAttempts", err)
	}
	if active, _ := s.PairingActive(); active {
		t.Error("the code should be invalidated after repeated failures")
	}
}

func TestPairingExpires(t *testing.T) {
	s := newStore(t)
	code, _, err := s.NewPairingCode(time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)

	if active, _ := s.PairingActive(); active {
		t.Error("an expired code should not be active")
	}
	if _, _, err := s.Redeem(code, "late", ""); !errors.Is(err, api.ErrCodeExpired) {
		t.Errorf("err = %v, want ErrCodeExpired", err)
	}
}

func TestRedeemWithoutActiveCode(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Redeem("AAAA-BBBB", "device", ""); !errors.Is(err, api.ErrNoPairing) {
		t.Errorf("err = %v, want ErrNoPairing", err)
	}
}

func TestNewCodeInvalidatesPrevious(t *testing.T) {
	s := newStore(t)
	first, _, err := s.NewPairingCode(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.NewPairingCode(time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Redeem(first, "device", ""); !errors.Is(err, api.ErrBadCode) {
		t.Errorf("err = %v, want the old code to stop working", err)
	}
}

func TestRevokeStopsToken(t *testing.T) {
	s := newStore(t)
	code, _, _ := s.NewPairingCode(time.Minute)
	token, device, err := s.Redeem(code, "phone", "android")
	if err != nil {
		t.Fatal(err)
	}

	removed, err := s.Revoke(device.ID)
	if err != nil || !removed {
		t.Fatalf("Revoke = %v, %v", removed, err)
	}
	if _, ok := s.Authenticate(token); ok {
		t.Error("a revoked token must not authenticate")
	}
	if removed, _ := s.Revoke("nope"); removed {
		t.Error("revoking an unknown device should report false")
	}
}

func TestStorePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	s, err := api.LoadDeviceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ := s.NewPairingCode(time.Minute)
	token, _, err := s.Redeem(code, "iPad", "ios")
	if err != nil {
		t.Fatal(err)
	}

	reloaded, err := api.LoadDeviceStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := reloaded.Authenticate(token); !ok {
		t.Error("tokens should survive a daemon restart")
	}
	if len(reloaded.Devices()) != 1 {
		t.Errorf("devices = %d", len(reloaded.Devices()))
	}

	// The code was consumed, so it is no longer redeemable.
	if active, _ := reloaded.PairingActive(); active {
		t.Error("a redeemed code should not still be active")
	}
}

func TestPairingCodeIsSharedBetweenProcesses(t *testing.T) {
	// `rpsync pair` mints the code in one process; the running daemon redeems
	// it in another. Both point at the same state directory.
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.json")

	cli, err := api.LoadDeviceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := api.LoadDeviceStore(path)
	if err != nil {
		t.Fatal(err)
	}

	code, _, err := cli.NewPairingCode(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if active, _ := daemon.PairingActive(); !active {
		t.Fatal("the daemon should see a code minted by the CLI")
	}

	token, _, err := daemon.Redeem(code, "iPad", "ios")
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if _, ok := daemon.Authenticate(token); !ok {
		t.Error("the issued token should authenticate")
	}
	// And the CLI sees the code is spent.
	if active, _ := cli.PairingActive(); active {
		t.Error("the code should be consumed for every process")
	}
}

func TestFailedAttemptsAreSharedBetweenProcesses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.json")
	a, err := api.LoadDeviceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := api.LoadDeviceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.NewPairingCode(time.Minute); err != nil {
		t.Fatal(err)
	}

	// Guessing through two different handles must still hit the same limit.
	for i := 0; i < 4; i++ {
		store := a
		if i%2 == 1 {
			store = b
		}
		if _, _, err := store.Redeem("AAAA-BBBB", "attacker", ""); !errors.Is(err, api.ErrBadCode) {
			t.Fatalf("attempt %d: err = %v", i, err)
		}
	}
	if _, _, err := b.Redeem("AAAA-BBBB", "attacker", ""); !errors.Is(err, api.ErrTooManyAttempts) {
		t.Fatalf("err = %v, want ErrTooManyAttempts", err)
	}
}

func TestRevocationIsSeenByAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.json")

	daemon, err := api.LoadDeviceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ := daemon.NewPairingCode(time.Minute)
	token, device, err := daemon.Redeem(code, "Lost phone", "android")
	if err != nil {
		t.Fatal(err)
	}

	// `rpsync devices revoke` in a separate process.
	cli, err := api.LoadDeviceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := cli.Revoke(device.ID); err != nil || !removed {
		t.Fatalf("Revoke = %v, %v", removed, err)
	}

	if _, ok := daemon.Authenticate(token); ok {
		t.Error("the running daemon should stop accepting a token revoked from the CLI")
	}
}

func TestAuthenticateRejectsUnknownTokens(t *testing.T) {
	s := newStore(t)
	for _, token := range []string{"", "   ", "rps_nope", "Bearer something"} {
		if _, ok := s.Authenticate(token); ok {
			t.Errorf("token %q should not authenticate", token)
		}
	}
}

func TestNormalizeCode(t *testing.T) {
	if got := api.NormalizeCode("ab cd-EF!gh"); got != "ABCDEFGH" {
		t.Errorf("NormalizeCode = %q", got)
	}
	// Ambiguous characters are not in the alphabet and are dropped.
	if got := api.NormalizeCode("O0I1"); got != "" {
		t.Errorf("NormalizeCode = %q, want ambiguous characters dropped", got)
	}
}
