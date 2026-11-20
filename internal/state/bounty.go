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
// an accepted offer whose data is stored in full, or return to its
// backers:
//
//	open      likes add to the pot until the deadline; only the requester
//	          may accept an offer, and only before the deadline
//	accepted  the offer is carried on chain and the pot is locked to it;
//	          the storage of its data (at the bounty's tier) is held from the
//	          pot. Within BountyRevealBlocks, deadline or not, anyone may send
//	          the reveal: valid only with the whole data at hand (see
//	          SetDataCheck), it stores the data and pays the rest of the pot
//	          to the hunter at once.
//
// A reveal that does not come in time fails the offer (it cannot be
// accepted again), reopens the bounty and gives it back the blocks it was
// locked for (up to BountyExtendMaxBlocks in all), or refunds it if the
// deadline has passed even so. No step depends on anyone's judgement: once
// an offer is accepted nothing can take the pot from its hunter, and
// nothing can be withdrawn before the deadline.

// BountyStatus is where a bounty stands.
type BountyStatus uint8

const (
	BountyOpen      BountyStatus = 1
	BountyAccepted  BountyStatus = 2
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
	Bucket   uint32
	Held     uint64 // storage fee set aside in Escrow
	Failed   []Hash // offers accepted whose reveal never came
	Extended uint64 // blocks added to the deadline for failed reveals
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
	b.Held, b.Bucket = held, s.BucketFor(h, months, perm)
	s.Bounties.Set(o.Bounty, b)
	s.due(h+s.P.BountyRevealBlocks+1, o.Bounty) // end of the reveal window
}

// checkBountyReveal checks a reveal and returns its bounty, its base fee
// (paid from the pot) and the account fee owed if the hunter has no account
// yet.
func (s *State) checkBountyReveal(t *types.Tx, h uint64, ctx *BlockCtx) (*Bounty, uint64, uint64, error) {
	b, ok := s.bounty(t.Target)
	switch {
	case !ok || b.Status != BountyAccepted || h > b.Accepted+s.P.BountyRevealBlocks:
		return nil, 0, 0, errors.New("the bounty is not waiting for a reveal")
	case t.DataRoot != b.DataRoot || t.Size != b.Size:
		return nil, 0, 0, errors.New("the reveal names other data than the accepted offer")
	case ctx.MediaUsed+b.Size > s.MediaCapPerBlock():
		return nil, 0, 0, errors.New("media cap for this block reached")
	}
	base := s.BytesFee(t)
	var account uint64
	if !s.Accounts.Has(b.Hunter) {
		account = s.NewAccountFee()
	}
	if base+account > b.Escrow-b.Held {
		return nil, 0, 0, errors.New("the pot cannot pay for the reveal")
	}
	if err := s.checkData(b.DataRoot, b.Size); err != nil {
		return nil, 0, 0, err
	}
	return b, base, account, nil
}

// applyBountyReveal stores the data and pays the hunter. Data already
// stored by someone else (say, the requester, from the shared data) is
// moved to the bounty's bucket: the hunter is paid all the same.
// The sender's account (acc, written back after this) is credited directly
// when the hunter sends its own reveal.
func (s *State) applyBountyReveal(id Hash, b *Bounty, base, account uint64, from Address, acc *Account, ctx *BlockCtx) {
	ctx.MediaUsed += b.Size
	ctx.Burn += account
	s.place(b.DataRoot, b.Size, b.Media, b.Tier, b.Bucket)
	s.fund(b.Bucket, b.Held)
	pay := b.Escrow - b.Held - base - account
	if b.Hunter == from {
		acc.Balance += pay
	} else {
		s.Account(b.Hunter).Balance += pay
	}
	s.Bounties.Delete(id)
}

// bountyDue runs what falls due at height h for one bounty.
func (s *State) bountyDue(id Hash, h uint64) {
	b, ok := s.bounty(id)
	if !ok {
		return
	}
	switch {
	case b.Status == BountyOpen && h >= b.Deadline:
		s.startRefund(id)
	case b.Status == BountyAccepted && h > b.Accepted+s.P.BountyRevealBlocks:
		// The bounty was locked from the acceptance to now for nothing: it
		// gets that time back, so a hunter that never reveals cannot run
		// out the clock on it.
		locked := min(h-b.Accepted, s.P.BountyExtendMaxBlocks-b.Extended)
		b.Failed = append(b.Failed, b.Offer)
		b.Deadline += locked
		b.Extended += locked
		b.Status, b.Held, b.Offer, b.Hunter, b.DataRoot, b.Size, b.Media, b.Accepted, b.Bucket =
			BountyOpen, 0, Hash{}, Address{}, Hash{}, 0, 0, 0, 0
		s.Bounties.Set(id, b)
		if h >= b.Deadline {
			s.startRefund(id)
		} else if locked > 0 {
			s.due(b.Deadline, id)
		}
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
	w.U32(b.Bucket)
	w.Var(b.Held)
	w.Var(b.Extended)
	w.Var(uint64(len(b.Failed)))
	for _, f := range b.Failed {
		w.Fixed(f[:])
	}
	w.Var(b.RefundPot)
	w.Var(uint64(b.RefundAt))
	return sumOf("bounty", &w)
}
