// Package ccapi is a minimal client for the Canon Camera Control API (CCAPI)
// as exposed by the EOS RP and other CCAPI-enabled bodies.
//
// Only the subset needed for image sync is implemented: device info, content
// listing/download/deletion, and event polling. The package deliberately has no
// third-party dependencies so it can be bound into the mobile companion apps.
package ccapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultPort is the fixed port CCAPI listens on.
const DefaultPort = 8080

// fallbackVersion is used when the camera's API index cannot be read.
const fallbackVersion = "ver100"

// ErrNotSupported is returned when the camera does not advertise an endpoint.
var ErrNotSupported = errors.New("ccapi: endpoint not supported by this camera")

// Options tunes a Client.
type Options struct {
	Port int
	// Timeout bounds ordinary requests. Long polls are bounded by context only.
	Timeout time.Duration
	// Transport is used for tests; nil means http.DefaultTransport.
	Transport http.RoundTripper
}

// Client talks to a single camera. It is safe for concurrent use, and
// serializes ordinary requests because the camera accepts only a couple of
// simultaneous HTTP connections.
type Client struct {
	host string
	port int
	base string // http://host:port/ccapi

	httpc *http.Client // ordinary requests
	pollc *http.Client // long polls: no client-side timeout

	reqMu sync.Mutex // serializes ordinary requests

	apiMu sync.Mutex
	api   map[string]string // "/deviceinformation" -> absolute URL
}

// New returns a client for the camera at host.
func New(host string, opt Options) *Client {
	if opt.Port == 0 {
		opt.Port = DefaultPort
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 60 * time.Second
	}
	return &Client{
		host:  host,
		port:  opt.Port,
		base:  fmt.Sprintf("http://%s/ccapi", net.JoinHostPort(host, fmt.Sprint(opt.Port))),
		httpc: &http.Client{Timeout: opt.Timeout, Transport: opt.Transport},
		pollc: &http.Client{Transport: opt.Transport},
	}
}

// Host reports the camera address this client is bound to.
func (c *Client) Host() string { return c.host }

// Port reports the camera port.
func (c *Client) Port() int { return c.port }

// BaseURL reports the CCAPI root, e.g. http://192.168.1.42:8080/ccapi.
func (c *Client) BaseURL() string { return c.base }

// DeviceInfo is the subset of /deviceinformation we care about.
type DeviceInfo struct {
	Manufacturer    string `json:"manufacturer"`
	ProductName     string `json:"productname"`
	GUID            string `json:"guid"`
	SerialNumber    string `json:"serialnumber"`
	FirmwareVersion string `json:"firmwareversion"`
	MacAddress      string `json:"macaddress"`
}

// DeviceInfo fetches identifying information from the camera. It doubles as the
// reachability check: a camera that answers this is ready for use.
func (c *Client) DeviceInfo(ctx context.Context) (*DeviceInfo, error) {
	var out DeviceInfo
	if err := c.getJSON(ctx, "/deviceinformation", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// File is a single image/movie on the camera's card.
type File struct {
	// URL is the absolute CCAPI URL for the file, e.g.
	// http://host:8080/ccapi/ver100/contents/sd/100CANON/IMG_0001.JPG
	URL  string `json:"url"`
	Name string `json:"name"` // IMG_0001.JPG
	Dir  string `json:"dir"`  // 100CANON
	Card string `json:"card"` // sd
}

// Ext returns the lower-cased extension without the dot.
func (f File) Ext() string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(f.Name), "."))
}

// Key uniquely identifies a file on a card.
func (f File) Key() string { return f.Card + "/" + f.Dir + "/" + f.Name }

// Stem is the file name without its extension, used to group a RAW+JPEG pair.
func (f File) Stem() string { return strings.TrimSuffix(f.Name, path.Ext(f.Name)) }

// ListFiles walks every storage device and directory and returns all files,
// oldest directory first.
func (c *Client) ListFiles(ctx context.Context) ([]File, error) {
	storages, err := c.listURLs(ctx, "/contents", "")
	if err != nil {
		return nil, fmt.Errorf("list storage: %w", err)
	}

	var files []File
	for _, storageURL := range storages {
		dirs, err := c.listURLsAbs(ctx, storageURL)
		if err != nil {
			return nil, fmt.Errorf("list directories on %s: %w", storageURL, err)
		}
		sort.Strings(dirs)
		for _, dirURL := range dirs {
			dirFiles, err := c.listDir(ctx, dirURL)
			if err != nil {
				return nil, err
			}
			files = append(files, dirFiles...)
		}
	}
	return files, nil
}

// listDir returns every file in one camera directory, following CCAPI's paging.
func (c *Client) listDir(ctx context.Context, dirURL string) ([]File, error) {
	var pages struct {
		PageNum int `json:"pagenumber"`
	}
	if err := c.getJSONAbs(ctx, dirURL+"?type=all&kind=number", &pages); err != nil {
		return nil, fmt.Errorf("page count for %s: %w", dirURL, err)
	}
	if pages.PageNum < 1 {
		pages.PageNum = 1
	}

	var files []File
	for p := 1; p <= pages.PageNum; p++ {
		urls, err := c.listURLsAbs(ctx, fmt.Sprintf("%s?type=all&kind=list&page=%d", dirURL, p))
		if err != nil {
			return nil, fmt.Errorf("list page %d of %s: %w", p, dirURL, err)
		}
		for _, fu := range urls {
			files = append(files, FileFromURL(fu))
		}
	}
	return files, nil
}

func (c *Client) listURLs(ctx context.Context, endpoint, query string) ([]string, error) {
	abs, err := c.endpoint(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	return c.listURLsAbs(ctx, abs+query)
}

func (c *Client) listURLsAbs(ctx context.Context, abs string) ([]string, error) {
	var out struct {
		URL []string `json:"url"`
	}
	if err := c.getJSONAbs(ctx, abs, &out); err != nil {
		return nil, err
	}
	return out.URL, nil
}

// Info is the metadata CCAPI reports for a single file.
type Info struct {
	// FileSize is the size in bytes, or 0 when the camera does not report it.
	FileSize int64
	// CapturedAt is the camera's timestamp for the file, if it reports one.
	CapturedAt time.Time
}

// FileInfo fetches per-file metadata (?kind=info). Cameras vary in what they
// report, so callers should treat a zero field as "unknown" rather than an error.
func (c *Client) FileInfo(ctx context.Context, f File) (Info, error) {
	var raw struct {
		FileSize int64  `json:"filesize"`
		DateTime string `json:"datetime"`
	}
	if err := c.getJSONAbs(ctx, f.URL+"?kind=info", &raw); err != nil {
		return Info{}, err
	}
	info := Info{FileSize: raw.FileSize}
	if raw.DateTime != "" {
		// CCAPI reports RFC1123-with-numeric-zone, e.g.
		// "Tue, 01 Jan 2019 12:00:00 +0900".
		if t, err := time.Parse(time.RFC1123Z, raw.DateTime); err == nil {
			info.CapturedAt = t
		}
	}
	return info, nil
}

// Download streams a file from the camera into dest, creating parent
// directories and writing through a temporary file so a partial transfer never
// looks like a finished import. It returns the number of bytes written.
func (c *Client) Download(ctx context.Context, f File, dest string) (int64, error) {
	return c.download(ctx, f.URL, dest)
}

// DownloadThumbnail fetches the embedded thumbnail instead of the full file.
// This is how the companion apps preview CR3s without decoding RAW.
func (c *Client) DownloadThumbnail(ctx context.Context, f File, dest string) (int64, error) {
	return c.download(ctx, f.URL+"?kind=thumbnail", dest)
}

// Thumbnail streams the embedded thumbnail to w.
func (c *Client) Thumbnail(ctx context.Context, f File, w io.Writer) error {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()
	resp, err := c.do(ctx, c.httpc, http.MethodGet, f.URL+"?kind=thumbnail")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

func (c *Client) download(ctx context.Context, src, dest string) (int64, error) {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()

	resp, err := c.do(ctx, c.httpc, http.MethodGet, src)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, resp.Body)
	if err != nil {
		out.Close()
		os.Remove(tmp)
		return n, err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return n, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return n, err
	}
	return n, nil
}

// Delete removes a file from the card. Only call this when the user has
// explicitly opted in.
func (c *Client) Delete(ctx context.Context, f File) error {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()
	resp, err := c.do(ctx, c.httpc, http.MethodDelete, f.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}

// Event is the subset of a CCAPI polling event rpsync reacts to.
type Event struct {
	AddedContents []string `json:"addedcontents"`
	// BatteryList and others are ignored; the field below keeps the raw payload
	// available for debugging.
	Raw json.RawMessage `json:"-"`
}

// AddedFiles returns the new files reported by the event.
func (e *Event) AddedFiles() []File {
	files := make([]File, 0, len(e.AddedContents))
	for _, u := range e.AddedContents {
		files = append(files, FileFromURL(u))
	}
	return files
}

// PollEvents long-polls the camera, blocking until it reports an event, the
// camera closes the connection, or ctx is cancelled. Unlike ordinary requests
// it has no client-side timeout: the caller controls the lifetime.
//
// The camera accepts very few simultaneous connections, so a poll must not be
// left running while files are downloading — cancel ctx first.
func (c *Client) PollEvents(ctx context.Context) (*Event, error) {
	abs, err := c.endpoint(ctx, "/event/polling")
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, c.pollc, http.MethodGet, abs+"?continue=on")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	ev := &Event{Raw: body}
	if err := json.Unmarshal(body, ev); err != nil {
		return nil, fmt.Errorf("decode event: %w", err)
	}
	return ev, nil
}

// endpoint resolves a version-independent path such as "/deviceinformation" to
// the absolute URL for the newest API version the camera advertises.
func (c *Client) endpoint(ctx context.Context, name string) (string, error) {
	c.apiMu.Lock()
	api := c.api
	c.apiMu.Unlock()

	if api == nil {
		var err error
		api, err = c.loadAPIIndex(ctx)
		if err != nil {
			// The index is an optimization; fall back to ver100 paths so a
			// camera with an unexpected index still works.
			api = map[string]string{}
		}
		c.apiMu.Lock()
		c.api = api
		c.apiMu.Unlock()
	}

	if abs, ok := api[name]; ok {
		return abs, nil
	}
	if len(api) > 0 {
		// The index parsed but does not list this endpoint.
		return "", fmt.Errorf("%w: %s", ErrNotSupported, name)
	}
	return c.base + "/" + fallbackVersion + name, nil
}

// loadAPIIndex reads GET /ccapi, which maps each API version to the endpoints
// it supports, and picks the newest version offering each endpoint.
func (c *Client) loadAPIIndex(ctx context.Context) (map[string]string, error) {
	var index map[string][]struct {
		Path string `json:"path"`
	}
	if err := c.getJSONAbs(ctx, c.base, &index); err != nil {
		return nil, err
	}
	if len(index) == 0 {
		return nil, errors.New("ccapi: empty API index")
	}

	versions := make([]string, 0, len(index))
	for v := range index {
		versions = append(versions, v)
	}
	sort.Strings(versions) // ver100 < ver110 < ver120 lexically

	api := map[string]string{}
	for _, v := range versions {
		prefix := "/ccapi/" + v
		for _, ep := range index[v] {
			name := strings.TrimPrefix(ep.Path, prefix)
			if name == ep.Path || name == "" {
				continue
			}
			api[name] = c.base + "/" + v + name
		}
	}
	if len(api) == 0 {
		return nil, errors.New("ccapi: API index listed no usable endpoints")
	}
	return api, nil
}

func (c *Client) getJSON(ctx context.Context, name string, out any) error {
	abs, err := c.endpoint(ctx, name)
	if err != nil {
		return err
	}
	return c.getJSONAbs(ctx, abs, out)
}

func (c *Client) getJSONAbs(ctx context.Context, abs string, out any) error {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()

	resp, err := c.do(ctx, c.httpc, http.MethodGet, abs)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", abs, err)
	}
	return nil
}

// do issues a request and converts a non-2xx response into an error, including
// the camera's own message which is usually more useful than the status code.
func (c *Client) do(ctx context.Context, hc *http.Client, method, abs string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, abs, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "rpsync")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		msg := strings.TrimSpace(string(body))
		if m := decodeAPIMessage(body); m != "" {
			msg = m
		}
		return nil, &HTTPError{Method: method, URL: abs, StatusCode: resp.StatusCode, Message: msg}
	}
	return resp, nil
}

// HTTPError is a non-2xx response from the camera.
type HTTPError struct {
	Method     string
	URL        string
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.URL, e.StatusCode)
}

func decodeAPIMessage(body []byte) string {
	var out struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ""
	}
	return out.Message
}

// FileFromURL parses a CCAPI content URL into its card/directory/name parts.
func FileFromURL(raw string) File {
	u, err := url.Parse(raw)
	if err != nil {
		return File{URL: raw, Name: raw}
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	f := File{URL: raw, Name: parts[len(parts)-1]}
	if len(parts) >= 2 {
		f.Dir = parts[len(parts)-2]
	}
	if len(parts) >= 3 {
		f.Card = parts[len(parts)-3]
	}
	return f
}
