package p2p

import (
	"time"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/merkle"
)

// A probe asks peers for a few chunks of data that is not on chain (a
// funding request's data, say) and keeps the ones that verify against the
// root, without storing them: a backer can check that the data exists and
// look at part of it before pledging.
type probe struct {
	size   uint64
	want   map[uint64]bool
	got    map[uint64][]byte
	at     time.Time
	sentAt time.Time
}

const probeLife = 10 * time.Minute

// Probe starts (or restarts) a spot check of chunks idx of a data root.
func (h *Host) Probe(root Hash, size uint64, idx []uint64) {
	h.mu.Lock()
	for r, p := range h.probes {
		if time.Since(p.at) > probeLife {
			delete(h.probes, r)
		}
	}
	pr := &probe{size: size, want: map[uint64]bool{}, got: map[uint64][]byte{}, at: time.Now()}
	for _, i := range idx {
		pr.want[i] = true
	}
	h.probes[root] = pr
	h.mu.Unlock()
	h.askProbe(root)
}

// Probing reports whether a spot check of root is under way (or recent).
func (h *Host) Probing(root Hash) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.probes[root]
	return ok && time.Since(p.at) <= probeLife
}

// askProbe asks every peer for the probe's chunks that have not arrived,
// at most once every few seconds (peers may connect after the probe began).
func (h *Host) askProbe(root Hash) {
	h.mu.Lock()
	pr, ok := h.probes[root]
	if !ok || time.Since(pr.sentAt) < 3*time.Second {
		h.mu.Unlock()
		return
	}
	ttl := uint8(0)
	if !pr.sentAt.IsZero() {
		ttl = relayTTL // asked before and still missing: let peers ask theirs
	}
	pr.sentAt = time.Now()
	var idx []uint64
	for i := range pr.want {
		if pr.got[i] == nil {
			idx = append(idx, i)
		}
	}
	var ps []*peer
	for p := range h.peers {
		ps = append(ps, p)
	}
	h.mu.Unlock()
	for _, i := range idx {
		msg := encodeGetChunk(root, i, ttl)
		for _, p := range ps {
			p.send(msgGetChunk, msg)
		}
	}
}

// ProbeResult returns the chunks of a probe that arrived and verified, and
// asks again for the rest.
func (h *Host) ProbeResult(root Hash) map[uint64][]byte {
	h.askProbe(root)
	h.mu.Lock()
	defer h.mu.Unlock()
	pr, ok := h.probes[root]
	if !ok {
		return nil
	}
	out := make(map[uint64][]byte, len(pr.got))
	for i, c := range pr.got {
		out[i] = c
	}
	return out
}

func (h *Host) probed(root Hash, idx, size uint64, c []byte, path merkle.Proof) {
	h.mu.Lock()
	defer h.mu.Unlock()
	pr, ok := h.probes[root]
	if !ok || !pr.want[idx] || pr.size != size || pr.got[idx] != nil {
		return
	}
	if chunk.VerifyChunk(root, size, idx, c, path) {
		pr.got[idx] = append([]byte(nil), c...)
	}
}
