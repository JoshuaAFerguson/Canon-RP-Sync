package discovery_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/cameratest"
	"github.com/JoshuaAFerguson/canon-rp-sync/internal/discovery"
)

func TestParseSearchResponse(t *testing.T) {
	canon := "HTTP/1.1 200 OK\r\n" +
		"CACHE-CONTROL: max-age=1800\r\n" +
		"LOCATION: http://192.168.1.42:49152/upnp/desc.xml\r\n" +
		"SERVER: Canon EOS RP/1.6.0 UPnP/1.0\r\n" +
		"ST: upnp:rootdevice\r\n" +
		"USN: uuid:0000-canon::upnp:rootdevice\r\n\r\n"

	cam, ok := discovery.ParseSearchResponse([]byte(canon), "192.168.1.42", 8080)
	if !ok {
		t.Fatal("a Canon response should be recognised")
	}
	if cam.Host != "192.168.1.42" {
		t.Errorf("Host = %q", cam.Host)
	}
	// The description document is on 49152; CCAPI is not, so the default stands.
	if cam.Port != 8080 {
		t.Errorf("Port = %d, want the CCAPI default 8080", cam.Port)
	}
	if cam.Source != "ssdp" {
		t.Errorf("Source = %q", cam.Source)
	}
	if cam.Name == "" {
		t.Error("Name should carry the SERVER header")
	}
}

func TestParseSearchResponseIgnoresOtherVendors(t *testing.T) {
	other := "HTTP/1.1 200 OK\r\n" +
		"LOCATION: http://192.168.1.10:1400/xml/device_description.xml\r\n" +
		"SERVER: Linux UPnP/1.0 Sonos/70.3\r\n\r\n"
	if _, ok := discovery.ParseSearchResponse([]byte(other), "192.168.1.10", 8080); ok {
		t.Fatal("a non-Canon responder should be ignored")
	}
}

func TestParseSearchResponseAdoptsCCAPIPort(t *testing.T) {
	resp := "HTTP/1.1 200 OK\r\n" +
		"LOCATION: http://10.0.0.8:8080/ccapi\r\n" +
		"SERVER: Canon Digital Camera\r\n\r\n"
	cam, ok := discovery.ParseSearchResponse([]byte(resp), "10.0.0.8", 8080)
	if !ok {
		t.Fatal("want a match")
	}
	if cam.Host != "10.0.0.8" || cam.Port != 8080 {
		t.Errorf("cam = %+v", cam)
	}
}

func TestProbeAcceptsCCAPIAndRejectsOtherServers(t *testing.T) {
	cam := cameratest.New()
	defer cam.Close()
	host, port := cam.HostPort()

	ctx := context.Background()
	if !discovery.Probe(ctx, host, port, time.Second) {
		t.Error("Probe should accept a CCAPI camera")
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer plain.Close()
	otherHost, otherPort := hostPort(t, plain.URL)
	if discovery.Probe(ctx, otherHost, otherPort, time.Second) {
		t.Error("Probe should reject a server that does not answer /ccapi")
	}

	if discovery.Probe(ctx, "127.0.0.1", 1, 200*time.Millisecond) {
		t.Error("Probe should reject a closed port")
	}
}

func TestDiscoverReturnsNotFoundWithoutScan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// No camera on this network and scanning disabled: the error must be
	// actionable rather than a panic or an empty success.
	cams, err := discovery.Discover(ctx, discovery.Options{Timeout: 200 * time.Millisecond, Scan: false})
	if err == nil && len(cams) > 0 {
		t.Skip("a real Canon camera answered on this network")
	}
	if err == nil {
		t.Fatal("want an error when nothing is found")
	}
}

func hostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := parseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.host, u.port
}
