package nat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// UPnP IGD: find the gateway by SSDP, read its description, and call its
// WANIPConnection (or WANPPPConnection) service over SOAP.

const ssdpMulticast = "239.255.255.250:1900"

// lanClient talks to the gateway directly: never through a proxy.
func lanClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
}

func upnpMap(ctx context.Context, cfg Config, internal int, lifetime time.Duration) (*Mapping, error) {
	addr := cfg.SSDPAddr
	if addr == "" {
		addr = ssdpMulticast
	}
	locs, err := ssdpSearch(ctx, addr, cfg.timeout())
	if err != nil {
		return nil, err
	}
	client := lanClient(cfg.timeout())
	var errs []error
	for _, loc := range locs {
		m, err := upnpMapAt(ctx, client, loc, internal, lifetime)
		if err == nil {
			return m, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

func ssdpSearch(ctx context.Context, addr string, timeout time.Duration) ([]string, error) {
	dst, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, err
	}
	c, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	req := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\n" +
		"ST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\n\r\n"
	if _, err := c.WriteToUDP([]byte(req), dst); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.SetReadDeadline(deadline)
	var locs []string
	seen := map[string]bool{}
	buf := make([]byte, 2048)
	for {
		n, _, err := c.ReadFromUDP(buf)
		if err != nil {
			break
		}
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if err != nil {
			continue
		}
		loc := resp.Header.Get("Location")
		if loc != "" && !seen[loc] {
			seen[loc] = true
			locs = append(locs, loc)
		}
		if len(locs) > 0 {
			break // the first gateway is enough
		}
	}
	if len(locs) == 0 {
		return nil, errors.New("no gateway answered the search")
	}
	return locs, nil
}

type upnpDevice struct {
	Services []struct {
		Type    string `xml:"serviceType"`
		Control string `xml:"controlURL"`
	} `xml:"serviceList>service"`
	Devices []upnpDevice `xml:"deviceList>device"`
}

type upnpDesc struct {
	URLBase string     `xml:"URLBase"`
	Device  upnpDevice `xml:"device"`
}

// findWAN returns the control URL and type of the WAN connection service.
func (d *upnpDevice) findWAN() (string, string) {
	for _, s := range d.Services {
		if strings.Contains(s.Type, ":WANIPConnection:") || strings.Contains(s.Type, ":WANPPPConnection:") {
			return s.Control, s.Type
		}
	}
	for i := range d.Devices {
		if c, t := d.Devices[i].findWAN(); c != "" {
			return c, t
		}
	}
	return "", ""
}

func upnpMapAt(ctx context.Context, client *http.Client, loc string, internal int, lifetime time.Duration) (*Mapping, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var desc upnpDesc
	if err := xml.Unmarshal(body, &desc); err != nil {
		return nil, err
	}
	ctrl, st := desc.Device.findWAN()
	if ctrl == "" {
		return nil, errors.New("gateway has no WAN connection service")
	}
	base := loc
	if desc.URLBase != "" {
		base = desc.URLBase
	}
	bu, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	cu, err := bu.Parse(ctrl)
	if err != nil {
		return nil, err
	}
	control := cu.String()
	local, err := localIPFor(cu.Host)
	if err != nil {
		return nil, err
	}
	out, err := soapCall(ctx, client, control, st, "GetExternalIPAddress", nil)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(strings.TrimSpace(out["NewExternalIPAddress"]))
	if ip == nil {
		return nil, errors.New("gateway gave no external address")
	}
	add := func(lease time.Duration) error {
		_, err := soapCall(ctx, client, control, st, "AddPortMapping", [][2]string{
			{"NewRemoteHost", ""}, {"NewExternalPort", fmt.Sprint(internal)}, {"NewProtocol", "TCP"},
			{"NewInternalPort", fmt.Sprint(internal)}, {"NewInternalClient", local},
			{"NewEnabled", "1"}, {"NewPortMappingDescription", "chainboard"},
			{"NewLeaseDuration", fmt.Sprint(int(lease / time.Second))},
		})
		return err
	}
	if err := add(lifetime); err != nil {
		// Some gateways only take permanent leases.
		if err2 := add(0); err2 != nil {
			return nil, err
		}
		lifetime = 0
	}
	m := &Mapping{Method: "upnp", ExternalIP: ip, ExternalPort: internal, InternalPort: internal, Lifetime: lifetime}
	m.unmap = func(ctx context.Context) error {
		_, err := soapCall(ctx, client, control, st, "DeletePortMapping", [][2]string{
			{"NewRemoteHost", ""}, {"NewExternalPort", fmt.Sprint(internal)}, {"NewProtocol", "TCP"}})
		return err
	}
	return m, nil
}

// localIPFor is this machine's address on the route to host.
func localIPFor(host string) (string, error) {
	h := host
	if _, _, err := net.SplitHostPort(host); err != nil {
		h = net.JoinHostPort(host, "80")
	}
	c, err := net.Dial("udp4", h)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

func soapCall(ctx context.Context, client *http.Client, control, service, action string, args [][2]string) (map[string]string, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&b, `<u:%s xmlns:u="%s">`, action, service)
	for _, a := range args {
		b.WriteString("<" + a[0] + ">")
		_ = xml.EscapeText(&b, []byte(a[1]))
		b.WriteString("</" + a[0] + ">")
	}
	fmt.Fprintf(&b, `</u:%s></s:Body></s:Envelope>`, action)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, control, strings.NewReader(b.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", fmt.Sprintf(`"%s#%s"`, service, action))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", action, resp.StatusCode)
	}
	// Leaf elements of the response by local name.
	out := map[string]string{}
	d := xml.NewDecoder(bytes.NewReader(body))
	var name string
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name = t.Name.Local
		case xml.CharData:
			if name != "" {
				out[name] += string(t)
			}
		case xml.EndElement:
			name = ""
		}
	}
	return out, nil
}
