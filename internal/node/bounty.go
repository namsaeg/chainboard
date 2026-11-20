package node

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/media"
	"github.com/namsaeg/chainboard/internal/p2p"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

// Bounties on the node side. Offers travel off chain with their work, in a
// bounded pool. A hunter's data stays private on its node (only the first
// quarter of its chunks is served, as a preview bound to the data root)
// until its offer is accepted; then the node shares it and sends the proof
// by itself. Results, like everything on chain, are public: participants
// who want privacy encrypt between themselves.

const (
	maxOffersPerBounty = 50
	maxOffersPerHunter = 3 // per bounty
	maxOffers          = 5000
	maxPreviewSize     = 1 << 20
)

type offerTend struct {
	o      *types.Offer
	pow    uint64
	bits   int
	own    bool
	closed bool   // own offer whose bounty ended without it: kept so its data stays private
	sentAt uint64 // height the reveal proof was last sent
}

func offerWire(o *types.Offer, nonce uint64) []byte {
	var w codec.Writer
	w.Bytes(o.Encode())
	w.U64(nonce)
	return w.B
}

func decodeOfferWire(b []byte) (*types.Offer, uint64, error) {
	r := codec.NewReader(b)
	raw := r.Bytes(2048)
	nonce := r.U64()
	if err := r.Done(); err != nil {
		return nil, 0, err
	}
	o, err := types.DecodeOffer(raw)
	if err != nil {
		return nil, 0, err
	}
	return o, nonce, o.Check()
}

// ownOffers is offers.json: this node's offers and their work.
type ownOffers struct {
	Offers []string `json:"offers"`           // hex wire forms
	Closed []string `json:"closed,omitempty"` // offer ids whose bounty ended without them
}

func (n *Node) loadOffers() {
	b, err := os.ReadFile(n.rlnPath("offers.json"))
	if err != nil {
		return
	}
	var f ownOffers
	if json.Unmarshal(b, &f) != nil {
		return
	}
	closed := map[string]bool{}
	for _, c := range f.Closed {
		closed[c] = true
	}
	for _, h := range f.Offers {
		raw, err := hex.DecodeString(h)
		if err != nil {
			continue
		}
		if o, nonce, err := decodeOfferWire(raw); err == nil {
			n.offers[o.ID()] = &offerTend{o: o, pow: nonce, bits: types.PoWBits(o.ID(), nonce), own: true, closed: closed[o.ID().String()]}
		}
	}
}

func (n *Node) saveOffersLocked() {
	var f ownOffers
	for id, ot := range n.offers {
		if ot.own {
			f.Offers = append(f.Offers, hex.EncodeToString(offerWire(ot.o, ot.pow)))
			if ot.closed {
				f.Closed = append(f.Closed, id.String())
			}
		}
	}
	b, _ := json.Marshal(f)
	if err := writeFileAtomic(n.rlnPath("offers.json"), b); err != nil {
		n.cfg.Logf("node: saving offers.json: %v", err)
	}
}

func (n *Node) offerIDs() []cryptox.Hash {
	n.offerMu.Lock()
	defer n.offerMu.Unlock()
	ids := make([]cryptox.Hash, 0, len(n.offers))
	for id, ot := range n.offers {
		if !ot.closed && ot.bits > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func (n *Node) offerWire(id cryptox.Hash) ([]byte, bool) {
	n.offerMu.Lock()
	defer n.offerMu.Unlock()
	ot, ok := n.offers[id]
	if !ok || ot.closed || ot.bits == 0 {
		return nil, false
	}
	return offerWire(ot.o, ot.pow), true
}

// openBounty checks that a bounty takes offers.
func openBounty(st *state.State, id cryptox.Hash) (*state.Bounty, error) {
	b, ok := st.Bounties.Get(id)
	if !ok || b.Status != state.BountyOpen || st.Height+1 >= b.Deadline {
		return nil, errors.New("that bounty takes no offers now")
	}
	return b, nil
}

// addOffer checks a gossiped offer and keeps it within the pool's bounds.
func (n *Node) addOffer(wire []byte) (cryptox.Hash, bool, error) {
	o, nonce, err := decodeOfferWire(wire)
	if err != nil {
		return cryptox.Hash{}, false, fmt.Errorf("%w: %v", p2p.ErrMisbehave, err)
	}
	id := o.ID()
	n.offerMu.Lock()
	_, held := n.offers[id]
	n.offerMu.Unlock()
	if held {
		return id, false, nil
	}
	bits := types.PoWBits(id, nonce)
	if bits < int(n.P.GossipPoWBits) {
		return id, false, fmt.Errorf("%w: offer with %d bits of work, %d needed", p2p.ErrMisbehave, bits, n.P.GossipPoWBits)
	}
	tip := n.Chain.Tip()
	b, err := openBounty(tip, o.Bounty)
	if err != nil {
		return id, false, err
	}
	for _, f := range b.Failed {
		if f == id {
			return id, false, errors.New("that offer failed before")
		}
	}
	if tip.Data.Has(o.DataRoot) || o.PreviewSize > maxPreviewSize {
		return id, false, errors.New("offer of stored data, or a preview too large")
	}
	n.offerMu.Lock()
	defer n.offerMu.Unlock()
	perBounty, perHunter := 0, 0
	var weakest cryptox.Hash
	weakBits := 1 << 30
	hunter := o.HunterAddr()
	for x, ot := range n.offers {
		if ot.o.Bounty == o.Bounty {
			perBounty++
			if ot.o.HunterAddr() == hunter {
				perHunter++
			}
		}
		if !ot.own && ot.bits < weakBits {
			weakest, weakBits = x, ot.bits
		}
	}
	switch {
	case perHunter >= maxOffersPerHunter:
		return id, false, fmt.Errorf("a hunter makes at most %d offers per bounty", maxOffersPerHunter)
	case perBounty >= maxOffersPerBounty:
		return id, false, fmt.Errorf("a bounty keeps at most %d offers", maxOffersPerBounty)
	case len(n.offers) >= maxOffers:
		if bits <= weakBits {
			return id, false, errors.New("offer pool full")
		}
		delete(n.offers, weakest)
	}
	n.offers[id] = &offerTend{o: o, pow: nonce, bits: bits}
	return id, true, nil
}

// serves says whether a chunk may be given to peers: a pending offer's data
// only in its first quarter (the preview bound to its root).
func (n *Node) serves(root cryptox.Hash, idx uint64) bool {
	n.offerMu.Lock()
	defer n.offerMu.Unlock()
	for _, ot := range n.offers {
		if ot.own && ot.o.DataRoot == root && !n.shared[root] {
			return idx < boundPreviewChunks(ot.o.Size)
		}
	}
	return true
}

// boundPreviewChunks is how many leading chunks of an offer's data are
// shown before acceptance: a quarter, rounded down.
func boundPreviewChunks(size uint64) uint64 { return chunk.Count(size) / 4 }

// CreateOffer keeps data (and an optional free preview) here and gossips a
// signed offer for a bounty. The data is shared only once the offer is
// accepted.
func (n *Node) CreateOffer(bounty cryptox.Hash, raw, preview []byte, note string) (*types.Offer, error) {
	tip := n.Chain.Tip()
	b, err := openBounty(tip, bounty)
	if err != nil {
		return nil, err
	}
	if b.Requester == n.Wallet.Address() {
		return nil, errors.New("you cannot answer your own bounty")
	}
	data, class, err := media.Reencode(raw)
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) > tip.FileCap() {
		return nil, fmt.Errorf("file larger than the cap of %d bytes", tip.FileCap())
	}
	if root, _ := chunk.Root(data); tip.Data.Has(root) {
		return nil, errors.New("that file is already stored on the network: point the requester to it instead")
	}
	o := &types.Offer{Bounty: bounty, Size: uint64(len(data)), Media: class, Note: note}
	if len(preview) > 0 {
		pv, _, err := media.Reencode(preview)
		if err != nil {
			return nil, fmt.Errorf("preview: %w", err)
		}
		if len(pv) > maxPreviewSize {
			return nil, fmt.Errorf("preview larger than %d bytes", maxPreviewSize)
		}
		if o.PreviewRoot, err = n.Store.PutData(pv); err != nil {
			return nil, err
		}
		o.PreviewSize = uint64(len(pv))
	}
	// Private before stored: the data must never be served in full early.
	n.offerMu.Lock()
	o.DataRoot, _ = chunk.Root(data)
	o.Sign(n.Wallet)
	n.offers[o.ID()] = &offerTend{o: o, own: true}
	n.offerMu.Unlock()
	if _, err := n.Store.PutData(data); err != nil {
		return nil, err
	}
	if err := o.Check(); err != nil {
		return nil, err
	}
	n.solveOffer(o)
	return o, nil
}

// solveOffer finds an own offer's work in the background, then keeps and
// gossips it (through the stem).
func (n *Node) solveOffer(o *types.Offer) {
	if n.ctx == nil {
		return
	}
	id := o.ID()
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		nonce, ok := types.SolvePoW(id, int(n.P.GossipPoWBits), n.ctx.Done())
		if !ok {
			return
		}
		n.offerMu.Lock()
		ot, have := n.offers[id]
		if have {
			ot.pow, ot.bits = nonce, types.PoWBits(id, nonce)
			n.saveOffersLocked()
		}
		n.offerMu.Unlock()
		if have {
			n.Host.SubmitItem(p2p.ItemOffer, offerWire(o, nonce))
		}
	}()
}

// CreateBounty locks coins for wanted data.
func (n *Node) CreateBounty(board, note string, refs []cryptox.Hash, amount uint64, days uint64, tier uint16, confirm bool) (cryptox.Hash, error) {
	if days == 0 || days*n.P.BlocksPerDay > n.P.BountyMaxBlocks {
		return cryptox.Hash{}, fmt.Errorf("the deadline must be 1 to %d days away", n.P.BountyMaxBlocks/n.P.BlocksPerDay)
	}
	t := &types.Tx{Kind: types.TxBounty, Board: board, Note: note, Attach: refs, Amount: amount,
		Deadline: n.Chain.Height() + days*n.P.BlocksPerDay, Tier: tier}
	if err := n.SendChecked(t, confirm); err != nil {
		return cryptox.Hash{}, err
	}
	return t.ID(), nil
}

// AcceptOffer accepts an offer for one of this node's bounties: the pot
// locks to it.
func (n *Node) AcceptOffer(offer cryptox.Hash, confirm bool) error {
	n.offerMu.Lock()
	ot, ok := n.offers[offer]
	n.offerMu.Unlock()
	if !ok {
		return errors.New("unknown offer")
	}
	t := &types.Tx{Kind: types.TxBountyAccept, Offer: ot.o}
	if !confirm {
		// Accepting cannot be undone: once the data is revealed the pot is
		// the hunter's, whatever the rest of the data turns out to be.
		ws, fee := n.Warnings(t)
		ws = append([]Warning{{Code: "accept", Message: "수락하면 되돌릴 수 없어요. 제안자가 데이터를 공개하면 현상금은 바로 제안자에게 가요. " +
			"앞부분 미리보기가 찾던 자료인지 꼭 확인하세요(앞부분은 진짜 데이터의 일부로 확인된 것이에요)."}}, ws...)
		return &WarningError{Warnings: ws, Fee: fee}
	}
	return n.Send(t)
}

// tendBounties shares the data of accepted own offers and sends their
// reveal (valid only once peers can have all of the data, which this node
// now serves); it forgets offers whose bounty moved on.
func (n *Node) tendBounties(st *state.State) {
	h := st.Height + 1
	n.offerMu.Lock()
	var own []*offerTend
	for id, ot := range n.offers {
		b, ok := st.Bounties.Get(ot.o.Bounty)
		switch {
		case ot.own && ok && b.Status != state.BountyOpen && b.Offer == id:
			own = append(own, ot)
			n.shared[ot.o.DataRoot] = true
		case ok && b.Status == state.BountyOpen && h < b.Deadline:
		case ot.own && ok && b.Status != state.BountyRefunding:
			// accepted for someone else: wait for the outcome
		case ot.own && n.shared[ot.o.DataRoot]:
			delete(n.offers, id) // revealed: public anyway
		case ot.own:
			ot.closed = true // ended without it: its data stays private
		default:
			delete(n.offers, id)
		}
	}
	n.saveOffersLocked()
	n.offerMu.Unlock()
	for _, ot := range own {
		b, _ := st.Bounties.Get(ot.o.Bounty)
		if b.Status != state.BountyAccepted || (ot.sentAt != 0 && h < ot.sentAt+5) {
			continue
		}
		if pb, ok := n.trialState().Bounties.Get(ot.o.Bounty); !ok || pb.Status != state.BountyAccepted {
			continue // the reveal is already in the pool
		}
		if err := n.Send(&types.Tx{Kind: types.TxBountyReveal, Target: ot.o.Bounty, DataRoot: b.DataRoot, Size: b.Size}); err != nil {
			n.cfg.Logf("node: bounty reveal: %v", err)
			continue
		}
		ot.sentAt = h
	}
}

// BountyItem is a bounty as the UI lists it.
type BountyItem struct {
	ID        string   `json:"id"`
	Board     string   `json:"board"`
	Note      string   `json:"note"`
	Refs      []string `json:"refs,omitempty"`
	Requester string   `json:"requester"`
	Mine      bool     `json:"mine,omitempty"`
	Status    string   `json:"status"` // open, accepted, refunding
	Pot       string   `json:"pot"`
	Backers   int      `json:"backers"`
	Deadline  uint64   `json:"deadline"`
	Left      uint64   `json:"blocks_left"`
	Tier      uint16   `json:"tier"`
	Offers    int      `json:"offers"`
	Extended  uint64   `json:"extended,omitempty"` // blocks given back for failed reveals
	Failed    int      `json:"failed,omitempty"`   // accepted offers that never revealed
	// The accepted offer, and the window of its reveal.
	Offer        string `json:"offer,omitempty"`
	Hunter       string `json:"hunter,omitempty"`
	Root         string `json:"root,omitempty"`
	Size         uint64 `json:"size,omitempty"`
	WindowLeft   uint64 `json:"window_left,omitempty"`
	MyOffer      bool   `json:"my_offer,omitempty"` // the accepted offer is this node's
	StorageToday string `json:"storage_per_mb"`     // what storing 1 MB of result at its tier costs now
}

var bountyStatusNames = map[state.BountyStatus]string{state.BountyOpen: "open", state.BountyAccepted: "accepted",
	state.BountyRefunding: "refunding"}

// BountyList lists bounties on chain: open first, nearest deadline first.
func (n *Node) BountyList(limit int) []BountyItem {
	tip := n.Chain.Tip()
	h := tip.Height + 1
	counts := map[cryptox.Hash]int{}
	mine := map[cryptox.Hash]bool{}
	n.offerMu.Lock()
	for id, ot := range n.offers {
		if !ot.closed {
			counts[ot.o.Bounty]++
		}
		if ot.own {
			mine[id] = true
		}
	}
	n.offerMu.Unlock()
	out := []BountyItem{}
	tip.Bounties.Range(func(id cryptox.Hash, b *state.Bounty) bool {
		it := BountyItem{ID: id.String(), Requester: b.Requester.String(), Mine: b.Requester == n.Wallet.Address(),
			Status: bountyStatusNames[b.Status], Pot: coins(b.Escrow), Backers: b.Pledges.Len(), Deadline: b.Deadline,
			Tier: b.Tier, Offers: counts[id], Extended: b.Extended, Failed: len(b.Failed)}
		months, perm, _ := params.TierMonths(b.Tier)
		it.StorageToday = coins(state.StorageFee(tip.P, tip.Price, 1_000_000, tip.P.MediaReplicas, months, perm))
		if b.Deadline > h {
			it.Left = b.Deadline - h
		}
		if v, ok := n.Index.post(id); ok {
			it.Board, it.Note, it.Refs = v.Board, v.Text, v.Attach
		}
		if b.Status == state.BountyAccepted {
			it.Offer, it.Hunter, it.Root, it.Size, it.MyOffer = b.Offer.String(), b.Hunter.String(), b.DataRoot.String(), b.Size, mine[b.Offer]
			if end := b.Accepted + tip.P.BountyRevealBlocks; end >= h {
				it.WindowLeft = end - h + 1
			}
		}
		out = append(out, it)
		return true
	})
	rank := map[string]int{"open": 0, "accepted": 1, "refunding": 2}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Status] != rank[out[j].Status] {
			return rank[out[i].Status] < rank[out[j].Status]
		}
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

// OfferView is an offer as the UI shows it.
type OfferView struct {
	ID          string `json:"id"`
	Bounty      string `json:"bounty"`
	Hunter      string `json:"hunter"`
	Size        uint64 `json:"size"`
	Media       uint8  `json:"media"`
	Note        string `json:"note"`
	PreviewSize uint64 `json:"preview_size,omitempty"`
	Bound       uint64 `json:"bound_preview_bytes"` // leading bytes shown, proven against the data root
	Mine        bool   `json:"mine,omitempty"`
	Listed      bool   `json:"listed"`           // has its work (others can see it)
	Closed      bool   `json:"closed,omitempty"` // its bounty went to another offer or ended
}

func (ot *offerTend) view(id cryptox.Hash) OfferView {
	o := ot.o
	return OfferView{ID: id.String(), Bounty: o.Bounty.String(), Hunter: o.HunterAddr().String(), Size: o.Size, Media: o.Media,
		Note: o.Note, PreviewSize: o.PreviewSize, Bound: min(o.Size, boundPreviewChunks(o.Size)*params.ChunkSize),
		Mine: ot.own, Listed: ot.bits > 0, Closed: ot.closed}
}

// BountyOffers lists the offers this node knows for a bounty.
func (n *Node) BountyOffers(bounty cryptox.Hash) []OfferView {
	n.offerMu.Lock()
	defer n.offerMu.Unlock()
	out := []OfferView{}
	for id, ot := range n.offers {
		if ot.o.Bounty != bounty || ot.closed {
			continue
		}
		out = append(out, ot.view(id))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// OfferPreview is what has arrived of an offer's previews.
type OfferPreview struct {
	Free      []byte `json:"free,omitempty"`  // the hunter's free preview, whole
	Bound     []byte `json:"bound,omitempty"` // the data's leading chunks that arrived and verified
	BoundWant int    `json:"bound_chunks"`
	BoundGot  int    `json:"bound_got"`
}

// PreviewOffer fetches (once started, it keeps asking) an offer's previews
// from the network: the free one, and the data's leading chunks, each
// checked against the data root.
func (n *Node) PreviewOffer(offer cryptox.Hash) (*OfferPreview, error) {
	n.offerMu.Lock()
	ot, ok := n.offers[offer]
	n.offerMu.Unlock()
	if !ok {
		return nil, errors.New("unknown offer")
	}
	o := ot.o
	pv := &OfferPreview{BoundWant: int(boundPreviewChunks(o.Size))}
	fetch := func(root cryptox.Hash, size uint64, count uint64) map[uint64][]byte {
		if count == 0 {
			return nil
		}
		if !n.Host.Probing(root) {
			idx := make([]uint64, count)
			for i := range idx {
				idx[i] = uint64(i)
			}
			n.Host.Probe(root, size, idx)
		}
		return n.Host.ProbeResult(root)
	}
	if o.PreviewSize > 0 {
		if data, err := n.Store.Data(o.PreviewRoot); err == nil {
			pv.Free = data
		} else if got := fetch(o.PreviewRoot, o.PreviewSize, chunk.Count(o.PreviewSize)); len(got) == int(chunk.Count(o.PreviewSize)) {
			for i := uint64(0); i < chunk.Count(o.PreviewSize); i++ {
				pv.Free = append(pv.Free, got[i]...)
			}
		}
	}
	got := fetch(o.DataRoot, o.Size, uint64(pv.BoundWant))
	if ot.own {
		got = map[uint64][]byte{}
		for i := 0; i < pv.BoundWant; i++ {
			if c, err := n.Store.Chunk(o.DataRoot, uint64(i)); err == nil {
				got[uint64(i)] = c
			}
		}
	}
	for i := 0; i < pv.BoundWant; i++ {
		c, ok := got[uint64(i)]
		if !ok {
			break
		}
		pv.Bound = append(pv.Bound, c...)
		pv.BoundGot++
	}
	return pv, nil
}

// solveOwnOffers starts the work of own offers that do not have it yet.
func (n *Node) solveOwnOffers() {
	n.offerMu.Lock()
	var todo []*types.Offer
	for _, ot := range n.offers {
		if ot.own && ot.bits == 0 {
			todo = append(todo, ot.o)
		}
	}
	n.offerMu.Unlock()
	for _, o := range todo {
		n.solveOffer(o)
	}
}
