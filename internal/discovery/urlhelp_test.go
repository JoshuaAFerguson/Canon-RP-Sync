package discovery_test

import (
	"net/url"
	"strconv"
)

type hostPortPair struct {
	host string
	port int
}

func parseURL(raw string) (hostPortPair, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return hostPortPair{}, err
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return hostPortPair{}, err
	}
	return hostPortPair{host: u.Hostname(), port: port}, nil
}
