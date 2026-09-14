// Package api serves rpsync's HTTP interface: the web UI, and the endpoints
// the iPad and Android companion apps use to pair, hand over photos and browse
// what has been imported.
//
// Every route except /api/v1/health and /api/v1/pair requires a bearer token
// issued by pairing, because this server is meant to be reachable from outside
// the LAN over Tailscale or a Cloudflare Tunnel.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/ccapi"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/events"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/manifest"
	rpsync "github.com/JoshuaAFerguson/canon-rp-sync/internal/sync"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/watcher"
)

// tokenCookie is the cookie the web UI authenticates with after pairing. It is
// HttpOnly so page scripts cannot read it, which also makes EventSource and
// <img> requests authenticate without exposing the token to the page.
const tokenCookie = "rpsync_token"

// CameraSource is the part of the watcher the API depends on.
type CameraSource interface {
	Status() watcher.Status
	SyncNow()
	Client() *ccapi.Client
}

// RemoteStatus describes how the daemon can be reached from outside the LAN.
// It is supplied as a function so the API does not depend on the tunnel
// implementation.
type RemoteStatus struct {
	Mode    string `json:"mode"`
	State   string `json:"state"`
	URL     string `json:"url,omitempty"`
	Message string `json:"message,omitempty"`
}

// Config wires the server to the rest of the daemon.
type Config struct {
	Devices  *DeviceStore
	Manifest *manifest.Manifest
	Importer *rpsync.Importer
	Camera   CameraSource
	Bus      *events.Bus
	Logger   *slog.Logger

	// ThumbDir caches generated thumbnails.
	ThumbDir string
	// MaxUploadBytes caps a single companion-app upload.
	MaxUploadBytes int64
	// AllowLocalUnauthenticated skips auth for loopback requests.
	AllowLocalUnauthenticated bool
	// AdvertiseURL is the base URL handed to devices at pairing time.
	AdvertiseURL string
	// Version is reported by /api/v1/health.
	Version string
	// Remote reports tunnel state; may be nil.
	Remote func() RemoteStatus
}

// Server is the HTTP interface.
type Server struct {
	cfg Config
	log *slog.Logger
	mux *http.ServeMux
}

// New builds the server and its routes.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = 2 << 30
	}
	if cfg.Bus == nil {
		cfg.Bus = events.NewBus()
	}

	s := &Server{cfg: cfg, log: cfg.Logger, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	// Open routes. /health carries nothing sensitive; /pair is rate-limited by
	// the device store and burns its code after a handful of wrong guesses.
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("POST /api/v1/pair", s.handlePair)

	// Authenticated API.
	s.mux.Handle("GET /api/v1/status", s.auth(http.HandlerFunc(s.handleStatus)))
	s.mux.Handle("GET /api/v1/events", s.auth(http.HandlerFunc(s.handleEvents)))
	s.mux.Handle("GET /api/v1/photos", s.auth(http.HandlerFunc(s.handlePhotos)))
	s.mux.Handle("GET /api/v1/photos/{id}", s.auth(http.HandlerFunc(s.handlePhoto)))
	s.mux.Handle("GET /api/v1/photos/{id}/file", s.auth(http.HandlerFunc(s.handlePhotoFile)))
	s.mux.Handle("GET /api/v1/photos/{id}/thumb", s.auth(http.HandlerFunc(s.handlePhotoThumb)))
	s.mux.Handle("POST /api/v1/ingest", s.auth(http.HandlerFunc(s.handleIngest)))
	s.mux.Handle("POST /api/v1/sync", s.auth(http.HandlerFunc(s.handleSync)))
	s.mux.Handle("GET /api/v1/devices", s.auth(http.HandlerFunc(s.handleDevices)))
	s.mux.Handle("DELETE /api/v1/devices/{id}", s.auth(http.HandlerFunc(s.handleRevokeDevice)))
	// Minting a code requires an already-paired device; the first code comes
	// from `rpsync pair` on the host, or from the daemon's startup log.
	s.mux.Handle("POST /api/v1/pair/code", s.auth(http.HandlerFunc(s.handleNewPairingCode)))
	s.mux.Handle("DELETE /api/v1/pair/code", s.auth(http.HandlerFunc(s.handleCancelPairingCode)))

	// Web UI.
	s.mux.Handle("GET /", s.webUI())
}

// Handler returns the server's http.Handler with middleware applied.
func (s *Server) Handler() http.Handler {
	return s.recoverPanics(s.secureHeaders(s.mux))
}

// Serve runs the HTTP server until ctx is cancelled, then shuts it down.
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
		// Uploads from a phone over a slow link can take a while, so there is
		// no write timeout; the read header timeout still guards slowloris.
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	s.log.Info("http server listening", "addr", ln.Addr().String())

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// ---------------------------------------------------------------- middleware

type contextKey string

const deviceContextKey contextKey = "device"

// DeviceFrom returns the authenticated device for a request.
func DeviceFrom(ctx context.Context) (Device, bool) {
	d, ok := ctx.Value(deviceContextKey).(Device)
	return d, ok
}

// auth requires a valid device token, or a loopback request when the daemon is
// configured to trust the local machine.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token := extractToken(r); token != "" {
			if device, ok := s.cfg.Devices.Authenticate(token); ok {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deviceContextKey, device)))
				return
			}
			s.writeError(w, http.StatusUnauthorized, "invalid or revoked token")
			return
		}

		if s.cfg.AllowLocalUnauthenticated && isLoopback(r) {
			local := Device{ID: "local", Name: "This computer", Platform: "local"}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deviceContextKey, local)))
			return
		}

		w.Header().Set("WWW-Authenticate", `Bearer realm="rpsync"`)
		s.writeError(w, http.StatusUnauthorized, "pair a device to get a token")
	})
}

// extractToken reads the bearer token from the places a client can put it.
// EventSource and <img> cannot set headers, so the cookie and query forms exist
// for the web UI.
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if token, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(token)
		}
	}
	if h := r.Header.Get("X-RPSync-Token"); h != "" {
		return strings.TrimSpace(h)
	}
	if c, err := r.Cookie(tokenCookie); err == nil && c.Value != "" {
		return c.Value
	}
	return strings.TrimSpace(r.URL.Query().Get("token"))
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// recoverPanics turns a panic in a handler into a 500 instead of killing the
// daemon and, with it, the import loop.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// recover only works when called directly by the deferred function.
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", v)
				s.writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- responses

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Debug("write response", "error", err)
	}
}

// errorResponse is the shape of every API error.
type errorResponse struct {
	Error string `json:"error"`
}

func (s *Server) writeError(w http.ResponseWriter, code int, msg string) {
	s.writeJSON(w, code, errorResponse{Error: msg})
}
