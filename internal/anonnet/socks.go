// Package anonnet connects through anonymity networks the user runs: a
// SOCKS5 proxy (Tor, or Nym's socks5 client) for outbound connections, and
// Tor's control port to publish this node as an onion service.
package anonnet

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Proxy is a SOCKS5 proxy.
type Proxy struct {
	Addr string // host:port
	// User and Pass authenticate; when empty a random pair is sent per
	// connection (Tor then puts each peer on its own circuit).
	User, Pass string
}

// ParseProxy reads socks5://[user:pass@]host:port.
func ParseProxy(s string) (*Proxy, error) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "socks5" && u.Scheme != "socks5h") || u.Host == "" {
		return nil, errors.New("proxy: want socks5://host:port")
	}
	p := &Proxy{Addr: u.Host}
	if u.User != nil {
		p.User = u.User.Username()
		p.Pass, _ = u.User.Password()
	}
	return p, nil
}

// IsOnion reports whether an address is a Tor onion service.
func IsOnion(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err == nil && strings.HasSuffix(strings.ToLower(host), ".onion")
}

// Dial connects to target (host:port; names are resolved by the proxy, so
// onion addresses work and no DNS query leaves this machine).
func (p *Proxy) Dial(ctx context.Context, target string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return nil, errors.New("socks5: bad port")
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", p.Addr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	} else {
		_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if err := p.handshake(c, host, port); err != nil {
		c.Close()
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}

func (p *Proxy) handshake(c net.Conn, host string, port int) error {
	user, pass := p.User, p.Pass
	if user == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		user, pass = "cb-"+hex.EncodeToString(b[:4]), hex.EncodeToString(b[4:])
	}
	if len(user) > 255 || len(pass) > 255 {
		return errors.New("socks5: credentials too long")
	}
	if _, err := c.Write([]byte{5, 2, 0x00, 0x02}); err != nil {
		return err
	}
	var rep [2]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		return err
	}
	if rep[0] != 5 {
		return errors.New("socks5: not a SOCKS5 proxy")
	}
	switch rep[1] {
	case 0x00:
	case 0x02:
		req := append([]byte{1, byte(len(user))}, user...)
		req = append(append(req, byte(len(pass))), pass...)
		if _, err := c.Write(req); err != nil {
			return err
		}
		if _, err := io.ReadFull(c, rep[:]); err != nil {
			return err
		}
		if rep[1] != 0 {
			return errors.New("socks5: authentication refused")
		}
	default:
		return errors.New("socks5: no acceptable authentication method")
	}
	req := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(append(req, 1), ip4...)
		} else {
			req = append(append(req, 4), ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("socks5: host name too long")
		}
		req = append(append(req, 3, byte(len(host))), host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := c.Write(req); err != nil {
		return err
	}
	var head [4]byte
	if _, err := io.ReadFull(c, head[:]); err != nil {
		return err
	}
	if head[0] != 5 {
		return errors.New("socks5: bad reply")
	}
	if head[1] != 0 {
		return fmt.Errorf("socks5: connect failed (code %d)", head[1])
	}
	var skip int
	switch head[3] {
	case 1:
		skip = 4
	case 4:
		skip = 16
	case 3:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return err
		}
		skip = int(l[0])
	default:
		return errors.New("socks5: bad address type")
	}
	_, err := io.ReadFull(c, make([]byte, skip+2))
	return err
}
