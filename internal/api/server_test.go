package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/api"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/ccapi"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/events"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/manifest"
	rpsync "github.com/JoshuaAFerguson/canon-rp-sync/internal/sync"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/watcher"
)

// stubCamera stands in for the watcher.
type stubCamera struct {
	status    watcher.Status
	syncCalls int
}

func (s *stubCamera) Status() watcher.Status { return s.status }
func (s *stubCamera) SyncNow()               { s.syncCalls++ }
func (s *stubCamera) Client() *ccapi.Client  { return nil }

type testServer struct {
	*httptest.Server
	store    *api.DeviceStore
	manifest *manifest.Manifest
	importer *rpsync.Importer
	camera   *stubCamera
	bus      *events.Bus
	dest     string
	token    string
}

func newTestServer(t *testing.T, allowLocal bool) *testServer {
	t.Helper()

	dir := t.TempDir()
	dest := filepath.Join(dir, "photos")
	mf, err := manifest.Load(filepath.Join(dest, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := api.LoadDeviceStore(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	im := rpsync.New(mf, rpsync.Options{Dest: dest, DateSource: rpsync.DateFromImport})
	cam := &stubCamera{status: watcher.Status{State: watcher.StateConnected,
		Camera: &watcher.CameraInfo{Host: "192.168.1.42", Model: "Canon EOS RP"}}}
	bus := events.NewBus()

	srv := api.New(api.Config{
		Devices:                   store,
		Manifest:                  mf,
		Importer:                  im,
		Camera:                    cam,
		Bus:                       bus,
		ThumbDir:                  filepath.Join(dir, "thumbs"),
		MaxUploadBytes:            1 << 20,
		AllowLocalUnauthenticated: allowLocal,
		Version:                   "test",
		Remote: func() api.RemoteStatus {
			return api.RemoteStatus{Mode: "tailscale", State: "ready", URL: "http://nas:8787"}
		},
	})

	ts := &testServer{
		Server:   httptest.NewServer(srv.Handler()),
		store:    store,
		manifest: mf,
		importer: im,
		camera:   cam,
		bus:      bus,
		dest:     dest,
	}
	t.Cleanup(ts.Close)
	return ts
}

// pair issues a token the way a companion app would.
func (ts *testServer) pair(t *testing.T, name string) string {
	t.Helper()
	code, _, err := ts.store.NewPairingCode(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"code": code, "name": name, "platform": "ios"})

	resp, err := http.Post(ts.URL+"/api/v1/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("pair: HTTP %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Token  string     `json:"token"`
		Device api.Device `json:"device"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	ts.token = out.Token
	return out.Token
}

func (ts *testServer) do(t *testing.T, method, path, token string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHealthNeedsNoAuth(t *testing.T) {
	ts := newTestServer(t, false)
	resp := ts.do(t, http.MethodGet, "/api/v1/health", "", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" || body["version"] != "test" {
		t.Errorf("body = %v", body)
	}
	// It must not leak anything about the library or the camera.
	for _, key := range []string{"camera", "library", "devices"} {
		if _, present := body[key]; present {
			t.Errorf("health response leaks %q", key)
		}
	}
}

func TestProtectedRoutesRejectAnonymous(t *testing.T) {
	ts := newTestServer(t, false)
	paths := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/status"},
		{http.MethodGet, "/api/v1/photos"},
		{http.MethodGet, "/api/v1/events"},
		{http.MethodGet, "/api/v1/devices"},
		{http.MethodPost, "/api/v1/sync"},
		{http.MethodPost, "/api/v1/ingest"},
		{http.MethodPost, "/api/v1/pair/code"},
	}
	for _, p := range paths {
		resp := ts.do(t, p.method, p.path, "", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s: HTTP %d, want 401", p.method, p.path, resp.StatusCode)
		}
	}
}

func TestInvalidTokenIsRejected(t *testing.T) {
	ts := newTestServer(t, false)
	resp := ts.do(t, http.MethodGet, "/api/v1/status", "rps_not-a-real-token", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("HTTP %d, want 401", resp.StatusCode)
	}
}

func TestRevokedTokenStopsWorking(t *testing.T) {
	ts := newTestServer(t, false)
	token := ts.pair(t, "iPad")

	resp := ts.do(t, http.MethodGet, "/api/v1/status", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d before revocation", resp.StatusCode)
	}

	devices := ts.store.Devices()
	if len(devices) != 1 {
		t.Fatalf("devices = %d", len(devices))
	}
	resp = ts.do(t, http.MethodDelete, "/api/v1/devices/"+devices[0].ID, token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: HTTP %d", resp.StatusCode)
	}

	resp = ts.do(t, http.MethodGet, "/api/v1/status", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("HTTP %d after revocation, want 401", resp.StatusCode)
	}
}

func TestLocalUnauthenticatedAccess(t *testing.T) {
	ts := newTestServer(t, true)
	resp := ts.do(t, http.MethodGet, "/api/v1/status", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d: loopback access should be allowed when configured", resp.StatusCode)
	}
}

func TestPairSetsCookieUsableForAuth(t *testing.T) {
	ts := newTestServer(t, false)
	code, _, err := ts.store.NewPairingCode(time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	jar := &cookieJar{}
	client := &http.Client{Jar: jar}
	body, _ := json.Marshal(map[string]string{"code": code, "name": "Web UI", "platform": "web"})
	resp, err := client.Post(ts.URL+"/api/v1/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pair: HTTP %d", resp.StatusCode)
	}

	// The browser is now logged in with the cookie alone.
	statusResp, err := client.Get(ts.URL + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status with cookie: HTTP %d", statusResp.StatusCode)
	}

	var cookie *http.Cookie
	for _, c := range jar.cookies {
		if c.Name == "rpsync_token" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie was set")
	}
	if !cookie.HttpOnly {
		t.Error("the session cookie should be HttpOnly")
	}
}

func TestPairRejectsWrongCode(t *testing.T) {
	ts := newTestServer(t, false)
	if _, _, err := ts.store.NewPairingCode(time.Minute); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"code": "AAAA-BBBB", "name": "attacker"})
	resp := ts.do(t, http.MethodPost, "/api/v1/pair", "", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("HTTP %d, want 403", resp.StatusCode)
	}
}

func TestPairWithoutActiveCodeConflicts(t *testing.T) {
	ts := newTestServer(t, false)
	body, _ := json.Marshal(map[string]string{"code": "AAAA-BBBB", "name": "device"})
	resp := ts.do(t, http.MethodPost, "/api/v1/pair", "", bytes.NewReader(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("HTTP %d, want 409", resp.StatusCode)
	}
}

func TestStatusReportsDaemonState(t *testing.T) {
	ts := newTestServer(t, false)
	token := ts.pair(t, "iPad")

	resp := ts.do(t, http.MethodGet, "/api/v1/status", token, nil)
	defer resp.Body.Close()

	var body struct {
		Version string `json:"version"`
		Camera  struct {
			State  string `json:"state"`
			Camera *struct {
				Model string `json:"model"`
			} `json:"camera"`
		} `json:"camera"`
		Library struct {
			Files int `json:"files"`
		} `json:"library"`
		Remote *struct {
			Mode string `json:"mode"`
			URL  string `json:"url"`
		} `json:"remote"`
		Devices int `json:"devices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Camera.State != "connected" || body.Camera.Camera.Model != "Canon EOS RP" {
		t.Errorf("camera = %+v", body.Camera)
	}
	if body.Remote == nil || body.Remote.Mode != "tailscale" || body.Remote.URL != "http://nas:8787" {
		t.Errorf("remote = %+v", body.Remote)
	}
	if body.Devices != 1 {
		t.Errorf("devices = %d", body.Devices)
	}
}

func TestIngestRawBodyAndDedupe(t *testing.T) {
	ts := newTestServer(t, false)
	token := ts.pair(t, "Josh's iPad")

	resp := ts.do(t, http.MethodPost,
		"/api/v1/ingest?name=IMG_0101.JPG&card=sd&dir=100CANON", token,
		strings.NewReader("photo-bytes"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTP %d: %s", resp.StatusCode, b)
	}

	var out struct {
		Imported bool           `json:"imported"`
		Entry    manifest.Entry `json:"entry"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.Imported {
		t.Error("first upload should be imported")
	}
	if out.Entry.Source != "device:Josh's iPad" {
		t.Errorf("Source = %q, want the paired device name", out.Entry.Source)
	}
	if !strings.HasPrefix(out.Entry.Dest, ts.dest) {
		t.Errorf("Dest = %q, outside the destination tree", out.Entry.Dest)
	}

	// Same file again: accepted, but not imported twice.
	resp2 := ts.do(t, http.MethodPost,
		"/api/v1/ingest?name=IMG_0101.JPG&card=sd&dir=100CANON", token,
		strings.NewReader("photo-bytes"))
	defer resp2.Body.Close()
	var out2 struct {
		Imported bool `json:"imported"`
	}
	json.NewDecoder(resp2.Body).Decode(&out2)
	if out2.Imported {
		t.Error("re-uploading the same file should not import it again")
	}
	if ts.manifest.Len() != 1 {
		t.Errorf("manifest has %d entries", ts.manifest.Len())
	}
}

func TestIngestMultipart(t *testing.T) {
	ts := newTestServer(t, false)
	token := ts.pair(t, "Pixel")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "IMG_0202.CR3")
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte("raw-bytes"))
	mw.WriteField("card", "sd")
	mw.WriteField("dir", "100CANON")
	mw.WriteField("captured_at", "2024-05-04T10:30:00Z")
	mw.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/ingest", &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTP %d: %s", resp.StatusCode, b)
	}

	var out struct {
		Entry manifest.Entry `json:"entry"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.Entry.Name != "IMG_0202.CR3" {
		t.Errorf("Name = %q", out.Entry.Name)
	}
	if out.Entry.Ext != "cr3" {
		t.Errorf("Ext = %q", out.Entry.Ext)
	}
}

func TestIngestRequiresName(t *testing.T) {
	ts := newTestServer(t, false)
	token := ts.pair(t, "device")
	resp := ts.do(t, http.MethodPost, "/api/v1/ingest", token, strings.NewReader("bytes"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("HTTP %d, want 400", resp.StatusCode)
	}
}

func TestIngestEnforcesUploadLimit(t *testing.T) {
	ts := newTestServer(t, false)
	token := ts.pair(t, "device")

	big := strings.Repeat("x", (1<<20)+1024) // just over the 1 MiB test limit
	resp := ts.do(t, http.MethodPost, "/api/v1/ingest?name=big.jpg", token, strings.NewReader(big))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("HTTP %d, want 413", resp.StatusCode)
	}
}

func TestPhotosListingAndFile(t *testing.T) {
	ts := newTestServer(t, false)
	token := ts.pair(t, "iPad")

	for i, name := range []string{"IMG_0001.JPG", "IMG_0002.CR3"} {
		if _, _, err := ts.importer.Ingest(context.Background(),
			strings.NewReader(fmt.Sprintf("bytes-%d", i)),
			rpsync.IngestMeta{Name: name, Card: "sd", Dir: "100CANON"}); err != nil {
			t.Fatal(err)
		}
	}

	resp := ts.do(t, http.MethodGet, "/api/v1/photos?limit=10", token, nil)
	defer resp.Body.Close()
	var list struct {
		Photos []manifest.Entry `json:"photos"`
		Total  int              `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 2 || len(list.Photos) != 2 {
		t.Fatalf("listing = %+v", list)
	}

	// Filter by extension.
	resp = ts.do(t, http.MethodGet, "/api/v1/photos?ext=cr3", token, nil)
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Photos) != 1 || list.Photos[0].Ext != "cr3" {
		t.Errorf("ext filter = %+v", list.Photos)
	}

	// Download the file itself.
	id := list.Photos[0].ID
	resp = ts.do(t, http.MethodGet, "/api/v1/photos/"+id+"/file", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("file: HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(string(body), "bytes-") {
		t.Errorf("body = %q", body)
	}

	// Metadata for one photo.
	resp = ts.do(t, http.MethodGet, "/api/v1/photos/"+id, token, nil)
	defer resp.Body.Close()
	var one manifest.Entry
	json.NewDecoder(resp.Body).Decode(&one)
	if one.ID != id {
		t.Errorf("entry = %+v", one)
	}
}

func TestUnknownPhotoIs404(t *testing.T) {
	ts := newTestServer(t, false)
	token := ts.pair(t, "iPad")
	for _, path := range []string{"/api/v1/photos/nope", "/api/v1/photos/nope/file", "/api/v1/photos/nope/thumb"} {
		resp := ts.do(t, http.MethodGet, path, token, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: HTTP %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestSyncTriggersWatcher(t *testing.T) {
	ts := newTestServer(t, false)
	token := ts.pair(t, "iPad")

	resp := ts.do(t, http.MethodPost, "/api/v1/sync", token, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("HTTP %d, want 202", resp.StatusCode)
	}
	if ts.camera.syncCalls != 1 {
		t.Errorf("syncCalls = %d", ts.camera.syncCalls)
	}
}

func TestEventStream(t *testing.T) {
	ts := newTestServer(t, false)
	token := ts.pair(t, "iPad")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	// Give the subscription a moment, then publish.
	go func() {
		time.Sleep(100 * time.Millisecond)
		ts.bus.Publishf(events.KindImport, "imported IMG_9999.JPG")
	}()

	found := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc strings.Builder
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
				if strings.Contains(acc.String(), "IMG_9999.JPG") {
					found <- acc.String()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	select {
	case payload := <-found:
		if !strings.Contains(payload, "event: import") {
			t.Errorf("stream payload missing the event type: %q", payload)
		}
	case <-ctx.Done():
		t.Fatal("event was not delivered over the stream")
	}
}

func TestWebUIIsServed(t *testing.T) {
	ts := newTestServer(t, false)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "rpsync") {
		t.Error("the web UI did not render")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}

	missing, err := http.Get(ts.URL + "/does-not-exist.js")
	if err != nil {
		t.Fatal(err)
	}
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("unknown asset: HTTP %d, want 404", missing.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	ts := newTestServer(t, false)
	resp := ts.do(t, http.MethodGet, "/api/v1/health", "", nil)
	defer resp.Body.Close()
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// cookieJar is a minimal http.CookieJar for the pairing-cookie test.
type cookieJar struct{ cookies []*http.Cookie }

func (j *cookieJar) SetCookies(u *urlURL, cookies []*http.Cookie) {
	j.cookies = append(j.cookies, cookies...)
}
func (j *cookieJar) Cookies(u *urlURL) []*http.Cookie { return j.cookies }
