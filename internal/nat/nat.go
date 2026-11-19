// Package nat asks the home router to forward a TCP port to this machine,
// so a node behind NAT can accept connections: NAT-PMP (RFC 6886) first,
// then UPnP IGD. Both only talk to the local network.
package nat

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// Mapping is a port the gateway forwards to us.
type Mapping struct {
	Method       string // "nat-pmp" or "upnp"
	ExternalIP   net.IP
	ExternalPort int
	InternalPort int
	Lifetime     time.Duration // 0: until removed
	unmap        func(context.Context) error
}

// Addr is the mapping's address as seen from the internet.
func (m *Mapping) Addr() string {
	return net.JoinHostPort(m.ExternalIP.String(), fmt.Sprint(m.ExternalPort))
}

// Unmap asks the gateway to remove the mapping.
func (m *Mapping) Unmap(ctx context.Context) error {
	if m.unmap == nil {
		return nil
	}
	return m.unmap(ctx)
}

// Config points the search at particular gateways (tests). Zero values
// mean: guess the gateways and search the standard SSDP multicast address.
type Config struct {
	PMPGateways []string      // host:port of NAT-PMP gateways
	SSDPAddr    string        // where the UPnP search is sent
	Timeout     time.Duration // per step
	NoPMP       bool
	NoUPnP      bool
}

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 2 * time.Second
}

// Map forwards the internal TCP port (to the same external port when the
// gateway allows) for lifetime, trying NAT-PMP, then UPnP.
func Map(ctx context.Context, cfg Config, internalPort int, lifetime time.Duration) (*Mapping, error) {
	var errs []error
	if !cfg.NoPMP {
		gws := cfg.PMPGateways
		if len(gws) == 0 {
			for _, ip := range guessGateways() {
				gws = append(gws, net.JoinHostPort(ip.String(), "5351"))
			}
		}
		for _, gw := range gws {
			m, err := pmpMap(ctx, gw, internalPort, lifetime, cfg.timeout())
			if err == nil {
				return m, nil
			}
			errs = append(errs, fmt.Errorf("nat-pmp %s: %w", gw, err))
		}
	}
	if !cfg.NoUPnP {
		m, err := upnpMap(ctx, cfg, internalPort, lifetime)
		if err == nil {
			return m, nil
		}
		errs = append(errs, fmt.Errorf("upnp: %w", err))
	}
	if len(errs) == 0 {
		return nil, errors.New("nat: no method enabled")
	}
	return nil, errors.Join(errs...)
}

// guessGateways lists likely gateways: the default route's (Linux), and
// x.y.z.1 of each private IPv4 network this machine is on.
func guessGateways() []net.IP {
	var out []net.IP
	seen := map[string]bool{}
	add := func(ip net.IP) {
		if ip != nil && !seen[ip.String()] {
			seen[ip.String()] = true
			out = append(out, ip)
		}
	}
	add(defaultGateway())
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := n.IP.To4()
		if ip == nil || !ip.IsPrivate() {
			continue
		}
		add(net.IPv4(ip[0], ip[1], ip[2], 1))
	}
	return out
}
