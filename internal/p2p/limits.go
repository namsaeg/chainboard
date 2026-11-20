package p2p

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Misbehaving peers and per-peer limits. A peer that relays what no honest
// node would (bad work, a bad signature or proof) is disconnected and its
// key and address are refused for banFor. What a peer may cost us in
// served chunks and proof-of-work checks is bounded per minute; past the
// bound its requests are ignored until the minute is over.

const (
	banFor        = time.Hour
	chunkServeMin = 512 // chunks served (or relayed) per peer per minute: 128 MiB
	limitWindow   = time.Minute
)

type peerLimits struct {
	mu     sync.Mutex
	start  time.Time
	chunks int
	checks int
}

// allow counts one event against counter; false once max is reached in
// the current minute.
func (l *peerLimits) allow(counter *int, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.start) > limitWindow {
		l.start, l.chunks, l.checks = time.Now(), 0, 0
	}
	if *counter >= max {
		return false
	}
	*counter++
	return true
}

func hashBytes(b []byte) [32]byte { return sha256.Sum256(b) }

// misbehave drops and bans a peer.
func (h *Host) misbehave(p *peer, why error) {
	h.cfg.Logf("p2p: banning peer %s for an hour: %v", p.addr, why)
	until := time.Now().Add(banFor)
	h.mu.Lock()
	h.banned["key:"+hex.EncodeToString(p.id)] = until
	if p.addr != "" {
		h.banned["addr:"+p.addr] = until
	}
	h.mu.Unlock()
	p.close()
}

// isBannedLocked reports whether a peer key or address is refused; the
// caller holds h.mu.
func (h *Host) isBannedLocked(key []byte, addr string) bool {
	now := time.Now()
	for k, until := range h.banned {
		if now.After(until) {
			delete(h.banned, k)
		}
	}
	if key != nil {
		if _, ok := h.banned["key:"+hex.EncodeToString(key)]; ok {
			return true
		}
	}
	_, ok := h.banned["addr:"+addr]
	return addr != "" && ok
}
