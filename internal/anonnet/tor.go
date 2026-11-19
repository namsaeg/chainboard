package anonnet

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Onion publishes a local listener as a Tor onion service through Tor's
// control port. The service lasts as long as the control connection: it
// goes away with the node.
type Onion struct {
	conn net.Conn
	rd   *bufio.Reader
	// Addr is the service's address (xxx.onion:port).
	Addr string
	// Key is the service key to pass next time for the same address
	// ("ED25519-V3:..."), set when Tor made a new one.
	Key string
}

// PublishOnion authenticates to the control port (no auth, cookie or
// password, as Tor offers) and adds an onion service forwarding virtPort
// to target. key is a previous service key ("" makes a new one).
func PublishOnion(control, password, key string, virtPort int, target string) (*Onion, error) {
	c, err := net.DialTimeout("tcp", control, 5*time.Second)
	if err != nil {
		return nil, err
	}
	o := &Onion{conn: c, rd: bufio.NewReader(c)}
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	if err := o.auth(password); err != nil {
		c.Close()
		return nil, err
	}
	spec := "NEW:ED25519-V3"
	if key != "" {
		spec = key
	}
	lines, err := o.cmd(fmt.Sprintf("ADD_ONION %s Port=%d,%s", spec, virtPort, target))
	if err != nil {
		c.Close()
		return nil, err
	}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "ServiceID="):
			o.Addr = net.JoinHostPort(strings.TrimPrefix(l, "ServiceID=")+".onion", fmt.Sprint(virtPort))
		case strings.HasPrefix(l, "PrivateKey="):
			o.Key = strings.TrimPrefix(l, "PrivateKey=")
		}
	}
	if o.Addr == "" {
		c.Close()
		return nil, errors.New("tor: no service id in reply")
	}
	_ = c.SetDeadline(time.Time{})
	return o, nil
}

// Close removes the service.
func (o *Onion) Close() error { return o.conn.Close() }

func (o *Onion) auth(password string) error {
	lines, err := o.cmd("PROTOCOLINFO 1")
	if err != nil {
		return err
	}
	var methods, cookie string
	for _, l := range lines {
		if strings.HasPrefix(l, "AUTH ") {
			for _, f := range splitFields(strings.TrimPrefix(l, "AUTH ")) {
				k, v, _ := strings.Cut(f, "=")
				switch k {
				case "METHODS":
					methods = v
				case "COOKIEFILE":
					cookie = strings.Trim(v, `"`)
				}
			}
		}
	}
	has := func(m string) bool {
		for _, x := range strings.Split(methods, ",") {
			if x == m {
				return true
			}
		}
		return false
	}
	var auth string
	switch {
	case has("NULL"):
		auth = "AUTHENTICATE"
	case has("HASHEDPASSWORD") && password != "":
		auth = "AUTHENTICATE " + quote(password)
	case has("COOKIE") && cookie != "":
		b, err := os.ReadFile(cookie)
		if err != nil {
			return fmt.Errorf("tor: reading the control cookie: %w", err)
		}
		auth = "AUTHENTICATE " + hex.EncodeToString(b)
	default:
		return fmt.Errorf("tor: no usable control authentication (offered: %s); set a control password", methods)
	}
	_, err = o.cmd(auth)
	return err
}

// cmd sends one command and returns the reply lines' text (without the
// "250-" / "250 " prefix), or the error reply.
func (o *Onion) cmd(line string) ([]string, error) {
	if _, err := o.conn.Write([]byte(line + "\r\n")); err != nil {
		return nil, err
	}
	var out []string
	for {
		l, err := o.rd.ReadString('\n')
		if err != nil {
			return nil, err
		}
		l = strings.TrimRight(l, "\r\n")
		if len(l) < 4 {
			return nil, fmt.Errorf("tor: bad reply %q", l)
		}
		code, sep, text := l[:3], l[3], l[4:]
		if code != "250" {
			return nil, fmt.Errorf("tor: %s", l)
		}
		out = append(out, text)
		if sep == ' ' {
			return out, nil
		}
	}
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// splitFields splits on spaces outside double quotes.
func splitFields(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '"' && (i == 0 || s[i-1] != '\\'):
			inQ = !inQ
			cur.WriteByte(ch)
		case ch == ' ' && !inQ:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(ch)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
