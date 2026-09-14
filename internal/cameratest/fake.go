// Package cameratest provides an in-process fake of a CCAPI camera. It exists
// so the sync core, the HTTP API and the daemon can be tested without hardware.
package cameratest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PageSize is how many files the fake returns per content page, matching
// CCAPI's own paging behaviour closely enough to exercise the client.
const PageSize = 100

// Item is one file on the fake camera's card.
type Item struct {
	Card      string
	Dir       string
	Name      string
	Body      []byte
	Thumbnail []byte
	Captured  time.Time
}

// Camera is a fake CCAPI camera backed by httptest.
type Camera struct {
	*httptest.Server

	mu      sync.Mutex
	items   []Item
	added   []string // pending addedcontents for the next poll
	waiters []chan []string
	deleted []string

	// Versions advertised by the API index. Defaults to ver100 and ver110.
	// Set it before issuing any request.
	Versions []string
	// ServeVersion is the version endpoints are actually mounted under. Set it
	// before issuing any request.
	ServeVersion string

	// failPaths makes matching requests fail with the given status. It is
	// guarded because tests change it while the server is running.
	failPaths map[string]int
}

// New starts a fake camera holding the given items.
func New(items ...Item) *Camera {
	c := &Camera{
		Versions:     []string{"ver100", "ver110"},
		ServeVersion: "ver110",
		failPaths:    map[string]int{},
	}
	for _, it := range items {
		c.items = append(c.items, normalize(it))
	}
	c.Server = httptest.NewServer(http.HandlerFunc(c.handle))
	return c
}

func normalize(it Item) Item {
	if it.Card == "" {
		it.Card = "sd"
	}
	if it.Dir == "" {
		it.Dir = "100CANON"
	}
	if it.Body == nil {
		it.Body = []byte("body:" + it.Name)
	}
	if it.Thumbnail == nil {
		it.Thumbnail = []byte("thumb:" + it.Name)
	}
	return it
}

// HostPort returns the fake's host and port, for ccapi.New.
func (c *Camera) HostPort() (string, int) {
	u, err := url.Parse(c.URL)
	if err != nil {
		panic(err)
	}
	port, _ := strconv.Atoi(u.Port())
	return u.Hostname(), port
}

// Add stores a new file and releases any in-flight event poll, the way the
// camera does when a shot is taken.
func (c *Camera) Add(items ...Item) {
	c.mu.Lock()
	var urls []string
	for _, it := range items {
		it = normalize(it)
		c.items = append(c.items, it)
		urls = append(urls, c.fileURL(it))
	}
	waiters := c.waiters
	c.waiters = nil
	if len(waiters) == 0 {
		c.added = append(c.added, urls...)
	}
	c.mu.Unlock()

	for _, w := range waiters {
		w <- urls
	}
}

// Fail makes every request to path fail with the given status, simulating a
// camera that has gone away or is refusing requests. Pass 0 to stop failing.
func (c *Camera) Fail(path string, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if status == 0 {
		delete(c.failPaths, path)
		return
	}
	c.failPaths[path] = status
}

func (c *Camera) failure(path string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	code, ok := c.failPaths[path]
	return code, ok
}

// Deleted returns the keys of files deleted through the API.
func (c *Camera) Deleted() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.deleted...)
}

func (c *Camera) fileURL(it Item) string {
	return fmt.Sprintf("%s/ccapi/%s/contents/%s/%s/%s", c.URL, c.ServeVersion, it.Card, it.Dir, it.Name)
}

func (c *Camera) handle(w http.ResponseWriter, r *http.Request) {
	if code, ok := c.failure(r.URL.Path); ok {
		http.Error(w, `{"message":"forced failure"}`, code)
		return
	}

	switch {
	case r.URL.Path == "/ccapi":
		c.writeIndex(w)
	case r.URL.Path == c.prefix()+"/deviceinformation":
		writeJSON(w, map[string]string{
			"manufacturer":    "Canon",
			"productname":     "Canon EOS RP",
			"guid":            "fake-guid",
			"serialnumber":    "0123456789",
			"firmwareversion": "1.6.0",
		})
	case r.URL.Path == c.prefix()+"/event/polling":
		c.poll(w, r)
	case r.URL.Path == c.prefix()+"/contents":
		writeJSON(w, map[string][]string{"url": c.storageURLs()})
	case strings.HasPrefix(r.URL.Path, c.prefix()+"/contents/"):
		c.contents(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (c *Camera) prefix() string { return "/ccapi/" + c.ServeVersion }

func (c *Camera) writeIndex(w http.ResponseWriter) {
	index := map[string][]map[string]any{}
	for _, v := range c.Versions {
		paths := []string{"/deviceinformation", "/contents", "/event/polling"}
		if v != c.ServeVersion {
			// Older versions advertise fewer endpoints; the client should pick
			// the newest one that offers each.
			paths = []string{"/deviceinformation"}
		}
		for _, p := range paths {
			index[v] = append(index[v], map[string]any{
				"path": "/ccapi/" + v + p,
				"get":  true,
			})
		}
	}
	writeJSON(w, index)
}

func (c *Camera) storageURLs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]bool{}
	var urls []string
	for _, it := range c.items {
		if seen[it.Card] {
			continue
		}
		seen[it.Card] = true
		urls = append(urls, fmt.Sprintf("%s/contents/%s", c.URL+c.prefix(), it.Card))
	}
	sort.Strings(urls)
	return urls
}

// contents serves storage, directory and file requests.
func (c *Camera) contents(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, c.prefix()+"/contents/"), "/")
	parts := strings.Split(rest, "/")

	switch len(parts) {
	case 1: // /contents/sd -> directories
		writeJSON(w, map[string][]string{"url": c.dirURLs(parts[0])})
	case 2: // /contents/sd/100CANON -> paged file list
		c.listDir(w, r, parts[0], parts[1])
	case 3: // /contents/sd/100CANON/IMG_0001.JPG
		c.file(w, r, parts[0], parts[1], parts[2])
	default:
		http.NotFound(w, r)
	}
}

func (c *Camera) dirURLs(card string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]bool{}
	var urls []string
	for _, it := range c.items {
		if it.Card != card || seen[it.Dir] {
			continue
		}
		seen[it.Dir] = true
		urls = append(urls, fmt.Sprintf("%s/contents/%s/%s", c.URL+c.prefix(), card, it.Dir))
	}
	sort.Strings(urls)
	return urls
}

func (c *Camera) listDir(w http.ResponseWriter, r *http.Request, card, dir string) {
	c.mu.Lock()
	var urls []string
	for _, it := range c.items {
		if it.Card == card && it.Dir == dir {
			urls = append(urls, c.fileURL(it))
		}
	}
	c.mu.Unlock()
	sort.Strings(urls)

	pages := (len(urls) + PageSize - 1) / PageSize
	if pages == 0 {
		pages = 1
	}

	switch r.URL.Query().Get("kind") {
	case "number":
		writeJSON(w, map[string]int{"pagenumber": pages})
	case "list":
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		start := (page - 1) * PageSize
		if start > len(urls) {
			start = len(urls)
		}
		end := start + PageSize
		if end > len(urls) {
			end = len(urls)
		}
		writeJSON(w, map[string][]string{"url": urls[start:end]})
	default:
		http.Error(w, `{"message":"unsupported kind"}`, http.StatusBadRequest)
	}
}

func (c *Camera) file(w http.ResponseWriter, r *http.Request, card, dir, name string) {
	c.mu.Lock()
	idx := -1
	for i, it := range c.items {
		if it.Card == card && it.Dir == dir && it.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		c.mu.Unlock()
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		return
	}
	item := c.items[idx]

	if r.Method == http.MethodDelete {
		c.items = append(c.items[:idx], c.items[idx+1:]...)
		c.deleted = append(c.deleted, card+"/"+dir+"/"+name)
		c.mu.Unlock()
		writeJSON(w, map[string]string{})
		return
	}
	c.mu.Unlock()

	switch r.URL.Query().Get("kind") {
	case "thumbnail":
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(item.Thumbnail)
	case "info":
		info := map[string]any{"filesize": len(item.Body)}
		if !item.Captured.IsZero() {
			info["datetime"] = item.Captured.Format(time.RFC1123Z)
		}
		writeJSON(w, info)
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(item.Body)
	}
}

// poll implements the long-polling event endpoint.
func (c *Camera) poll(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	if len(c.added) > 0 {
		added := c.added
		c.added = nil
		c.mu.Unlock()
		writeJSON(w, map[string][]string{"addedcontents": added})
		return
	}
	ch := make(chan []string, 1)
	c.waiters = append(c.waiters, ch)
	c.mu.Unlock()

	select {
	case added := <-ch:
		writeJSON(w, map[string][]string{"addedcontents": added})
	case <-r.Context().Done():
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
