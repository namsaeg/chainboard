package state

import (
	"errors"
	"fmt"
	"slices"

	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/pmap"
	"github.com/namsaeg/chainboard/internal/types"
)

// Crowdfunding (see types.FundRequest). A campaign's escrow can only pay
// for storing its data:
//
//	open    pledges come in until the deadline
//	stored  the creator sent the fund store before the deadline: its data
//	        is placed at the longest tier the escrow affords (not shorter
//	        than the request asks), the storage fee goes to the bucket, and
//	        what is left returns to the backers in proportion
//
// A fund store is valid only with all of its data at hand: a node takes it
// into its pool, and a recent block holding it, only once every chunk is
// here and matches the data root (see SetDataCheck). So a store either
// happens with the whole data or does not happen at all, and a failed
// attempt costs the backers nothing. If the deadline passes without a
// store, the escrow returns to the backers in full by itself. Backers
// cannot withdraw a pledge, so a creator who delivers is always paid.

// FundStatus is where a campaign stands.
type FundStatus uint8

const (
	FundOpen      FundStatus = 1
	FundRefunding FundStatus = 3 // being returned to the backers (see refund.go)
)

// PledgeRec is one pledge, kept for refunds.
type PledgeRec struct {
	From   Address
	Amount uint64
}

// Campaign is a funding request on chain.
type Campaign struct {
	Creator  Address
	DataRoot Hash
	Size     uint64
	Media    uint8
	MinTier  uint16
	Deadline uint64
	Status   FundStatus
	Escrow   uint64 // coins held: pledges minus the fees paid from them
	Raised   uint64 // pledged in total (the refund shares)
	Pledges  pmap.Vec[PledgeRec]
	Tier     uint16 // the tier its data was stored at (0 until stored)
	// Refund in progress.
	RefundPot uint64
	RefundAt  int
}

func (s *State) campaign(id Hash) (*Campaign, bool) {
	c, ok := s.Funds.Get(id)
	if !ok {
		return nil, false
	}
	cp := *c
	return &cp, true
}

// MinPledge is the smallest like: a tip, a pledge or a bounty boost. It is
// what 32 state bytes cost as block bytes, so pledge records cannot fill
// the state for nothing (new accounts pay NewAccountFee on top).
func (s *State) MinPledge() uint64 { return 32 * s.PermByteFee() }

func (s *State) checkRequest(r *types.FundRequest, h uint64) error {
	if r.Deadline <= h {
		return errors.New("funding request's deadline has passed")
	}
	if r.Deadline > h+s.P.FundMaxBlocks {
		return fmt.Errorf("funding deadline must be within %d blocks", s.P.FundMaxBlocks)
	}
	if r.Size > s.FileCap() {
		return fmt.Errorf("funded data larger than the file cap %d", s.FileCap())
	}
	if s.Data.Has(r.DataRoot) {
		return errors.New("that data is already stored")
	}
	return nil
}

// checkPledges validates a batch's carried requests and pledges and returns
// what it moves from the sender.
// checkPledges checks a like batch's pledges and tips and returns the coins
// they move out of the sender's balance.
func (s *State) checkPledges(t *types.Tx, h uint64) (uint64, error) {
	carried := map[Hash]*types.FundRequest{}
	for _, r := range t.Requests {
		if !s.Funds.Has(r.ID()) {
			if err := s.checkRequest(r, h); err != nil {
				return 0, err
			}
		}
		carried[r.ID()] = r
	}
	var total uint64
	for _, p := range t.Tips {
		if p.Amount < s.MinPledge() {
			return 0, fmt.Errorf("tip below the minimum %d", s.MinPledge())
		}
		total += p.Amount
		if total < p.Amount {
			return 0, errors.New("tips overflow")
		}
	}
	for _, p := range t.Pledges {
		if p.Amount < s.MinPledge() {
			return 0, fmt.Errorf("pledge below the minimum %d", s.MinPledge())
		}
		c, ok := s.Funds.Get(p.Request)
		b, isBounty := s.Bounties.Get(p.Request)
		switch {
		case isBounty:
			if (b.Status != BountyOpen && b.Status != BountyAccepted) || h >= b.Deadline {
				return 0, errors.New("that bounty no longer takes likes")
			}
		case ok && (c.Status != FundOpen || h >= c.Deadline):
			return 0, errors.New("that funding request no longer takes pledges")
		case !ok && carried[p.Request] == nil:
			return 0, errors.New("pledge to an unknown funding request (carry the request)")
		}
		total += p.Amount
		if total < p.Amount {
			return 0, errors.New("pledges overflow")
		}
	}
	return total, nil
}

func (s *State) applyPledges(t *types.Tx, from Address, h uint64) {
	for _, r := range t.Requests {
		id := r.ID()
		if s.Funds.Has(id) {
			continue
		}
		s.Funds.Set(id, &Campaign{Creator: r.CreatorAddr(), DataRoot: r.DataRoot, Size: r.Size, Media: r.Media,
			MinTier: r.MinTier, Deadline: r.Deadline, Status: FundOpen})
		s.due(r.Deadline, id)
	}
	for _, p := range t.Tips {
		s.Account(p.To).Balance += p.Amount
	}
	for _, p := range t.Pledges {
		if b, ok := s.bounty(p.Request); ok {
			b.Escrow += p.Amount
			b.Raised += p.Amount
			b.Pledges.Append(PledgeRec{From: from, Amount: p.Amount})
			s.Bounties.Set(p.Request, b)
			continue
		}
		c, _ := s.campaign(p.Request)
		c.Escrow += p.Amount
		c.Raised += p.Amount
		c.Pledges.Append(PledgeRec{From: from, Amount: p.Amount})
		s.Funds.Set(p.Request, c)
	}
}

// due schedules a campaign to be looked at in the finish of block h.
func (s *State) due(h uint64, id Hash) {
	list, _ := s.FundDue.Get(h)
	s.FundDue.Set(h, append(slices.Clip(list), id))
}

// tierOrder lists retention tiers longest first.
func tierOrder() []uint16 {
	out := []uint16{params.TierPermanent}
	for y := uint16(100); y >= 1; y-- {
		out = append(out, y*12)
	}
	return append(out, 1)
}

func tierAtLeast(t, min uint16) bool {
	if min == params.TierPermanent {
		return t == params.TierPermanent
	}
	return t == params.TierPermanent || t >= min
}

// FundTier is the longest tier (not shorter than the request asks for) that
// a campaign's escrow affords at height h after paying base; ok is false if
// even the minimum is out of reach.
func (s *State) FundTier(c *Campaign, h, base uint64) (tier uint16, fee uint64, ok bool) {
	if c.Escrow < base {
		return 0, 0, false
	}
	budget := c.Escrow - base
	for _, t := range tierOrder() {
		if !tierAtLeast(t, c.MinTier) {
			break
		}
		months, perm, _ := params.TierMonths(t)
		if f := StorageFee(s.P, s.Price, c.Size, s.P.MediaReplicas, months, perm); f <= budget {
			return t, f, true
		}
	}
	return 0, 0, false
}

// checkFundStore checks a fund store against its campaign and returns its
// base fee (paid from the escrow), the tier the escrow affords and that
// tier's storage fee.
func (s *State) checkFundStore(t *types.Tx, from Address, h uint64, ctx *BlockCtx) (c *Campaign, base, fee uint64, tier uint16, err error) {
	c, ok := s.campaign(t.Target)
	switch {
	case !ok:
		return nil, 0, 0, 0, errors.New("unknown funding request")
	case from != c.Creator:
		// Only the creator chooses when to store (and so the tier), so no
		// one can force an early, short-tier store.
		return nil, 0, 0, 0, errors.New("only the request's creator stores its data")
	case c.Status != FundOpen || h >= c.Deadline:
		return nil, 0, 0, 0, errors.New("funding request is not open for its store")
	case t.DataRoot != c.DataRoot || t.Size != c.Size:
		return nil, 0, 0, 0, errors.New("fund store names other data than its request")
	case s.Data.Has(c.DataRoot):
		return nil, 0, 0, 0, errors.New("that data is already stored")
	case c.Size > s.FileCap():
		return nil, 0, 0, 0, fmt.Errorf("funded data larger than the file cap %d", s.FileCap())
	case ctx.MediaUsed+c.Size > s.MediaCapPerBlock():
		return nil, 0, 0, 0, errors.New("media cap for this block reached")
	}
	base = s.BytesFee(t)
	if tier, fee, ok = s.FundTier(c, h, base); !ok {
		return nil, 0, 0, 0, errors.New("the escrow does not yet afford the minimum tier")
	}
	if err := s.checkData(c.DataRoot, c.Size); err != nil {
		return nil, 0, 0, 0, err
	}
	return c, base, fee, tier, nil
}

// applyFundStore places the funded data and returns the rest of the escrow.
func (s *State) applyFundStore(id Hash, c *Campaign, base, fee uint64, tier uint16, h uint64, ctx *BlockCtx) {
	months, perm, _ := params.TierMonths(tier)
	bucket := s.BucketFor(h, months, perm)
	ctx.MediaUsed += c.Size
	s.place(c.DataRoot, c.Size, c.Media, tier, bucket)
	s.fund(bucket, fee)
	c.Escrow -= base + fee
	c.Tier = tier
	s.Funds.Set(id, c)
	s.startRefund(id) // what is left returns to the backers
}

// checkData asks the installed data check (see SetDataCheck) whether all of
// a data root's chunks are here. Replays (blocks read back from our own
// database) were checked when they came.
func (s *State) checkData(root Hash, size uint64) error {
	if s.dataCheck == nil || s.replay {
		return nil
	}
	return s.dataCheck(root, size)
}

// SetDataCheck installs (or with nil removes) the check that a fund store
// or bounty reveal comes with all of its data: it returns an error (the
// chain's MissingDataError) while chunks are missing.
func (s *State) SetDataCheck(f func(root Hash, size uint64) error) { s.dataCheck = f }

// dueAll runs the campaign and bounty deadlines and windows that fall at
// height h (the block whose header is hd), then the refund queue.
func (s *State) dueAll(h uint64, ctx *BlockCtx) {
	if ids, ok := s.FundDue.Get(h); ok {
		s.FundDue.Delete(h)
		for _, id := range ids {
			if s.Bounties.Has(id) {
				s.bountyDue(id, h)
				continue
			}
			if c, ok := s.campaign(id); ok && c.Status == FundOpen && h >= c.Deadline {
				s.startRefund(id)
			}
		}
	}
	s.runRefunds()
}

func pledgeSum(p PledgeRec) pmap.Sum {
	var pw codec.Writer
	pw.Fixed(p.From[:])
	pw.Var(p.Amount)
	return sumOf("pledge", &pw)
}

func campaignSum(id Hash, c *Campaign) pmap.Sum {
	var w codec.Writer
	w.Fixed(id[:])
	w.Fixed(c.Creator[:])
	w.Fixed(c.DataRoot[:])
	w.Var(c.Size)
	w.U8(c.Media)
	w.U16(c.MinTier)
	w.Var(c.Deadline)
	w.U8(uint8(c.Status))
	w.Var(c.Escrow)
	w.Var(c.Raised)
	pr := c.Pledges.Root(pledgeSum)
	w.Fixed(pr[:])
	w.U16(c.Tier)
	w.Var(c.RefundPot)
	w.Var(uint64(c.RefundAt))
	return sumOf("campaign", &w)
}

func dueSum(h uint64, ids []Hash) pmap.Sum {
	var w codec.Writer
	w.Var(h)
	for _, id := range ids {
		w.Fixed(id[:])
	}
	return sumOf("due", &w)
}
