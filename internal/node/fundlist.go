package node

import (
	"errors"
	"fmt"
	"sort"

	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/p2p"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

// The list of crowdfunding in progress: requests on chain that still take
// pledges, and requests gossiped by peers before their first pledge.
// Gossiped ones carry a proof of work (params.GossipPoWBits) and the pool is
// bounded: maxGossipFunds in all, maxCreatorFunds per creator key; when it
// is full a new request must carry more work than the weakest one held,
// which it then replaces.
const (
	maxGossipFunds  = 1000
	maxCreatorFunds = 3
)

func fundWire(q *types.FundRequest, nonce uint64) []byte {
	var w codec.Writer
	w.Bytes(q.Encode())
	w.U64(nonce)
	return w.B
}

func decodeFundWire(b []byte) (*types.FundRequest, uint64, error) {
	r := codec.NewReader(b)
	raw := r.Bytes(2048)
	nonce := r.U64()
	if err := r.Done(); err != nil {
		return nil, 0, err
	}
	q, err := types.DecodeFundRequest(raw)
	if err != nil {
		return nil, 0, err
	}
	return q, nonce, q.Check()
}

// ItemIDs lists the items of a kind this node gossips.
func (n *Node) ItemIDs(kind uint8) []cryptox.Hash {
	if kind == p2p.ItemOffer {
		return n.offerIDs()
	}
	return n.fundIDs()
}

// ItemWire is a gossiped item's wire form.
func (n *Node) ItemWire(kind uint8, id cryptox.Hash) ([]byte, bool) {
	if kind == p2p.ItemOffer {
		return n.offerWire(id)
	}
	return n.fundWireOf(id)
}

// AddItem checks a gossiped item and keeps it within its pool's bounds.
// Bad work or a bad signature is the sender's misbehaviour.
func (n *Node) AddItem(kind uint8, wire []byte) (cryptox.Hash, bool, error) {
	switch kind {
	case p2p.ItemFund:
		return n.addFund(wire)
	case p2p.ItemOffer:
		return n.addOffer(wire)
	}
	return cryptox.Hash{}, false, fmt.Errorf("%w: unknown item kind %d", p2p.ErrMisbehave, kind)
}

// fundIDs lists the requests this node gossips: with their work, not on
// chain yet, deadline ahead.
func (n *Node) fundIDs() []cryptox.Hash {
	tip := n.Chain.Tip()
	n.fundMu.Lock()
	defer n.fundMu.Unlock()
	var ids []cryptox.Hash
	for id, ft := range n.funds {
		if ft.hasPoW && ft.req.Deadline > tip.Height+1 && !tip.Funds.Has(id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// fundWireOf is a gossiped request's wire form.
func (n *Node) fundWireOf(id cryptox.Hash) ([]byte, bool) {
	n.fundMu.Lock()
	defer n.fundMu.Unlock()
	ft, ok := n.funds[id]
	if !ok || !ft.hasPoW {
		return nil, false
	}
	return fundWire(ft.req, ft.pow), true
}

// addFund checks a gossiped request and keeps it within the pool's bounds.
func (n *Node) addFund(wire []byte) (cryptox.Hash, bool, error) {
	q, nonce, err := decodeFundWire(wire)
	if err != nil {
		return cryptox.Hash{}, false, fmt.Errorf("%w: %v", p2p.ErrMisbehave, err)
	}
	id := q.ID()
	if n.fundHeld(id) {
		return id, false, nil // known with its work: no need to check it again
	}
	bits := types.PoWBits(id, nonce)
	if bits < int(n.P.GossipPoWBits) {
		return id, false, fmt.Errorf("%w: funding request with %d bits of work, %d needed", p2p.ErrMisbehave, bits, n.P.GossipPoWBits)
	}
	tip := n.Chain.Tip()
	h := tip.Height + 1
	if q.Deadline <= h || q.Deadline > h+n.P.FundMaxBlocks+n.P.BlocksPerDay/24 {
		return id, false, errors.New("funding request's deadline out of range")
	}
	if tip.Funds.Has(id) || tip.Data.Has(q.DataRoot) {
		return id, false, nil // on chain (listed from there) or already stored
	}
	n.fundMu.Lock()
	defer n.fundMu.Unlock()
	if ft, ok := n.funds[id]; ok {
		if ft.hasPoW {
			return id, false, nil
		}
		ft.pow, ft.hasPoW, ft.bits = nonce, true, bits // a pasted request, now with work
		return id, true, nil
	}
	creator := q.CreatorAddr()
	count, mine := 0, 0
	var weakest cryptox.Hash
	weakBits := 1 << 30
	for x, ft := range n.funds {
		if !ft.gossip || tip.Funds.Has(x) {
			continue
		}
		count++
		if ft.req.CreatorAddr() == creator {
			mine++
		}
		if ft.bits < weakBits {
			weakest, weakBits = x, ft.bits
		}
	}
	if mine >= maxCreatorFunds {
		return id, false, fmt.Errorf("a creator gossips at most %d requests", maxCreatorFunds)
	}
	if count >= maxGossipFunds {
		if bits <= weakBits {
			return id, false, errors.New("funding request pool full")
		}
		delete(n.funds, weakest)
	}
	n.funds[id] = &fundTend{req: q, gossip: true, hasPoW: true, pow: nonce, bits: bits}
	return id, true, nil
}

// fundHeld reports whether a request is held with its work.
func (n *Node) fundHeld(id cryptox.Hash) bool {
	n.fundMu.Lock()
	defer n.fundMu.Unlock()
	ft, ok := n.funds[id]
	return ok && ft.hasPoW
}

// pruneFunds forgets requests of others past their deadline, and gossiped
// ones the chain now carries or whose data is stored (they no longer take
// a slot of the pool).
func (n *Node) pruneFunds(st *state.State) {
	n.fundMu.Lock()
	defer n.fundMu.Unlock()
	for id, ft := range n.funds {
		switch {
		case ft.own:
		case ft.req.Deadline <= st.Height:
			delete(n.funds, id)
		case ft.gossip && (st.Funds.Has(id) || st.Data.Has(ft.req.DataRoot)):
			delete(n.funds, id)
		}
	}
}

// solveFund finds the proof of work of an own request in the background,
// then keeps and gossips it.
func (n *Node) solveFund(q *types.FundRequest) {
	if n.ctx == nil {
		return // not started: Start solves what is missing
	}
	id := q.ID()
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		nonce, ok := types.SolvePoW(id, int(n.P.GossipPoWBits), n.ctx.Done())
		if !ok {
			return
		}
		n.fundMu.Lock()
		ft, have := n.funds[id]
		if have {
			ft.pow, ft.hasPoW, ft.bits = nonce, true, types.PoWBits(id, nonce)
			n.saveFundsLocked()
		}
		n.fundMu.Unlock()
		if have {
			n.Host.SubmitItem(p2p.ItemFund, fundWire(q, nonce))
		}
	}()
}

// solveOwnFunds starts the work of own requests that do not have it yet.
func (n *Node) solveOwnFunds() {
	h := n.Chain.Height()
	n.fundMu.Lock()
	var todo []*types.FundRequest
	for _, ft := range n.funds {
		if ft.own && !ft.hasPoW && ft.req.Deadline > h {
			todo = append(todo, ft.req)
		}
	}
	n.fundMu.Unlock()
	for _, q := range todo {
		n.solveFund(q)
	}
}

// FundItem is one crowdfunding in progress.
type FundItem struct {
	ID      string    `json:"id"`
	Board   string    `json:"board"`
	Note    string    `json:"note"`
	Creator string    `json:"creator"`
	Media   uint8     `json:"media"`
	OnChain bool      `json:"on_chain"`
	Listed  bool      `json:"listed"` // others can see it (on chain, or gossiped with its work)
	Left    uint64    `json:"blocks_left"`
	Fund    *FundView `json:"fund"`
}

// FundList lists crowdfunding still taking pledges, nearest deadline first.
func (n *Node) FundList(limit int) []FundItem {
	tip := n.Chain.Tip()
	h := tip.Height + 1
	out := []FundItem{}
	tip.Funds.Range(func(id cryptox.Hash, c *state.Campaign) bool {
		if c.Status != state.FundOpen || c.Deadline <= h {
			return true
		}
		it := FundItem{ID: id.String(), Creator: c.Creator.String(), Media: c.Media, OnChain: true, Listed: true, Left: c.Deadline - h,
			Fund: n.fundView(tip, id, c.DataRoot, c.Size, c.MinTier, c.Creator)}
		if v, ok := n.Index.post(id); ok {
			it.Board, it.Note = v.Board, v.Text
		}
		out = append(out, it)
		return true
	})
	n.fundMu.Lock()
	var pool []*fundTend
	for id, ft := range n.funds {
		if ft.req.Deadline > h && !tip.Funds.Has(id) { // on chain: listed above while open
			pool = append(pool, ft)
		}
	}
	n.fundMu.Unlock()
	for _, ft := range pool {
		q := ft.req
		id := q.ID()
		v := n.fundView(tip, id, q.DataRoot, q.Size, q.MinTier, q.CreatorAddr())
		if v.Status == "stored" {
			continue
		}
		v.Status, v.Deadline = "new", q.Deadline
		out = append(out, FundItem{ID: id.String(), Board: q.Board, Note: q.Note, Creator: q.CreatorAddr().String(),
			Media: q.Media, Listed: ft.hasPoW, Left: q.Deadline - h, Fund: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Left != out[j].Left {
			return out[i].Left < out[j].Left
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
