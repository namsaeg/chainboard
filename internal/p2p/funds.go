package p2p

import (
	"github.com/namsaeg/chainboard/internal/codec"
)

// Funding requests are gossiped before a pledge carries them on chain, so
// every node can list what is asking for backing. Each carries a proof of
// work (checked by the node), and nodes keep a bounded pool of them.

// FundGossip is the node's pool of gossiped funding requests. A request's
// wire form is its encoding and its proof-of-work nonce.
type FundGossip interface {
	FundIDs() []Hash
	FundWire(id Hash) ([]byte, bool)
	// FundAdd checks and keeps a request; isNew is true when it was not
	// held (with its work) before, so it is relayed on.
	FundAdd(wire []byte) (id Hash, isNew bool, err error)
}

const (
	maxFundIDs     = 4096
	maxFundsPerMsg = 256
	maxFundWire    = 2048
)

func encodeIDs(ids []Hash) []byte {
	var w codec.Writer
	w.Var(uint64(len(ids)))
	for _, id := range ids {
		w.Fixed(id[:])
	}
	return w.B
}

func decodeIDs(b []byte) ([]Hash, error) {
	r := codec.NewReader(b)
	n := r.Count(maxFundIDs)
	ids := make([]Hash, 0, n)
	for i := 0; i < n && r.Err == nil; i++ {
		var id Hash
		r.Fixed(id[:])
		ids = append(ids, id)
	}
	return ids, r.Done()
}

func encodeFunds(wires [][]byte) []byte {
	var w codec.Writer
	w.Var(uint64(len(wires)))
	for _, x := range wires {
		w.Bytes(x)
	}
	return w.B
}

func (h *Host) sendFundInv(p *peer) {
	if h.cfg.Funds == nil {
		return
	}
	if ids := h.cfg.Funds.FundIDs(); len(ids) > 0 {
		p.send(msgFundInv, encodeIDs(ids[:min(len(ids), maxFundIDs)]))
	}
}

// AnnounceFund gossips a funding request (with its work) to every peer.
func (h *Host) AnnounceFund(wire []byte) {
	h.broadcast(msgFunds, encodeFunds([][]byte{wire}), nil)
}

func (h *Host) handleFunds(p *peer, typ uint8, b []byte) error {
	g := h.cfg.Funds
	if g == nil {
		return nil
	}
	switch typ {
	case msgFundInv:
		ids, err := decodeIDs(b)
		if err != nil {
			return err
		}
		var want []Hash
		for _, id := range ids {
			if _, ok := g.FundWire(id); !ok {
				want = append(want, id)
			}
		}
		for len(want) > 0 {
			k := min(len(want), maxFundsPerMsg)
			p.send(msgGetFunds, encodeIDs(want[:k]))
			want = want[k:]
		}
	case msgGetFunds:
		ids, err := decodeIDs(b)
		if err != nil {
			return err
		}
		var wires [][]byte
		for _, id := range ids[:min(len(ids), maxFundsPerMsg)] {
			if x, ok := g.FundWire(id); ok {
				wires = append(wires, x)
			}
		}
		if len(wires) > 0 {
			p.send(msgFunds, encodeFunds(wires))
		}
	case msgFunds:
		r := codec.NewReader(b)
		n := r.Count(maxFundsPerMsg)
		var fresh [][]byte
		for i := 0; i < n && r.Err == nil; i++ {
			x := r.Bytes(maxFundWire)
			if r.Err != nil {
				break
			}
			if _, isNew, err := g.FundAdd(x); err == nil && isNew {
				fresh = append(fresh, x)
			}
		}
		if err := r.Done(); err != nil {
			return err
		}
		if len(fresh) > 0 {
			h.broadcast(msgFunds, encodeFunds(fresh), p)
		}
	}
	return nil
}
