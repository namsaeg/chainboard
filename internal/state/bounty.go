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

// Bounties (see types.Offer). A bounty's pot can only go to the hunter of
// an accepted offer whose data is proven, be burned by a dispute, or
// return to its backers:
//
//	open      likes add to the pot until the deadline; only the requester
//	          may accept an offer, and only before the deadline
//	accepted  the offer is carried on chain; the storage of its data (at
//	          the bounty's tier) is held from the pot; the vdf output of the
//	          next block picks FundSamples sub-chunks the reveal must show
//	          within BountyRevealBlocks, deadline or not
//	proven    the data is placed and its storage paid; after
//	          BountyDisputeBlocks the pot goes to the hunter, unless the
//	          requester disputed, which burns it
//
// A reveal that does not come in time fails the offer (it cannot be
// accepted again) and reopens the bounty, or refunds it past the deadline.
// Nothing the requester can do takes the pot back once an offer is
// accepted, and nothing can be withdrawn before the deadline.

// BountyStatus is where a bounty stands.
type BountyStatus uint8

const (
	BountyOpen      BountyStatus = 1
	BountyAccepted  BountyStatus = 2
	BountyProven    BountyStatus = 3
	BountyRefunding BountyStatus = 4
)

// MaxFailedOffers bounds the offers a bounty can accept and see fail.
const MaxFailedOffers = 32

// Bounty is a bounty on chain.
type Bounty struct {
	Requester Address
	Deadline  uint64
	Tier      uint16 // retention of the result
	Status    BountyStatus
	Escrow    uint64 // the pot (less fees paid from it)
	Raised    uint64 // contributed in total (the refund shares)
	Pledges   pmap.Vec[PledgeRec]
	// The accepted offer.
	Offer    Hash
	Hunter   Address
	DataRoot Hash
	Size     uint64
	Media    uint8
	Accepted uint64 // height of the acceptance
	Seed     Hash
	Bucket   uint32
	Held     uint64 // storage fee set aside in Escrow
	Proven   uint64 // height of the proof (the dispute window starts)
	Failed   []Hash // offers accepted whose reveal never came
	// Refund in progress.
	RefundPot uint64
	RefundAt  int
}

func (s *State) bounty(id Hash) (*Bounty, bool) {
	b, ok := s.Bounties.Get(id)
	if !ok {
		return nil, false
	}
	cp := *b
	cp.Failed = slices.Clip(cp.Failed)
	return &cp, true
}

func (s *State) checkNewBounty(t *types.Tx, h uint64) error {
	if t.Deadline <= h || t.Deadline > h+s.P.BountyMaxBlocks {
		return fmt.Errorf("a bounty's deadline must be within %d blocks", s.P.BountyMaxBlocks)
	}
	if t.Amount < s.MinPledge() {
		return fmt.Errorf("a bounty starts at %d at least", s.MinPledge())
	}
	for _, a := range t.Attach {
		if !s.Data.Has(a) {
			return errors.New("reference attachment not stored")
		}
	}
	return nil
}

func (s *State) applyNewBounty(t *types.Tx, from Address, h uint64) {
	var ps pmap.Vec[PledgeRec]
	ps.Append(PledgeRec{From: from, Amount: t.Amount})
	id := t.ID()
	s.Bounties.Set(id, &Bounty{Requester: from, Deadline: t.Deadline, Tier: t.Tier, Status: BountyOpen,
		Escrow: t.Amount, Raised: t.Amount, Pledges: ps})
	s.due(t.Deadline, id)
}

// resultFee is what storing an offer's data at the bounty's tier costs now.
func (s *State) resultFee(b *Bounty, size uint64) uint64 {
	months, perm, _ := params.TierMonths(b.Tier)
	return StorageFee(s.P, s.Price, size, s.P.MediaReplicas, months, perm)
}

func (s *State) checkAccept(t *types.Tx, from Address, h uint64) (*Bounty, uint64, error) {
	o := t.Offer
	b, ok := s.bounty(o.Bounty)
	switch {
	case !ok:
		return nil, 0, errors.New("unknown bounty")
	case from != b.Requester:
		return nil, 0, errors.New("only the requester accepts an offer")
	case b.Status != BountyOpen || h >= b.Deadline:
		return nil, 0, errors.New("the bounty takes no acceptance now")
	case slices.Contains(b.Failed, o.ID()):
		return nil, 0, errors.New("that offer was accepted before and never revealed")
	case len(b.Failed) >= MaxFailedOffers:
		return nil, 0, errors.New("too many failed offers on this bounty")
	case s.Data.Has(o.DataRoot):
		return nil, 0, errors.New("that data is already stored: it needs no bounty")
	case o.Size > s.FileCap():
		return nil, 0, fmt.Errorf("offered data larger than the file cap %d", s.FileCap())
	}
	held := s.resultFee(b, o.Size)
	if held >= b.Escrow {
		return nil, 0, errors.New("the pot does not cover storing that data at the bounty's tier")
	}
	return b, held, nil
}

func (s *State) applyAccept(t *types.Tx, b *Bounty, held uint64, h uint64) {
	o := t.Offer
	months, perm, _ := params.TierMonths(b.Tier)
	b.Status, b.Accepted = BountyAccepted, h
	b.Offer, b.Hunter, b.DataRoot, b.Size, b.Media = o.ID(), o.HunterAddr(), o.DataRoot, o.Size, o.Media
	b.Held, b.Bucket, b.Seed = held, s.BucketFor(h, months, perm), Hash{}
	s.Bounties.Set(o.Bounty, b)
	s.due(h+1, o.Bounty)                        // the seed: vdf output of block h+1
	s.due(h+s.P.BountyRevealBlocks+1, o.Bounty) // end of the reveal window
}

func (s *State) checkBountyProof(t *types.Tx, h uint64, ctx *BlockCtx) (*Bounty, uint64, error) {
	b, ok := s.bounty(t.Target)
	if !ok || b.Status != BountyAccepted || b.Seed.IsZero() || h > b.Accepted+s.P.BountyRevealBlocks {
		return nil, 0, errors.New("the bounty is not waiting for a reveal")
	}
	base := s.BytesFee(t)
	if base > b.Escrow-b.Held {
		return nil, 0, errors.New("the pot cannot pay for the proof")
	}
	if ctx.MediaUsed+b.Size > s.MediaCapPerBlock() {
		return nil, 0, errors.New("media cap for this block reached")
	}
	if !s.replay {
		if err := s.checkSamples(t, b.Seed, b.DataRoot, b.Size); err != nil {
			return nil, 0, err
		}
	}
	return b, base, nil
}

func (s *State) applyBountyProof(t *types.Tx, b *Bounty, base uint64, h uint64, ctx *BlockCtx) {
	b.Escrow -= base
	ctx.MediaUsed += b.Size
	s.place(b.DataRoot, b.Size, b.Media, b.Tier, b.Bucket)
	s.fund(b.Bucket, b.Held)
	b.Escrow -= b.Held
	b.Held = 0
	b.Status, b.Proven = BountyProven, h
	s.Bounties.Set(t.Target, b)
	s.due(h+s.P.BountyDisputeBlocks+1, t.Target)
}

func (s *State) checkDispute(t *types.Tx, from Address, h uint64) (*Bounty, error) {
	b, ok := s.bounty(t.Target)
	switch {
	case !ok || b.Status != BountyProven:
		return nil, errors.New("no proven result to dispute")
	case from != b.Requester:
		return nil, errors.New("only the requester disputes")
	case h > b.Proven+s.P.BountyDisputeBlocks:
		return nil, errors.New("the dispute window has closed")
	}
	return b, nil
}

// applyDispute burns the pot: the requester gains nothing by disputing
// (the data is public and the coins do not come back), and a hunter whose
// data was not what its preview showed is not paid.
func (s *State) applyDispute(id Hash, b *Bounty, ctx *BlockCtx) {
	ctx.Burn += b.Escrow
	s.Bounties.Delete(id)
}

// bountyDue runs what falls due at height h for one bounty.
func (s *State) bountyDue(id Hash, h uint64, seed Hash, ctx *BlockCtx) {
	b, ok := s.bounty(id)
	if !ok {
		return
	}
	switch {
	case b.Status == BountyOpen && h >= b.Deadline:
		s.startRefund(id)
	case b.Status == BountyAccepted && h == b.Accepted+1 && b.Seed.IsZero():
		b.Seed = sampleSeed(seed, id)
		s.Bounties.Set(id, b)
	case b.Status == BountyAccepted && h > b.Accepted+s.P.BountyRevealBlocks:
		b.Failed = append(b.Failed, b.Offer)
		b.Status, b.Held, b.Offer, b.Hunter, b.DataRoot, b.Size, b.Media, b.Accepted, b.Seed, b.Bucket =
			BountyOpen, 0, Hash{}, Address{}, Hash{}, 0, 0, 0, Hash{}, 0
		s.Bounties.Set(id, b)
		if h >= b.Deadline {
			s.startRefund(id)
		}
	case b.Status == BountyProven && h > b.Proven+s.P.BountyDisputeBlocks:
		pay := b.Escrow
		if !s.Accounts.Has(b.Hunter) {
			fee := min(pay, s.NewAccountFee())
			ctx.Burn += fee
			pay -= fee
		}
		s.Account(b.Hunter).Balance += pay
		s.Bounties.Delete(id)
	}
}

func bountySum(id Hash, b *Bounty) pmap.Sum {
	var w codec.Writer
	w.Fixed(id[:])
	w.Fixed(b.Requester[:])
	w.Var(b.Deadline)
	w.U16(b.Tier)
	w.U8(uint8(b.Status))
	w.Var(b.Escrow)
	w.Var(b.Raised)
	pr := b.Pledges.Root(pledgeSum)
	w.Fixed(pr[:])
	w.Fixed(b.Offer[:])
	w.Fixed(b.Hunter[:])
	w.Fixed(b.DataRoot[:])
	w.Var(b.Size)
	w.U8(b.Media)
	w.Var(b.Accepted)
	w.Fixed(b.Seed[:])
	w.U32(b.Bucket)
	w.Var(b.Held)
	w.Var(b.Proven)
	w.Var(uint64(len(b.Failed)))
	for _, f := range b.Failed {
		w.Fixed(f[:])
	}
	w.Var(b.RefundPot)
	w.Var(uint64(b.RefundAt))
	return sumOf("bounty", &w)
}
