package p2p

import (
	mrand "math/rand/v2"
	"time"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/merkle"
)

// Relayed chunk requests. A node behind NAT accepts no connections, so a
// node that wants its data may have no direct link to it; their common
// peers pass the request on and the chunk back. A request carries a hop
// budget: a peer without the chunk asks up to relayFanout of its own peers
// with one hop less, remembers who asked for relayLife, and forwards the
// first verified chunk that comes back to them.
const (
	relayTTL    = 1 // hops a re-asked request may take beyond the first peer
	relayFanout = 4
	relayLife   = 30 * time.Second
	maxRelays   = 4096
)

type relayKey struct {
	root Hash
	idx  uint64
}

type relay struct {
	askers []*peer
	at     time.Time
}

func encodeGetChunk(root Hash, idx uint64, ttl uint8) []byte {
	var w codec.Writer
	w.Fixed(root[:])
	w.Var(idx)
	w.U8(ttl)
	return w.B
}

// relayGet passes on a chunk request we cannot serve.
func (h *Host) relayGet(from *peer, root Hash, idx uint64, ttl uint8) {
	if ttl == 0 {
		return
	}
	k := relayKey{root, idx}
	h.mu.Lock()
	if r, ok := h.relays[k]; ok && time.Since(r.at) < relayLife {
		for _, a := range r.askers {
			if a == from {
				h.mu.Unlock()
				return
			}
		}
		r.askers = append(r.askers, from) // already asked our peers
		h.mu.Unlock()
		return
	}
	if len(h.relays) >= maxRelays {
		h.mu.Unlock()
		return
	}
	h.relays[k] = &relay{askers: []*peer{from}, at: time.Now()}
	var ps []*peer
	for p := range h.peers {
		if p != from {
			ps = append(ps, p)
		}
	}
	h.mu.Unlock()
	mrand.Shuffle(len(ps), func(i, j int) { ps[i], ps[j] = ps[j], ps[i] })
	msg := encodeGetChunk(root, idx, ttl-1)
	for _, p := range ps[:min(len(ps), relayFanout)] {
		p.send(msgGetChunk, msg)
	}
}

// relayChunk hands a chunk to the peers we asked for it on behalf of, once
// it verifies against its root.
func (h *Host) relayChunk(root Hash, idx, size uint64, c []byte, path merkle.Proof, raw []byte) {
	k := relayKey{root, idx}
	h.mu.Lock()
	r, ok := h.relays[k]
	if ok {
		delete(h.relays, k)
	}
	h.mu.Unlock()
	if !ok || time.Since(r.at) > relayLife || !chunk.VerifyChunk(root, size, idx, c, path) {
		return
	}
	for _, a := range r.askers {
		a.send(msgChunk, raw)
	}
}

func (h *Host) expireRelays() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, r := range h.relays {
		if time.Since(r.at) > relayLife {
			delete(h.relays, k)
		}
	}
}
