package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/ccapi"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/events"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/manifest"
	rpsync "github.com/JoshuaAFerguson/canon-rp-sync/internal/sync"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/thumb"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/watcher"
)

// handleHealth answers without authentication so a container healthcheck, a
// reverse proxy or a tunnel can probe the daemon. It deliberately reveals
// nothing about the camera, the library or paired devices.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{
		"service": "rpsync",
		"status":  "ok",
		"version": s.cfg.Version,
	})
}

// pairRequest is the body a device sends to redeem a pairing code.
type pairRequest struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
}

// pairResponse hands back the only copy of the token the daemon will ever show.
type pairResponse struct {
	Token   string `json:"token"`
	Device  Device `json:"device"`
	BaseURL string `json:"base_url,omitempty"`
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var req pairRequest
	if err := decodeJSON(r, &req, 1<<16); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	token, device, err := s.cfg.Devices.Redeem(req.Code, req.Name, req.Platform)
	if err != nil {
		status := http.StatusForbidden
		if errors.Is(err, ErrNoPairing) {
			status = http.StatusConflict
		}
		s.cfg.Bus.Publishf(events.KindPairing, "pairing attempt rejected: %v", err)
		s.writeError(w, status, err.Error())
		return
	}

	// Also set a cookie, so a browser that just paired is logged in and
	// EventSource/<img> requests authenticate without exposing the token to
	// page scripts.
	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
	})

	s.log.Info("device paired", "device", device.Name, "platform", device.Platform)
	s.cfg.Bus.Publishf(events.KindPairing, "paired %s", device.Name)
	s.writeJSON(w, http.StatusOK, pairResponse{
		Token:   token,
		Device:  device,
		BaseURL: s.cfg.AdvertiseURL,
	})
}

// statusResponse is the daemon's overall state, as shown by the web UI and the
// companion apps.
type statusResponse struct {
	Version string           `json:"version"`
	Camera  watcher.Status   `json:"camera"`
	Library manifest.Stats   `json:"library"`
	Storage storageStatus    `json:"storage"`
	Remote  *RemoteStatus    `json:"remote,omitempty"`
	Pairing *pairingStatus   `json:"pairing,omitempty"`
	Devices int              `json:"devices"`
	Recent  []events.Event   `json:"recent,omitempty"`
	Photos  []manifest.Entry `json:"photos,omitempty"`
}

type storageStatus struct {
	Dest     string `json:"dest"`
	Layout   string `json:"layout"`
	Manifest string `json:"manifest"`
}

type pairingStatus struct {
	Active    bool      `json:"active"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	opt := s.cfg.Importer.Options()
	resp := statusResponse{
		Version: s.cfg.Version,
		Library: s.cfg.Manifest.Stats(),
		Storage: storageStatus{
			Dest:     opt.Dest,
			Layout:   opt.Layout,
			Manifest: s.cfg.Manifest.Path(),
		},
		Devices: len(s.cfg.Devices.Devices()),
		Recent:  s.cfg.Bus.Recent(25),
		Photos:  s.cfg.Manifest.List(manifest.Query{Limit: 24}),
	}
	if s.cfg.Camera != nil {
		resp.Camera = s.cfg.Camera.Status()
	} else {
		resp.Camera = watcher.Status{State: watcher.StateStopped}
	}
	if s.cfg.Remote != nil {
		remote := s.cfg.Remote()
		resp.Remote = &remote
	}
	if active, expires := s.cfg.Devices.PairingActive(); active {
		resp.Pairing = &pairingStatus{Active: true, ExpiresAt: expires}
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// photosResponse is a page of the imported library.
type photosResponse struct {
	Photos []manifest.Entry `json:"photos"`
	Total  int              `json:"total"`
}

func (s *Server) handlePhotos(w http.ResponseWriter, r *http.Request) {
	q := manifest.Query{
		Ext:    strings.ToLower(strings.TrimSpace(r.URL.Query().Get("ext"))),
		Limit:  intParam(r, "limit", 100),
		Offset: intParam(r, "offset", 0),
	}
	if since := r.URL.Query().Get("since"); since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "since must be an RFC3339 timestamp")
			return
		}
		q.Since = t
	}

	s.writeJSON(w, http.StatusOK, photosResponse{
		Photos: s.cfg.Manifest.List(q),
		Total:  s.cfg.Manifest.Len(),
	})
}

func (s *Server) handlePhoto(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.cfg.Manifest.ByID(r.PathValue("id"))
	if !ok {
		s.writeError(w, http.StatusNotFound, "no such photo")
		return
	}
	s.writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handlePhotoFile(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.cfg.Manifest.ByID(r.PathValue("id"))
	if !ok {
		s.writeError(w, http.StatusNotFound, "no such photo")
		return
	}
	if !s.withinDest(entry.Dest) {
		s.writeError(w, http.StatusNotFound, "no such photo")
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", entry.Name))
	http.ServeFile(w, r, entry.Dest)
}

// handlePhotoThumb serves a preview. JPEGs are downscaled and cached; for RAW
// the JPEG shot alongside it is used, and failing that the camera's own
// thumbnail while the card is still reachable.
func (s *Server) handlePhotoThumb(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.cfg.Manifest.ByID(r.PathValue("id"))
	if !ok {
		s.writeError(w, http.StatusNotFound, "no such photo")
		return
	}

	size := intParam(r, "size", thumb.DefaultSize)
	if src, ok := s.thumbSource(entry); ok {
		path, err := thumb.Cache(s.cfg.ThumbDir, entry.ID+"-"+strconv.Itoa(size), src, size)
		if err == nil {
			w.Header().Set("Cache-Control", "private, max-age=86400")
			http.ServeFile(w, r, path)
			return
		}
		s.log.Debug("thumbnail generation failed", "photo", entry.Name, "error", err)
	}

	if s.serveCameraThumbnail(w, r, entry) {
		return
	}
	s.writeError(w, http.StatusNotFound, "no preview available for this file")
}

// thumbSource picks a file a thumbnail can be decoded from.
func (s *Server) thumbSource(entry manifest.Entry) (string, bool) {
	if thumb.Supported(entry.Ext) && s.withinDest(entry.Dest) {
		return entry.Dest, true
	}
	// RAW+JPEG shooters have the same frame as a JPEG; use it.
	stem := strings.TrimSuffix(entry.Name, filepath.Ext(entry.Name))
	for _, candidate := range s.cfg.Manifest.List(manifest.Query{}) {
		if candidate.ID == entry.ID || !thumb.Supported(candidate.Ext) {
			continue
		}
		if strings.TrimSuffix(candidate.Name, filepath.Ext(candidate.Name)) == stem {
			return candidate.Dest, true
		}
	}
	return "", false
}

// serveCameraThumbnail streams the embedded preview straight off the card.
func (s *Server) serveCameraThumbnail(w http.ResponseWriter, r *http.Request, entry manifest.Entry) bool {
	if s.cfg.Camera == nil {
		return false
	}
	client := s.cfg.Camera.Client()
	if client == nil || entry.Card == "" || entry.Card == "upload" {
		return false
	}

	file := ccapi.File{
		URL:  fmt.Sprintf("%s/ver100/contents/%s/%s/%s", client.BaseURL(), entry.Card, entry.Dir, entry.Name),
		Name: entry.Name, Dir: entry.Dir, Card: entry.Card,
	}
	w.Header().Set("Content-Type", "image/jpeg")
	if err := client.Thumbnail(r.Context(), file, w); err != nil {
		s.log.Debug("camera thumbnail failed", "photo", entry.Name, "error", err)
		return false
	}
	return true
}

// ingestResponse reports what happened to an uploaded file.
type ingestResponse struct {
	Imported bool           `json:"imported"`
	Entry    manifest.Entry `json:"entry"`
}

// handleIngest accepts a photo from a companion app. This is the handover path:
// the app pulled the shot over the camera's own hotspot while out, and pushes
// it to the daemon once it is back on the home network or through a tunnel.
//
// The body is either multipart/form-data with a "file" part, or the raw bytes
// with the metadata in query parameters.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	device, _ := DeviceFrom(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)

	meta := rpsync.IngestMeta{
		Name:   r.URL.Query().Get("name"),
		Card:   r.URL.Query().Get("card"),
		Dir:    r.URL.Query().Get("dir"),
		Device: device.Name,
	}
	if v := r.URL.Query().Get("captured_at"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "captured_at must be an RFC3339 timestamp")
			return
		}
		meta.CapturedAt = t
	}

	var body io.Reader = r.Body
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		part, header, err := r.FormFile("file")
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "multipart upload needs a \"file\" part")
			return
		}
		defer part.Close()
		body = part

		if meta.Name == "" {
			meta.Name = header.Filename
		}
		if v := r.FormValue("name"); v != "" && meta.Name == "" {
			meta.Name = v
		}
		if v := r.FormValue("card"); v != "" && meta.Card == "" {
			meta.Card = v
		}
		if v := r.FormValue("dir"); v != "" && meta.Dir == "" {
			meta.Dir = v
		}
		if v := r.FormValue("captured_at"); v != "" && meta.CapturedAt.IsZero() {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				meta.CapturedAt = t
			}
		}
	}
	if meta.Name == "" {
		meta.Name = r.Header.Get("X-RPSync-Filename")
	}
	if meta.Name == "" {
		s.writeError(w, http.StatusBadRequest, "a file name is required (name query parameter, multipart filename or X-RPSync-Filename)")
		return
	}

	entry, imported, err := s.cfg.Importer.Ingest(r.Context(), body, meta)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, "upload exceeds the configured limit")
			return
		}
		s.log.Error("ingest failed", "file", meta.Name, "device", device.Name, "error", err)
		s.writeError(w, http.StatusInternalServerError, "could not import the upload")
		return
	}

	if imported {
		s.log.Info("accepted upload", "file", entry.Name, "device", device.Name, "bytes", entry.Size)
	}
	s.writeJSON(w, http.StatusOK, ingestResponse{Imported: imported, Entry: entry})
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Camera == nil {
		s.writeError(w, http.StatusServiceUnavailable, "the camera watcher is not running")
		return
	}
	s.cfg.Camera.SyncNow()
	s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "sync requested"})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"devices": s.cfg.Devices.Devices()})
}

func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	removed, err := s.cfg.Devices.Revoke(r.PathValue("id"))
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not revoke the device")
		return
	}
	if !removed {
		s.writeError(w, http.StatusNotFound, "no such device")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handleEvents streams daemon activity as server-sent events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, unsubscribe := s.cfg.Bus.Subscribe(64)
	defer unsubscribe()

	// Replay recent activity so a page that just loaded is not blank.
	for _, ev := range s.cfg.Bus.Recent(25) {
		s.writeSSE(w, ev)
	}
	flusher.Flush()

	// A heartbeat keeps proxies and tunnels from closing an idle stream.
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			s.writeSSE(w, ev)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) writeSSE(w http.ResponseWriter, ev events.Event) {
	payload, err := jsonMarshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, payload)
}

// withinDest guards against serving a path outside the import tree, even if the
// manifest were tampered with.
func (s *Server) withinDest(path string) bool {
	dest := s.cfg.Importer.Options().Dest
	if dest == "" {
		return false
	}
	rel, err := filepath.Rel(dest, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func intParam(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// pairingCodeResponse is a freshly minted pairing code.
type pairingCodeResponse struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	BaseURL   string    `json:"base_url,omitempty"`
}

// handleNewPairingCode mints a code for another device. It requires an existing
// paired device (or a trusted local request), so possession of the daemon's UI
// is what authorises adding a phone.
func (s *Server) handleNewPairingCode(w http.ResponseWriter, r *http.Request) {
	code, expires, err := s.cfg.Devices.NewPairingCode(DefaultPairingTTL)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "could not generate a pairing code")
		return
	}
	s.cfg.Bus.Publishf(events.KindPairing, "pairing code generated; valid until %s", expires.Format(time.Kitchen))
	s.writeJSON(w, http.StatusOK, pairingCodeResponse{Code: code, ExpiresAt: expires, BaseURL: s.cfg.AdvertiseURL})
}

func (s *Server) handleCancelPairingCode(w http.ResponseWriter, r *http.Request) {
	s.cfg.Devices.CancelPairing()
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}
