package p2p

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/namsaeg/chainboard/internal/anonnet"
)

// Outbound connections through an anonymity network, onion announcement,
// and cover traffic.

// dialTLS connects to a peer: through the proxy when there is one (always
// for onion addresses), directly otherwise.
func (h *Host) dialTLS(addr string) (*tls.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var raw net.Conn
	var err error
	switch {
	case h.cfg.Proxy != nil:
		raw, err = h.cfg.Proxy.Dial(ctx, addr)
	case anonnet.IsOnion(addr):
		return nil, errors.New("p2p: onion address without a proxy")
	default:
		d := &net.Dialer{Timeout: 5 * time.Second}
		raw, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	c := tls.Client(raw, h.tls)
	return c, nil
}

// SetOnion announces an onion service address to peers from now on.
func (h *Host) SetOnion(addr string) {
	h.mu.Lock()
	h.onion = addr
	h.mu.Unlock()
}

// Onion is the announced onion service address ("" when none).
func (h *Host) Onion() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.onion
}

// CoverConfig sets cover traffic: a frame of Size bytes to the stem peer
// every Every, carrying a piece of a stem transaction when one waits and
// nothing otherwise. An observer of the link sees the same frames whether
// or not this node sends anything. Zero Every is off.
type CoverConfig struct {
	Size  int
	Every time.Duration
}

// On reports whether cover traffic is enabled.
func (c CoverConfig) On() bool { return c.Every > 0 && c.Size >= coverMin }

// BytesPerSecond is the upload cover traffic costs (frames and their
// headers; TLS adds a little).
func (c CoverConfig) BytesPerSecond() float64 {
	if !c.On() {
		return 0
	}
	return float64(c.Size+5) / c.Every.Seconds()
}

const (
	coverMin      = 256
	padHeader     = 8 + 2 + 2 + 2 // id, part, parts, length
	maxAssemblies = 64
	assemblyLife  = 2 * time.Minute
)

type coverState struct {
	mu     sync.Mutex
	queue  [][]byte // padded frames waiting for a slot
	parts  map[*peer]map[uint64]*assembly
	sent   uint64 // frames sent
	filled uint64 // of which carried data
}

type assembly struct {
	parts [][]byte
	got   int
	at    time.Time
}

// sendStem sends a stem transaction: at once, or with cover traffic on,
// cut into frames that leave in the next slots.
func (h *Host) sendStem(p *peer, raw []byte) {
	cc := h.cfg.Cover
	if !cc.On() {
		p.send(msgStemTx, raw)
		return
	}
	room := cc.Size - padHeader
	n := (len(raw) + room - 1) / room
	var idb [8]byte
	_, _ = rand.Read(idb[:])
	id := binary.BigEndian.Uint64(idb[:])
	h.cover.mu.Lock()
	for i := 0; i < n; i++ {
		part := raw[i*room : min(len(raw), (i+1)*room)]
		f := make([]byte, cc.Size)
		binary.BigEndian.PutUint64(f, id)
		binary.BigEndian.PutUint16(f[8:], uint16(i))
		binary.BigEndian.PutUint16(f[10:], uint16(n))
		binary.BigEndian.PutUint16(f[12:], uint16(len(part)))
		copy(f[padHeader:], part)
		h.cover.queue = append(h.cover.queue, f)
	}
	h.cover.mu.Unlock()
}

// coverLoop sends one frame per slot to the stem peer.
func (h *Host) coverLoop() {
	defer h.wg.Done()
	cc := h.cfg.Cover
	t := time.NewTicker(cc.Every)
	defer t.Stop()
	for {
		select {
		case <-h.quit:
			return
		case <-t.C:
		}
		p := h.stemPeer()
		if p == nil {
			continue
		}
		h.cover.mu.Lock()
		var f []byte
		typ := msgPad
		if len(h.cover.queue) > 0 {
			f, typ = h.cover.queue[0], msgPadded
			h.cover.queue = h.cover.queue[1:]
			h.cover.filled++
		} else {
			f = make([]byte, cc.Size)
			_, _ = rand.Read(f) // looks like any frame inside TLS anyway
		}
		h.cover.sent++
		h.cover.mu.Unlock()
		p.send(typ, f)
	}
}

// CoverStats reports frames sent and how many carried data.
func (h *Host) CoverStats() (sent, filled uint64, waiting int) {
	h.cover.mu.Lock()
	defer h.cover.mu.Unlock()
	return h.cover.sent, h.cover.filled, len(h.cover.queue)
}

// receivePadded collects a stem transaction's frames; once all arrived it
// is handled like any stem transaction.
func (h *Host) receivePadded(p *peer, b []byte) error {
	if len(b) < padHeader {
		return errors.New("p2p: short padded frame")
	}
	id := binary.BigEndian.Uint64(b)
	i := int(binary.BigEndian.Uint16(b[8:]))
	n := int(binary.BigEndian.Uint16(b[10:]))
	l := int(binary.BigEndian.Uint16(b[12:]))
	if n == 0 || i >= n || padHeader+l > len(b) || n*(len(b)-padHeader) > maxMessage {
		return errors.New("p2p: bad padded frame")
	}
	h.cover.mu.Lock()
	if h.cover.parts == nil {
		h.cover.parts = map[*peer]map[uint64]*assembly{}
	}
	m := h.cover.parts[p]
	if m == nil {
		m = map[uint64]*assembly{}
		h.cover.parts[p] = m
	}
	for k, a := range m {
		if time.Since(a.at) > assemblyLife {
			delete(m, k)
		}
	}
	a := m[id]
	if a == nil {
		if len(m) >= maxAssemblies {
			h.cover.mu.Unlock()
			return nil
		}
		a = &assembly{parts: make([][]byte, n), at: time.Now()}
		m[id] = a
	}
	if len(a.parts) != n || a.parts[i] != nil {
		h.cover.mu.Unlock()
		return nil
	}
	a.parts[i] = append([]byte(nil), b[padHeader:padHeader+l]...)
	a.got++
	var raw []byte
	if a.got == n {
		delete(m, id)
		for _, part := range a.parts {
			raw = append(raw, part...)
		}
	}
	h.cover.mu.Unlock()
	if raw == nil {
		return nil
	}
	return h.handle(p, msgStemTx, raw)
}

// forgetPeer drops a closed peer's partial frames.
func (h *Host) forgetPeer(p *peer) {
	h.cover.mu.Lock()
	delete(h.cover.parts, p)
	h.cover.mu.Unlock()
}
