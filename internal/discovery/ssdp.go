// Package discovery finds CCAPI cameras on the local network.
//
// Two strategies are used, in order:
//
//  1. SSDP M-SEARCH on every multicast-capable interface. This is how Canon's
//     own apps find the camera and is nearly instant.
//  2. A bounded TCP probe of the local /24s. Multicast is frequently dropped by
//     Docker bridge networks, VLANs and some consumer access points, and the
//     probe keeps the daemon working there without extra configuration.
package discovery

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ssdpAddr = "239.255.255.250:1900"
	// canonST is the service type Canon bodies answer to. rootdevice is
	// searched as well because firmware varies in what it advertises.
	canonST = "urn:schemas-canon-com:service:ICPO-SmartPhoneEOSSystemService:1"
)

// ErrNotFound is returned when no camera answered.
var ErrNotFound = errors.New("discovery: no CCAPI camera found")

// Camera is a discovered CCAPI endpoint.
type Camera struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	// Name is whatever the device called itself in SSDP, if anything.
	Name string `json:"name,omitempty"`
	// Source is "ssdp" or "scan".
	Source string `json:"source"`
}

// Addr returns host:port.
func (c Camera) Addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// Options tunes a search.
type Options struct {
	// Timeout bounds the SSDP listen window.
	Timeout time.Duration
	// Port is the CCAPI port to assume and to probe. 0 means 8080.
	Port int
	// Scan enables the subnet probe fallback when SSDP finds nothing.
	Scan bool
	// ScanTimeout bounds a single TCP probe during the fallback scan.
	ScanTimeout time.Duration
}

func (o *Options) setDefaults() {
	if o.Timeout <= 0 {
		o.Timeout = 3 * time.Second
	}
	if o.Port == 0 {
		o.Port = 8080
	}
	if o.ScanTimeout <= 0 {
		o.ScanTimeout = 400 * time.Millisecond
	}
}

// Discover searches for cameras, returning them in a stable order.
func Discover(ctx context.Context, opt Options) ([]Camera, error) {
	opt.setDefaults()

	cams, err := SearchSSDP(ctx, opt)
	if err == nil && len(cams) > 0 {
		return cams, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !opt.Scan {
		if err != nil {
			return nil, fmt.Errorf("%w (SSDP: %v)", ErrNotFound, err)
		}
		return nil, ErrNotFound
	}

	cams, scanErr := ScanLocalSubnets(ctx, opt)
	if len(cams) > 0 {
		return cams, nil
	}
	if scanErr != nil {
		return nil, fmt.Errorf("%w (scan: %v)", ErrNotFound, scanErr)
	}
	return nil, ErrNotFound
}

// SearchSSDP sends an M-SEARCH on every usable interface and collects Canon
// responders until the timeout expires.
func SearchSSDP(ctx context.Context, opt Options) ([]Camera, error) {
	opt.setDefaults()

	ifaces, err := multicastInterfaces()
	if err != nil {
		return nil, err
	}
	// A nil entry means "let the OS choose", which covers hosts where interface
	// enumeration is unavailable (some container runtimes).
	ifaces = append(ifaces, nil)

	var (
		mu   sync.Mutex
		seen = map[string]Camera{}
		wg   sync.WaitGroup
		errs []error
	)
	for _, ifi := range ifaces {
		wg.Add(1)
		go func(ifi *net.Interface) {
			defer wg.Done()
			found, err := searchOn(ctx, ifi, opt)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			for _, c := range found {
				if _, dup := seen[c.Host]; !dup {
					seen[c.Host] = c
				}
			}
		}(ifi)
	}
	wg.Wait()

	if len(seen) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return sortedCameras(seen), nil
}

func multicastInterfaces() ([]*net.Interface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []*net.Interface
	for i := range all {
		ifi := all[i]
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if !hasIPv4(&ifi) {
			continue
		}
		out = append(out, &ifi)
	}
	return out, nil
}

func hasIPv4(ifi *net.Interface) bool {
	addrs, err := ifi.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			return true
		}
	}
	return false
}

func searchOn(ctx context.Context, ifi *net.Interface, opt Options) ([]Camera, error) {
	raddr, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		return nil, err
	}
	laddr := &net.UDPAddr{IP: net.IPv4zero}
	if ifi != nil {
		if ip := firstIPv4(ifi); ip != nil {
			laddr = &net.UDPAddr{IP: ip}
		}
	}
	conn, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	mx := int(opt.Timeout.Seconds())
	if mx < 1 {
		mx = 1
	}
	for _, st := range []string{canonST, "upnp:rootdevice"} {
		msg := "M-SEARCH * HTTP/1.1\r\n" +
			"HOST: " + ssdpAddr + "\r\n" +
			"MAN: \"ssdp:discover\"\r\n" +
			"MX: " + strconv.Itoa(mx) + "\r\n" +
			"ST: " + st + "\r\n\r\n"
		if _, err := conn.WriteTo([]byte(msg), raddr); err != nil {
			return nil, err
		}
	}

	deadline := time.Now().Add(opt.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)

	// Unblock the read when the caller cancels.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-stop:
		}
	}()

	seen := map[string]Camera{}
	buf := make([]byte, 8192)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // deadline or cancellation
		}
		cam, ok := ParseSearchResponse(buf[:n], from.IP.String(), opt.Port)
		if !ok {
			continue
		}
		if _, dup := seen[cam.Host]; !dup {
			seen[cam.Host] = cam
		}
	}
	return sortedCameras(seen), nil
}

func firstIPv4(ifi *net.Interface) net.IP {
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip4 := ipn.IP.To4(); ip4 != nil {
				return ip4
			}
		}
	}
	return nil
}

// ParseSearchResponse decides whether an SSDP reply came from a Canon camera
// and extracts its address. from is the sender's IP, used when the response
// carries no usable LOCATION.
func ParseSearchResponse(payload []byte, from string, defaultPort int) (Camera, bool) {
	headers := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(payload)))
	for sc.Scan() {
		line := sc.Text()
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}

	blob := strings.ToLower(string(payload))
	if !strings.Contains(blob, "canon") {
		return Camera{}, false
	}

	cam := Camera{Host: from, Port: defaultPort, Source: "ssdp"}
	if loc := headers["location"]; loc != "" {
		if u, err := url.Parse(loc); err == nil && u.Hostname() != "" {
			cam.Host = u.Hostname()
			// The description document lives on a different port than CCAPI, so
			// only adopt the port when it already looks like CCAPI.
			if p, err := strconv.Atoi(u.Port()); err == nil && p == defaultPort {
				cam.Port = p
			}
		}
	}
	if s := headers["server"]; s != "" {
		cam.Name = s
	}
	if cam.Host == "" {
		return Camera{}, false
	}
	return cam, true
}

// Probe reports whether a CCAPI camera answers at host:port.
func Probe(ctx context.Context, host string, port int, timeout time.Duration) bool {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	conn.Close()

	// A listening port is not proof: confirm it speaks CCAPI.
	reqCtx, cancelReq := context.WithTimeout(ctx, timeout*2)
	defer cancelReq()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		fmt.Sprintf("http://%s/ccapi", net.JoinHostPort(host, strconv.Itoa(port))), nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: timeout * 2}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode/100 == 2
}

// ScanLocalSubnets probes every address in the /24 around each local IPv4
// address. Larger prefixes are clamped to a /24 so a misconfigured netmask
// cannot turn discovery into a 65k-host sweep.
func ScanLocalSubnets(ctx context.Context, opt Options) ([]Camera, error) {
	opt.setDefaults()

	hosts, err := localScanTargets()
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, errors.New("discovery: no local IPv4 networks to scan")
	}

	const workers = 64
	jobs := make(chan string)
	var (
		mu    sync.Mutex
		found = map[string]Camera{}
		wg    sync.WaitGroup
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for host := range jobs {
				if ctx.Err() != nil {
					return
				}
				if !Probe(ctx, host, opt.Port, opt.ScanTimeout) {
					continue
				}
				mu.Lock()
				found[host] = Camera{Host: host, Port: opt.Port, Source: "scan"}
				mu.Unlock()
			}
		}()
	}
	for _, h := range hosts {
		select {
		case jobs <- h:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()

	return sortedCameras(found), ctx.Err()
}

// localScanTargets lists the addresses worth probing: every host in the /24
// containing one of our own IPv4 addresses, minus our own addresses.
func localScanTargets() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	self := map[string]bool{}
	nets := map[string]net.IP{}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil {
				continue
			}
			self[ip4.String()] = true
			base := ip4.Mask(net.CIDRMask(24, 32))
			nets[base.String()] = base
		}
	}

	var hosts []string
	for _, base := range nets {
		for i := 1; i < 255; i++ {
			ip := net.IPv4(base[0], base[1], base[2], byte(i)).String()
			if !self[ip] {
				hosts = append(hosts, ip)
			}
		}
	}
	sort.Strings(hosts)
	return hosts, nil
}

func sortedCameras(m map[string]Camera) []Camera {
	out := make([]Camera, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}
