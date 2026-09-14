// Package ccapi is a minimal client for the Canon Camera Control API (CCAPI)
// as exposed by the EOS RP and other CCAPI-enabled bodies.
//
// Only the subset needed for image sync is implemented: device info,
// content listing/download, and event polling.
package ccapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

const (
	DefaultPort = 8080
	apiVersion  = "ver100"
)

// Client talks to a single camera.
type Client struct {
	base string // e.g. http://192.168.1.42:8080/ccapi
	http *http.Client
}

// New returns a client for the camera at host:port.
func New(host string, port int) *Client {
	if port == 0 {
		port = DefaultPort
	}
	return &Client{
		base: fmt.Sprintf("http://%s:%d/ccapi", host, port),
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// DeviceInfo is the subset of /deviceinformation we care about.
type DeviceInfo struct {
	Manufacturer    string `json:"manufacturer"`
	ProductName     string `json:"productname"`
	GUID            string `json:"guid"`
	SerialNumber    string `json:"serialnumber"`
	FirmwareVersion string `json:"firmwareversion"`
}

// DeviceInfo fetches identifying information from the camera.
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
	URL  string
	Name string // IMG_0001.JPG
	Dir  string // 100CANON
	Card string // sd
}

// Ext returns the lower-cased extension without the dot.
func (f File) Ext() string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(f.Name), "."))
}

// ListFiles walks every storage device and directory and returns all files.
func (c *Client) ListFiles(ctx context.Context) ([]File, error) {
	var storages struct {
		URL []string `json:"url"`
	}
	if err := c.getJSON(ctx, "/contents", &storages); err != nil {
		return nil, fmt.Errorf("list storage: %w", err)
	}

	var files []File
	for _, storageURL := range storages.URL {
		var dirs struct {
			URL []string `json:"url"`
		}
		if err := c.getJSONAbs(ctx, storageURL, &dirs); err != nil {
			return nil, fmt.Errorf("list dirs %s: %w", storageURL, err)
		}
		for _, dirURL := range dirs.URL {
			// Directories can be paged; CCAPI returns "pages" then "page=N" queries.
			var pages struct {
				PageNum int `json:"pagenumber"`
			}
			if err := c.getJSONAbs(ctx, dirURL+"?type=all&kind=number", &pages); err != nil {
				return nil, fmt.Errorf("page count %s: %w", dirURL, err)
			}
			for p := 1; p <= max(pages.PageNum, 1); p++ {
				var page struct {
					URL []string `json:"url"`
				}
				if err := c.getJSONAbs(ctx, fmt.Sprintf("%s?type=all&kind=list&page=%d", dirURL, p), &page); err != nil {
					return nil, fmt.Errorf("list page %d %s: %w", p, dirURL, err)
				}
				for _, fu := range page.URL {
					files = append(files, fileFromURL(fu))
				}
			}
		}
	}
	return files, nil
}

// Download streams a file from the camera to dest, creating parent dirs.
func (c *Client) Download(ctx context.Context, f File, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", f.Name, resp.StatusCode)
	}
	if err := os.MkdirAll(path.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// Delete removes a file from the card. Only call this when the user has
// explicitly opted in.
func (c *Client) Delete(ctx context.Context, f File) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, f.URL, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("delete %s: HTTP %d", f.Name, resp.StatusCode)
	}
	return nil
}

// Event is a (partial) CCAPI polling event.
type Event struct {
	AddedContents []string `json:"addedcontents"`
}

// PollEvents blocks until the camera reports an event or the request times out.
// Use continue=on so the camera keeps the connection open.
func (c *Client) PollEvents(ctx context.Context) (*Event, error) {
	var ev Event
	if err := c.getJSON(ctx, "/event/polling?continue=on", &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

func (c *Client) getJSON(ctx context.Context, p string, out any) error {
	return c.getJSONAbs(ctx, c.base+"/"+apiVersion+p, out)
}

func (c *Client) getJSONAbs(ctx context.Context, abs string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, abs, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s: HTTP %d: %s", abs, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func fileFromURL(raw string) File {
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
