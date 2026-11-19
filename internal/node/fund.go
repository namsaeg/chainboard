package node

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/media"
	"github.com/namsaeg/chainboard/internal/miner"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

// FundView is a funding request as the UI shows it.
type FundView struct {
	Status   string `json:"status"` // open, uploaded, stored, closed
	Raised   string `json:"raised"`
	Escrow   string `json:"escrow"`
	Backers  int    `json:"backers"`
	Deadline uint64 `json:"deadline"`
	Root     string `json:"root"`
	Size     uint64 `json:"size"`
	MinTier  uint16 `json:"min_tier"`
	// Tier the escrow would buy if uploaded now (0: not yet the minimum),
	// or the tier bought once uploaded.
	Tier uint16 `json:"tier"`
	// NeedForMin is what the minimum tier costs now, in coins.
	NeedForMin string `json:"need_for_min"`
	Mine       bool   `json:"mine,omitempty"`
}

func coins(u uint64) string { return fmt.Sprintf("%d.%08d", u/params.Coin, u%params.Coin) }

// uploadBase estimates the base fee of a funded upload (its block bytes).
func uploadBase(st *state.State) uint64 {
	probe := &types.Tx{Kind: types.TxFundUpload, From: make([]byte, 32), Sig: make([]byte, 64), Target: cryptox.Hash{1}, Nonce: 1 << 20, MaxFee: 1 << 40}
	return st.BytesFee(probe)
}

// fundView describes a funding request at the given state.
func (n *Node) fundView(st *state.State, id cryptox.Hash, root cryptox.Hash, size uint64, minTier uint16, creator cryptox.Address) *FundView {
	v := &FundView{Root: root.String(), Size: size, MinTier: minTier, Mine: creator == n.Wallet.Address()}
	if months, perm, ok := params.TierMonths(minTier); ok {
		v.NeedForMin = coins(state.StorageFee(st.P, st.Price, size, st.P.MediaReplicas, months, perm) + uploadBase(st))
	}
	c, ok := st.Funds.Get(id)
	switch {
	case ok:
		v.Raised, v.Escrow, v.Backers, v.Deadline = coins(c.Raised), coins(c.Escrow), c.Pledges.Len(), c.Deadline
		if c.Status == state.FundUploaded {
			v.Status, v.Tier = "uploaded", c.Tier
		} else {
			v.Status = "open"
			if t, _, ok := st.FundTier(c, st.Height+1, uploadBase(st)); ok {
				v.Tier = t
			}
		}
	case st.Data.Has(root):
		v.Status = "stored"
	default:
		v.Status = "closed"
	}
	return v
}

// fillFund completes the view of a listed funding request.
func (n *Node) fillFund(v *PostView, tip *state.State) {
	if v.Lane != "fund" || len(v.Attach) == 0 {
		return
	}
	root, err := cryptox.ParseHash(v.Attach[0])
	if err != nil {
		return
	}
	var size uint64
	var minTier uint16
	if c, ok := tip.Funds.Get(v.id); ok {
		size, minTier = c.Size, c.MinTier
	} else if d, ok := tip.Data.Get(root); ok {
		size = d.Size
	}
	author, _ := cryptox.ParseAddress(v.Author)
	v.Fund = n.fundView(tip, v.id, root, size, minTier, author)
	v.Attach = nil // shown by the UI only once stored
	if v.Fund.Status == "stored" {
		v.Attach = []string{root.String()}
	}
}

// ownFunds is fund.json: the requests this node's wallet signed.
type ownFunds struct {
	Codes []string `json:"codes"`
}

func (n *Node) loadFunds() {
	b, err := os.ReadFile(n.rlnPath("fund.json"))
	if err != nil {
		return
	}
	var f ownFunds
	if json.Unmarshal(b, &f) != nil {
		return
	}
	for _, code := range f.Codes {
		if q, err := decodeCode(code); err == nil {
			n.funds[q.ID()] = &fundTend{req: q, own: true}
		}
	}
}

func (n *Node) saveFundsLocked() {
	var f ownFunds
	for _, ft := range n.funds {
		if ft.own {
			f.Codes = append(f.Codes, hex.EncodeToString(ft.req.Encode()))
		}
	}
	b, _ := json.Marshal(f)
	if err := writeFileAtomic(n.rlnPath("fund.json"), b); err != nil {
		n.cfg.Logf("node: saving fund.json: %v", err)
	}
}

// fundTend is a request this node knows: its own (it uploads and proves
// once funded) or one a user pasted (to carry with a pledge).
type fundTend struct {
	req     *types.FundRequest
	own     bool
	sentAt  uint64 // height when the last upload or proof was sent
	sentFor state.FundStatus
}

func decodeCode(code string) (*types.FundRequest, error) {
	b, err := hex.DecodeString(code)
	if err != nil {
		return nil, errors.New("not a funding request code")
	}
	q, err := types.DecodeFundRequest(b)
	if err != nil {
		return nil, errors.New("not a funding request code")
	}
	return q, q.Check()
}

// CreateFund keeps data here and signs a request for pledges to store it.
// The returned code is what the creator shares; it needs no coins. Files
// pass the same checks and re-encoding as uploads.
func (n *Node) CreateFund(raw []byte, board, note string, minTier uint16, days uint64) (*types.FundRequest, string, error) {
	data, class, err := media.Reencode(raw)
	if err != nil {
		return nil, "", err
	}
	tip := n.Chain.Tip()
	if days == 0 || days*n.P.BlocksPerDay > n.P.FundMaxBlocks {
		return nil, "", fmt.Errorf("the deadline must be 1 to %d days away", n.P.FundMaxBlocks/n.P.BlocksPerDay)
	}
	if uint64(len(data)) > tip.FileCap() {
		return nil, "", fmt.Errorf("file larger than the cap of %d bytes", tip.FileCap())
	}
	root, err := n.Store.PutData(data)
	if err != nil {
		return nil, "", err
	}
	q := &types.FundRequest{DataRoot: root, Size: uint64(len(data)), Media: class, MinTier: minTier,
		Deadline: tip.Height + days*n.P.BlocksPerDay, Board: board, Note: note}
	q.Sign(n.Wallet)
	if err := q.Check(); err != nil {
		return nil, "", err
	}
	n.fundMu.Lock()
	n.funds[q.ID()] = &fundTend{req: q, own: true}
	n.saveFundsLocked()
	n.fundMu.Unlock()
	return q, hex.EncodeToString(q.Encode()), nil
}

// FundCheck is what a backer sees before pledging.
type FundCheck struct {
	ID       string    `json:"id"`
	Board    string    `json:"board"`
	Note     string    `json:"note"`
	Creator  string    `json:"creator"`
	Media    uint8     `json:"media"`
	OnChain  bool      `json:"on_chain"`
	Fund     *FundView `json:"fund"`
	Asked    int       `json:"samples_asked"`
	Verified int       `json:"samples_verified"`
	// Preview is the first chunk once it verified (the start of the file).
	Preview []byte `json:"preview,omitempty"`
}

// CheckFund reads a request code and starts a spot check of its data:
// the first chunk (a preview) and three at random must arrive from the
// network and match the data root.
func (n *Node) CheckFund(code string) (*FundCheck, error) {
	q, err := decodeCode(code)
	if err != nil {
		return nil, err
	}
	n.fundMu.Lock()
	if _, ok := n.funds[q.ID()]; !ok {
		n.funds[q.ID()] = &fundTend{req: q}
	}
	n.fundMu.Unlock()
	if !n.Store.Complete(q.DataRoot) {
		count := chunk.Count(q.Size)
		idx := []uint64{0}
		for len(idx) < min(4, int(count)) {
			i := rand.Uint64N(count)
			if !contains(idx, i) {
				idx = append(idx, i)
			}
		}
		n.Host.Probe(q.DataRoot, q.Size, idx)
	}
	return n.FundStatus(q.ID())
}

func contains(xs []uint64, x uint64) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// FundStatus reports a known request and how far its spot check got.
func (n *Node) FundStatus(id cryptox.Hash) (*FundCheck, error) {
	n.fundMu.Lock()
	ft, ok := n.funds[id]
	n.fundMu.Unlock()
	if !ok {
		return nil, errors.New("unknown funding request: paste its code first")
	}
	q := ft.req
	tip := n.Chain.Tip()
	fc := &FundCheck{ID: id.String(), Board: q.Board, Note: q.Note, Creator: q.CreatorAddr().String(), Media: q.Media}
	_, fc.OnChain = tip.Funds.Get(id)
	fc.Fund = n.fundView(tip, id, q.DataRoot, q.Size, q.MinTier, q.CreatorAddr())
	if fc.Fund.Deadline == 0 {
		fc.Fund.Deadline = q.Deadline
	}
	if fc.Fund.Status == "closed" && q.Deadline > tip.Height {
		fc.Fund.Status = "new" // not carried on chain yet
	}
	count := chunk.Count(q.Size)
	fc.Asked = min(4, int(count))
	if n.Store.Complete(q.DataRoot) {
		fc.Verified = fc.Asked
		if c, err := n.Store.Chunk(q.DataRoot, 0); err == nil {
			fc.Preview = c
		}
		return fc, nil
	}
	got := n.Host.ProbeResult(q.DataRoot)
	fc.Verified = len(got)
	fc.Preview = got[0]
	return fc, nil
}

// Pledge backs a request with coins. A request not on chain yet is
// carried with the pledge.
func (n *Node) Pledge(id cryptox.Hash, amount uint64, confirm bool) error {
	tip := n.trialState()
	tx := &types.Tx{Kind: types.TxLikes, Pledges: []types.Pledge{{Request: id, Amount: amount}}}
	if !tip.Funds.Has(id) {
		n.fundMu.Lock()
		ft, ok := n.funds[id]
		n.fundMu.Unlock()
		if !ok {
			return errors.New("unknown funding request: paste its code first")
		}
		tx.Requests = []*types.FundRequest{ft.req}
	}
	return n.SendChecked(tx, confirm)
}

// fundLead is how long before the deadline an own request is uploaded on
// the minimum tier (earlier only once it affords permanent storage).
func (n *Node) fundLead() uint64 { return max(3, n.P.BlocksPerDay/4) }

// tendFunds uploads and proves this node's own requests: as soon as the
// escrow affords permanent storage, or near the deadline at the longest
// tier it affords; then the proof once its seed is known.
func (n *Node) tendFunds(st *state.State) {
	n.fundMu.Lock()
	var own []*fundTend
	for _, ft := range n.funds {
		if ft.own {
			own = append(own, ft)
		}
	}
	n.fundMu.Unlock()
	h := st.Height + 1
	for _, ft := range own {
		id := ft.req.ID()
		c, ok := st.Funds.Get(id)
		if !ok || !n.Store.Complete(c.DataRoot) {
			continue
		}
		if ft.sentFor == c.Status && h < ft.sentAt+5 {
			continue // sent; give it a few blocks
		}
		var tx *types.Tx
		switch c.Status {
		case state.FundOpen:
			tier, _, ok := st.FundTier(c, h, uploadBase(st)*2)
			if !ok || (tier != params.TierPermanent && h+n.fundLead() < c.Deadline) {
				continue
			}
			tx = &types.Tx{Kind: types.TxFundUpload, Target: id}
		case state.FundUploaded:
			if c.Seed.IsZero() {
				continue
			}
			samples, err := miner.FundSamples(n.Store, c, id)
			if err != nil {
				n.cfg.Logf("node: funding proof: %v", err)
				continue
			}
			tx = &types.Tx{Kind: types.TxFundProof, Target: id, Samples: samples}
		}
		if err := n.Send(tx); err != nil {
			n.cfg.Logf("node: funding %s: %v", tx.Kind, err)
			continue
		}
		ft.sentAt, ft.sentFor = h, c.Status
	}
}
