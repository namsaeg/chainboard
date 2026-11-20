package p2p

import (
	"errors"
	mrand "math/rand/v2"
	"time"

	"github.com/namsaeg/chainboard/internal/codec"
)

// Gossiped items: funding requests and bounty offers travel between nodes
// before anything carries them on chain, so every node can list them. Each
// carries a memory-hard proof of work (checked by the node). A node's own
// items leave through the stem like its transactions, so the first node to
// broadcast one is not, as a rule, the one that made it.

// Item kinds.
const (
	ItemFund  uint8 = 1 // a funding request
	ItemOffer uint8 = 2 // a bounty offer
)

// ErrMisbehave marks an item (or transaction) that no honest node relays:
// bad work, a bad signature, a malformed encoding. The sending peer is
// disconnected and banned for a while.
var ErrMisbehave = errors.New("p2p: invalid item")

// Gossip is the node's pool of gossiped items. An item's wire form is its
// encoding and its proof-of-work nonce.
type Gossip interface {
	ItemIDs(kind uint8) []Hash
	ItemWire(kind uint8, id Hash) ([]byte, bool)
	// AddItem checks and keeps an item; isNew is true when it was not held
	// (with its work) before, so it is passed on.
	AddItem(kind uint8, wire []byte) (id Hash, isNew bool, err error)
}

const (
	maxItemIDs     = 8192
	maxItemsPerMsg = 256
	maxItemWire    = 4096
	itemChecksMin  = 120 // proof-of-work checks a peer may cost us per minute
)

func encodeIDs(kind uint8, ids []Hash) []byte {
	var w codec.Writer
	w.U8(kind)
	w.Var(uint64(len(ids)))
	for _, id := range ids {
		w.Fixed(id[:])
	}
	return w.B
}

func decodeIDs(b []byte) (uint8, []Hash, error) {
	r := codec.NewReader(b)
	kind := r.U8()
	n := r.Count(maxItemIDs)
	ids := make([]Hash, 0, n)
	for i := 0; i < n && r.Err == nil; i++ {
		var id Hash
		r.Fixed(id[:])
		ids = append(ids, id)
	}
	return kind, ids, r.Done()
}

func encodeItems(kind uint8, wires [][]byte) []byte {
	var w codec.Writer
	w.U8(kind)
	w.Var(uint64(len(wires)))
	for _, x := range wires {
		w.Bytes(x)
	}
	return w.B
}

func (h *Host) sendItemInv(p *peer) {
	if h.cfg.Gossip == nil {
		return
	}
	for _, kind := range []uint8{ItemFund, ItemOffer} {
		if ids := h.cfg.Gossip.ItemIDs(kind); len(ids) > 0 {
			p.send(msgItemInv, encodeIDs(kind, ids[:min(len(ids), maxItemIDs)]))
		}
	}
}

// SubmitItem sends one of our own items into the stem.
func (h *Host) SubmitItem(kind uint8, wire []byte) {
	next := h.stemPeer()
	if next == nil {
		h.broadcast(msgItems, encodeItems(kind, [][]byte{wire}), nil)
		return
	}
	var w codec.Writer
	w.U8(kind)
	w.Bytes(wire)
	h.armItemEmbargo(kind, wire)
	next.send(msgStemItem, w.B)
}

// armItemEmbargo broadcasts an item ourselves if it does not come back as
// fluff in time (a stem peer that drops it cannot censor it).
func (h *Host) armItemEmbargo(kind uint8, wire []byte) {
	id := itemKey(kind, wire)
	d := h.cfg.Embargo
	if d == 0 {
		d = embargoMin + time.Duration(mrand.Int64N(int64(embargoJitter)))
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.itemEmb[id]; ok {
		return
	}
	h.itemEmb[id] = time.AfterFunc(d, func() {
		h.mu.Lock()
		delete(h.itemEmb, id)
		h.mu.Unlock()
		h.broadcast(msgItems, encodeItems(kind, [][]byte{wire}), nil)
	})
}

func (h *Host) cancelItemEmbargo(kind uint8, wire []byte) {
	id := itemKey(kind, wire)
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.itemEmb[id]; ok {
		t.Stop()
		delete(h.itemEmb, id)
	}
}

func itemKey(kind uint8, wire []byte) Hash {
	var k Hash
	k[0] = kind
	d := Hash(hashBytes(wire))
	copy(k[1:], d[:31])
	return k
}

// addItem checks an item from a peer, counting the work checks it costs.
func (h *Host) addItem(p *peer, kind uint8, wire []byte) (bool, error) {
	if !p.limits.allow(&p.limits.checks, itemChecksMin) {
		return false, nil // too many from this peer this minute: ignore
	}
	_, isNew, err := h.cfg.Gossip.AddItem(kind, wire)
	if errors.Is(err, ErrMisbehave) {
		return false, err
	}
	return isNew && err == nil, nil
}

func (h *Host) handleItems(p *peer, typ uint8, b []byte) error {
	g := h.cfg.Gossip
	if g == nil {
		return nil
	}
	switch typ {
	case msgItemInv:
		kind, ids, err := decodeIDs(b)
		if err != nil {
			return err
		}
		var want []Hash
		for _, id := range ids {
			if _, ok := g.ItemWire(kind, id); !ok {
				want = append(want, id)
			}
		}
		for len(want) > 0 {
			k := min(len(want), maxItemsPerMsg)
			p.send(msgGetItems, encodeIDs(kind, want[:k]))
			want = want[k:]
		}
	case msgGetItems:
		kind, ids, err := decodeIDs(b)
		if err != nil {
			return err
		}
		var wires [][]byte
		for _, id := range ids[:min(len(ids), maxItemsPerMsg)] {
			if x, ok := g.ItemWire(kind, id); ok {
				wires = append(wires, x)
			}
		}
		if len(wires) > 0 {
			p.send(msgItems, encodeItems(kind, wires))
		}
	case msgItems:
		r := codec.NewReader(b)
		kind := r.U8()
		n := r.Count(maxItemsPerMsg)
		var fresh [][]byte
		for i := 0; i < n && r.Err == nil; i++ {
			x := r.Bytes(maxItemWire)
			if r.Err != nil {
				break
			}
			h.cancelItemEmbargo(kind, x)
			isNew, err := h.addItem(p, kind, x)
			if err != nil {
				h.misbehave(p, err)
				return nil
			}
			if isNew {
				fresh = append(fresh, x)
			}
		}
		if err := r.Done(); err != nil {
			return err
		}
		if len(fresh) > 0 {
			h.broadcast(msgItems, encodeItems(kind, fresh), p)
		}
	case msgStemItem:
		r := codec.NewReader(b)
		kind := r.U8()
		x := r.Bytes(maxItemWire)
		if err := r.Done(); err != nil {
			return err
		}
		isNew, err := h.addItem(p, kind, x)
		if err != nil {
			h.misbehave(p, err)
			return nil
		}
		if !isNew {
			return nil
		}
		next := h.stemPeer()
		if next == nil || next == p || mrand.IntN(100) < fluffChance {
			h.broadcast(msgItems, encodeItems(kind, [][]byte{x}), p)
			return nil
		}
		h.armItemEmbargo(kind, x)
		next.send(msgStemItem, b)
	}
	return nil
}
