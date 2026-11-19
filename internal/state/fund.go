package state

import (
	"errors"
	"fmt"
	"slices"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/pmap"
	"github.com/namsaeg/chainboard/internal/types"
)

// Crowdfunding (see types.FundRequest). A campaign's escrow can only pay
// for storing its data:
//
//	open      pledges come in until the deadline
//	uploaded  someone holding the data sent the funded upload before the
//	          deadline: the base fee is paid, the storage fee for the longest
//	          tier the escrow affords is held, and the proof window starts
//	done      a proof of FundSamples sub-chunks, drawn from the hash of the
//	          block after the upload, came within the window: the data is
//	          placed, the held fee goes to storage, and what is left returns
//	          to the backers in proportion. From here nothing can be taken back.
//
// If the deadline passes without an upload, or the window without a proof,
// the escrow (minus fees spent) returns to the backers by itself. Backers
// cannot withdraw a pledge, so a creator who delivers is always paid.

// FundStatus is where a campaign stands.
type FundStatus uint8

const (
	FundOpen     FundStatus = 1
	FundUploaded FundStatus = 2
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
	// Set by the funded upload.
	Upload uint64 // its height
	Tier   uint16
	Bucket uint32
	Held   uint64 // storage fee set aside in Escrow
	Seed   Hash   // hash of the block after the upload: picks the proof's samples
}

func (s *State) campaign(id Hash) (*Campaign, bool) {
	c, ok := s.Funds.Get(id)
	if !ok {
		return nil, false
	}
	cp := *c
	return &cp, true
}

// MinPledge is the smallest pledge: what its 32 state bytes cost as block
// bytes, so pledges cannot be used to fill the state for nothing.
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
	for _, p := range t.Pledges {
		if p.Amount < s.MinPledge() {
			return 0, fmt.Errorf("pledge below the minimum %d", s.MinPledge())
		}
		c, ok := s.Funds.Get(p.Request)
		switch {
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
	for _, p := range t.Pledges {
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

// FundCost is what a funded upload or proof pays from the escrow: its base
// fee (block bytes) and, for the upload, the storage fee it holds.
func (s *State) fundCost(t *types.Tx, c *Campaign, h uint64, ctx *BlockCtx) (base, held uint64, tier uint16, err error) {
	base = s.BytesFee(t)
	switch t.Kind {
	case types.TxFundUpload:
		if c.Status != FundOpen || h >= c.Deadline {
			return 0, 0, 0, errors.New("funding request is not open for its upload")
		}
		if s.Data.Has(c.DataRoot) {
			return 0, 0, 0, errors.New("that data is already stored")
		}
		if c.Size > s.FileCap() {
			return 0, 0, 0, fmt.Errorf("funded data larger than the file cap %d", s.FileCap())
		}
		var ok bool
		if tier, held, ok = s.FundTier(c, h, base); !ok {
			return 0, 0, 0, errors.New("the escrow does not yet afford the minimum tier")
		}
	case types.TxFundProof:
		if c.Status != FundUploaded || c.Seed.IsZero() || h > c.Upload+s.P.FundProofBlocks {
			return 0, 0, 0, errors.New("funding request is not waiting for its proof")
		}
		if base > c.Escrow-c.Held {
			return 0, 0, 0, errors.New("the escrow cannot pay for the proof")
		}
		if ctx.MediaUsed+c.Size > s.MediaCapPerBlock() {
			return 0, 0, 0, errors.New("media cap for this block reached")
		}
		if err := s.checkFundSamples(t, c); err != nil {
			return 0, 0, 0, err
		}
	}
	return base, held, tier, nil
}

// FundSampleAt is the chunk and sub-chunk a funding proof's sample j shows.
func FundSampleAt(seed, id Hash, size uint64, j int) (ci uint64, sub int) {
	x := cryptox.Tagged("cb/fund/sample", seed[:], id[:], []byte{byte(j)})
	ci = (uint64(x[0])<<56 | uint64(x[1])<<48 | uint64(x[2])<<40 | uint64(x[3])<<32 |
		uint64(x[4])<<24 | uint64(x[5])<<16 | uint64(x[6])<<8 | uint64(x[7])) % chunk.Count(size)
	return ci, int(x[8]) % chunk.SubCount(size, ci)
}

func (s *State) checkFundSamples(t *types.Tx, c *Campaign) error {
	if s.replay {
		return nil
	}
	for j, x := range t.Samples {
		ci, sub := FundSampleAt(c.Seed, t.Target, c.Size, j)
		if len(x.Sub) != chunk.SubLen(c.Size, ci, sub) || !chunk.VerifySub(c.DataRoot, c.Size, ci, sub, x.Sub, x.Path) {
			return fmt.Errorf("funding proof sample %d does not verify", j)
		}
	}
	return nil
}

func (s *State) applyFundUpload(id Hash, c *Campaign, base, held uint64, tier uint16, h uint64) {
	months, perm, _ := params.TierMonths(tier)
	c.Escrow -= base
	c.Held, c.Tier, c.Bucket = held, tier, s.BucketFor(h, months, perm)
	c.Status, c.Upload = FundUploaded, h
	s.Funds.Set(id, c)
	s.due(h+2, id)                     // the seed: hash of block h+1
	s.due(h+s.P.FundProofBlocks+1, id) // end of the proof window
}

func (s *State) applyFundProof(id Hash, c *Campaign, base uint64, ctx *BlockCtx) {
	c.Escrow -= base
	ctx.MediaUsed += c.Size
	s.place(c.DataRoot, c.Size, c.Media, c.Tier, c.Bucket)
	s.fund(c.Bucket, c.Held)
	c.Escrow -= c.Held
	c.Held = 0
	s.refund(c)
	s.Funds.Delete(id)
}

// refund returns a campaign's escrow to its backers in proportion to their
// pledges; the rounding remainder goes to the repair pool.
func (s *State) refund(c *Campaign) {
	left := c.Escrow
	for i := 0; i < c.Pledges.Len(); i++ {
		p := c.Pledges.At(i)
		share := mulDiv(p.Amount, c.Escrow, c.Raised)
		if share > 0 {
			s.Account(p.From).Balance += share
			left -= share
		}
	}
	s.RepairPool += left
	c.Escrow = 0
}

// fundDue runs the campaign deadlines that fall at height h. prev is the
// hash of block h-1.
func (s *State) fundDue(h uint64, prev Hash) {
	ids, ok := s.FundDue.Get(h)
	if !ok {
		return
	}
	s.FundDue.Delete(h)
	for _, id := range ids {
		c, ok := s.campaign(id)
		if !ok {
			continue
		}
		switch {
		case c.Status == FundOpen && h >= c.Deadline:
			s.refund(c)
			s.Funds.Delete(id)
		case c.Status == FundUploaded && h == c.Upload+2 && c.Seed.IsZero():
			c.Seed = prev
			s.Funds.Set(id, c)
		case c.Status == FundUploaded && h > c.Upload+s.P.FundProofBlocks:
			s.refund(c)
			s.Funds.Delete(id)
		}
	}
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
	pr := c.Pledges.Root(func(p PledgeRec) pmap.Sum {
		var pw codec.Writer
		pw.Fixed(p.From[:])
		pw.Var(p.Amount)
		return sumOf("pledge", &pw)
	})
	w.Fixed(pr[:])
	w.Var(c.Upload)
	w.U16(c.Tier)
	w.U32(c.Bucket)
	w.Var(c.Held)
	w.Fixed(c.Seed[:])
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
