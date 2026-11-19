package nat

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// NAT-PMP (RFC 6886): two UDP exchanges with the gateway on port 5351.
func pmpMap(ctx context.Context, gw string, internal int, lifetime, timeout time.Duration) (*Mapping, error) {
	c, err := net.Dial("udp4", gw)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	resp, err := pmpCall(ctx, c, []byte{0, 0}, 12, 128, timeout)
	if err != nil {
		return nil, err
	}
	ip := net.IPv4(resp[8], resp[9], resp[10], resp[11])
	resp, err = pmpCall(ctx, c, pmpRequest(internal, internal, lifetime), 16, 130, timeout)
	if err != nil {
		return nil, err
	}
	m := &Mapping{Method: "nat-pmp", ExternalIP: ip, InternalPort: internal,
		ExternalPort: int(binary.BigEndian.Uint16(resp[10:12])),
		Lifetime:     time.Duration(binary.BigEndian.Uint32(resp[12:16])) * time.Second}
	m.unmap = func(ctx context.Context) error {
		c, err := net.Dial("udp4", gw)
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = pmpCall(ctx, c, pmpRequest(internal, 0, 0), 16, 130, timeout)
		return err
	}
	return m, nil
}

func pmpRequest(internal, external int, lifetime time.Duration) []byte {
	b := make([]byte, 12)
	b[1] = 2 // map TCP
	binary.BigEndian.PutUint16(b[4:], uint16(internal))
	binary.BigEndian.PutUint16(b[6:], uint16(external))
	binary.BigEndian.PutUint32(b[8:], uint32(lifetime/time.Second))
	return b
}

// pmpCall sends a request, resending with doubling waits, and checks the
// reply's size, opcode and result code.
func pmpCall(ctx context.Context, c net.Conn, req []byte, size int, op byte, timeout time.Duration) ([]byte, error) {
	wait := 250 * time.Millisecond
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 64)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := c.Write(req); err != nil {
			return nil, err
		}
		_ = c.SetReadDeadline(minTime(time.Now().Add(wait), deadline))
		n, err := c.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				wait *= 2
				continue
			}
			return nil, err
		}
		if n < size || buf[0] != 0 || buf[1] != op {
			continue
		}
		if code := binary.BigEndian.Uint16(buf[2:4]); code != 0 {
			return nil, fmt.Errorf("result code %d", code)
		}
		return buf[:n], nil
	}
	return nil, errors.New("no answer")
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
