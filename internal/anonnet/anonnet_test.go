package anonnet

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSOCKS is a SOCKS5 proxy that resolves names through a table and
// records the credentials each connection used.
func fakeSOCKS(t *testing.T, names map[string]string, creds chan<- string) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 262)
				if _, err := io.ReadFull(c, b[:2]); err != nil {
					return
				}
				io.ReadFull(c, b[:b[1]])
				c.Write([]byte{5, 2})
				io.ReadFull(c, b[:2])
				user := make([]byte, b[1])
				io.ReadFull(c, user)
				io.ReadFull(c, b[:1])
				io.ReadFull(c, make([]byte, b[0]))
				creds <- string(user)
				c.Write([]byte{1, 0})
				io.ReadFull(c, b[:4])
				var host string
				switch b[3] {
				case 3:
					io.ReadFull(c, b[:1])
					h := make([]byte, b[0])
					io.ReadFull(c, h)
					host = string(h)
				case 1:
					ip := make([]byte, 4)
					io.ReadFull(c, ip)
					host = net.IP(ip).String()
				}
				io.ReadFull(c, b[:2])
				target := net.JoinHostPort(host, itoa(int(binary.BigEndian.Uint16(b[:2]))))
				if m, ok := names[target]; ok {
					target = m
				}
				up, err := net.Dial("tcp", target)
				if err != nil {
					c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				go io.Copy(up, c)
				io.Copy(c, up)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestSOCKS5(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	creds := make(chan string, 4)
	proxy := fakeSOCKS(t, map[string]string{"abcdefg.onion:7700": echo.Addr().String()}, creds)
	p, err := ParseProxy("socks5://" + proxy)
	if err != nil {
		t.Fatal(err)
	}
	if !IsOnion("abcdefg.onion:7700") || IsOnion("1.2.3.4:7700") {
		t.Fatal("IsOnion")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var users []string
	for i := 0; i < 2; i++ {
		c, err := p.Dial(ctx, "abcdefg.onion:7700")
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("안녕"))
		buf := make([]byte, len("안녕"))
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "안녕" {
			t.Fatalf("echo through the proxy: %q %v", buf, err)
		}
		c.Close()
		users = append(users, <-creds)
	}
	if users[0] == users[1] {
		t.Fatal("connections share credentials (and so a Tor circuit)")
	}
	if _, err := p.Dial(ctx, "nowhere.onion:1"); err == nil {
		t.Fatal("failed connect reported as success")
	}
}

// fakeTor is a control port with cookie authentication.
func fakeTor(t *testing.T) (string, *[]string) {
	dir := t.TempDir()
	cookie := []byte("0123456789abcdef0123456789abcdef")
	path := filepath.Join(dir, "control_auth_cookie")
	os.WriteFile(path, cookie, 0o600)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var got []string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				rd := bufio.NewReader(c)
				authed := false
				for {
					l, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					l = strings.TrimSpace(l)
					got = append(got, l)
					switch {
					case l == "PROTOCOLINFO 1":
						c.Write([]byte("250-PROTOCOLINFO 1\r\n250-AUTH METHODS=COOKIE,SAFECOOKIE COOKIEFILE=\"" + path + "\"\r\n250-VERSION Tor=\"0.4.8\"\r\n250 OK\r\n"))
					case strings.HasPrefix(l, "AUTHENTICATE "):
						if strings.TrimPrefix(l, "AUTHENTICATE ") == hex.EncodeToString(cookie) {
							authed = true
							c.Write([]byte("250 OK\r\n"))
						} else {
							c.Write([]byte("515 Authentication failed\r\n"))
						}
					case strings.HasPrefix(l, "ADD_ONION ") && authed:
						if strings.Contains(l, "NEW:ED25519-V3") {
							c.Write([]byte("250-ServiceID=abcdefghijklmnop\r\n250-PrivateKey=ED25519-V3:c2VjcmV0\r\n250 OK\r\n"))
						} else {
							c.Write([]byte("250-ServiceID=abcdefghijklmnop\r\n250 OK\r\n"))
						}
					default:
						c.Write([]byte("510 Unrecognized command\r\n"))
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String(), &got
}

func TestOnion(t *testing.T) {
	ctl, got := fakeTor(t)
	o, err := PublishOnion(ctl, "", "", 7700, "127.0.0.1:7700")
	if err != nil {
		t.Fatal(err)
	}
	if o.Addr != "abcdefghijklmnop.onion:7700" || o.Key != "ED25519-V3:c2VjcmV0" {
		t.Fatalf("onion %+v", o)
	}
	o.Close()
	o, err = PublishOnion(ctl, "", o.Key, 7700, "127.0.0.1:7700")
	if err != nil || o.Addr != "abcdefghijklmnop.onion:7700" || o.Key != "" {
		t.Fatalf("republish: %+v %v", o, err)
	}
	o.Close()
	last := (*got)[len(*got)-1]
	if !strings.HasPrefix(last, "ADD_ONION ED25519-V3:c2VjcmV0 Port=7700,127.0.0.1:7700") {
		t.Fatalf("second ADD_ONION: %q", last)
	}
}
