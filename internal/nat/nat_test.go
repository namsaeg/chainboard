package nat

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePMP answers NAT-PMP like a home router with external address
// 203.0.113.7 that maps internal port p to external p+1.
func fakePMP(t *testing.T) (string, *sync.Map) {
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	maps := &sync.Map{}
	go func() {
		buf := make([]byte, 64)
		for {
			n, from, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			switch {
			case n == 2 && buf[1] == 0:
				r := make([]byte, 12)
				r[1] = 128
				copy(r[8:], []byte{203, 0, 113, 7})
				_, _ = c.WriteTo(r, from)
			case n == 12 && buf[1] == 2:
				in := binary.BigEndian.Uint16(buf[4:])
				life := binary.BigEndian.Uint32(buf[8:])
				r := make([]byte, 16)
				r[1] = 130
				binary.BigEndian.PutUint16(r[8:], in)
				ext := in + 1
				if life == 0 {
					maps.Delete(in)
					ext = 0
				} else {
					maps.Store(in, ext)
				}
				binary.BigEndian.PutUint16(r[10:], ext)
				binary.BigEndian.PutUint32(r[12:], life)
				_, _ = c.WriteTo(r, from)
			}
		}
	}()
	return c.LocalAddr().String(), maps
}

func TestPMP(t *testing.T) {
	gw, maps := fakePMP(t)
	ctx := context.Background()
	m, err := Map(ctx, Config{PMPGateways: []string{gw}, NoUPnP: true, Timeout: time.Second}, 7700, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if m.Method != "nat-pmp" || m.Addr() != "203.0.113.7:7701" || m.Lifetime != time.Hour {
		t.Fatalf("mapping %+v (%s)", m, m.Addr())
	}
	if _, ok := maps.Load(uint16(7700)); !ok {
		t.Fatal("gateway has no mapping")
	}
	if err := m.Unmap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := maps.Load(uint16(7700)); ok {
		t.Fatal("mapping not removed")
	}
	// Nobody there: fails within the timeout.
	dead, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	addr := dead.LocalAddr().String()
	dead.Close()
	if _, err := Map(ctx, Config{PMPGateways: []string{addr}, NoUPnP: true, Timeout: 600 * time.Millisecond}, 7700, time.Hour); err == nil {
		t.Fatal("mapped with no gateway")
	}
}

// fakeIGD is a UPnP router: SSDP on a local UDP port, description and SOAP
// control over HTTP.
func fakeIGD(t *testing.T) (string, *sync.Map) {
	maps := &sync.Map{}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/desc.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0"?><root xmlns="urn:schemas-upnp-org:device-1-0"><device>
<deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType><deviceList><device>
<deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType><deviceList><device>
<deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType><serviceList><service>
<serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType><controlURL>/ctl</controlURL>
</service></serviceList></device></deviceList></device></deviceList></device></root>`)
	})
	mux.HandleFunc("/ctl", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		action := r.Header.Get("SOAPAction")
		reply := func(inner string) {
			fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>%s</s:Body></s:Envelope>`, inner)
		}
		field := func(name string) string {
			i := strings.Index(body, "<"+name+">")
			j := strings.Index(body, "</"+name+">")
			if i < 0 || j < 0 {
				return ""
			}
			return body[i+len(name)+2 : j]
		}
		switch {
		case strings.Contains(action, "#GetExternalIPAddress"):
			reply(`<u:GetExternalIPAddressResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"><NewExternalIPAddress>198.51.100.4</NewExternalIPAddress></u:GetExternalIPAddressResponse>`)
		case strings.Contains(action, "#AddPortMapping"):
			if field("NewLeaseDuration") != "0" { // this router only takes permanent leases
				w.WriteHeader(500)
				return
			}
			maps.Store(field("NewExternalPort"), field("NewInternalClient"))
			reply(`<u:AddPortMappingResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"/>`)
		case strings.Contains(action, "#DeletePortMapping"):
			maps.Delete(field("NewExternalPort"))
			reply(`<u:DeletePortMappingResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"/>`)
		default:
			w.WriteHeader(500)
		}
	})
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			if strings.HasPrefix(string(buf[:n]), "M-SEARCH") {
				_, _ = c.WriteTo([]byte("HTTP/1.1 200 OK\r\nST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\nLOCATION: "+
					srv.URL+"/desc.xml\r\n\r\n"), from)
			}
		}
	}()
	return c.LocalAddr().String(), maps
}

func TestUPnP(t *testing.T) {
	ssdp, maps := fakeIGD(t)
	ctx := context.Background()
	m, err := Map(ctx, Config{NoPMP: true, SSDPAddr: ssdp, Timeout: 2 * time.Second}, 7700, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if m.Method != "upnp" || m.Addr() != "198.51.100.4:7700" || m.Lifetime != 0 {
		t.Fatalf("mapping %+v (%s)", m, m.Addr())
	}
	if v, ok := maps.Load("7700"); !ok || v.(string) != "127.0.0.1" {
		t.Fatalf("gateway mapping: %v", v)
	}
	if err := m.Unmap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := maps.Load("7700"); ok {
		t.Fatal("mapping not removed")
	}
}
