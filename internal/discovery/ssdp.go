// Package discovery finds CCAPI cameras on the local network via SSDP.
package discovery

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

const ssdpAddr = "239.255.255.250:1900"

// Camera is a discovered CCAPI endpoint.
type Camera struct {
	Host string
	Port int
}

// Discover sends an SSDP M-SEARCH and returns any responders that look like a
// Canon camera. It stops after timeout or when ctx is cancelled.
func Discover(ctx context.Context, timeout time.Duration) ([]Camera, error) {
	raddr, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	msg := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: " + ssdpAddr + "\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 2\r\n" +
		"ST: upnp:rootdevice\r\n\r\n"
	if _, err := conn.WriteTo([]byte(msg), raddr); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)

	seen := map[string]bool{}
	var cams []Camera
	buf := make([]byte, 4096)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // timeout
		}
		resp := strings.ToLower(string(buf[:n]))
		if !strings.Contains(resp, "canon") {
			continue
		}
		host := from.IP.String()
		if seen[host] {
			continue
		}
		seen[host] = true
		cams = append(cams, Camera{Host: host, Port: 8080})
	}
	if len(cams) == 0 {
		return nil, fmt.Errorf("no Canon cameras responded to SSDP within %s", timeout)
	}
	return cams, nil
}
