package miner

import (
	"crypto/rand"
	"testing"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

func bountyParams() *params.Params {
	p := FastParams()
	p.BountyMaxBlocks, p.BountyRevealBlocks, p.BountyDisputeBlocks = 60, 4, 3
	return p
}

// bountyRig: a requester with coins, a backer, and a hunter with data but
// no coins.
type bountyRig struct {
	*rig
	requester, backer, hunter *cryptox.Key
	id                        types.Hash
	data                      []byte
	root                      types.Hash
}

func newBountyRig(t *testing.T, deadlineIn uint64) *bountyRig {
	b := &bountyRig{rig: newRig(t, bountyParams(), 9)}
	b.mineTo(t, 10)
	b.requester = b.m.Key
	b.backer = cryptox.KeyFromSeed(append(make([]byte, 31), 71))
	b.hunter = cryptox.KeyFromSeed(append(make([]byte, 31), 72))
	b.send(t, b.requester, &types.Tx{Kind: types.TxTransfer, To: b.backer.Address(), Amount: 10 * params.Coin, MaxFee: params.Coin})
	tx := &types.Tx{Kind: types.TxBounty, Board: "현상금", Note: "비 오는 광화문 사진", Amount: 2 * params.Coin,
		Deadline: b.c.Height() + deadlineIn, Tier: 12, MaxFee: params.Coin}
	b.send(t, b.requester, tx)
	b.id = tx.ID()
	b.mineN(t, 1)
	b.data = make([]byte, 300_000)
	rand.Read(b.data)
	var err error
	if b.root, err = b.st.PutData(b.data); err != nil {
		t.Fatal(err)
	}
	b.pool.Reset(b.c.Tip())
	return b
}

func (b *bountyRig) bounty() *state.Bounty {
	x, _ := b.c.Tip().Bounties.Get(b.id)
	return x
}

func (b *bountyRig) offer(note string) *types.Offer {
	o := &types.Offer{Bounty: b.id, DataRoot: b.root, Size: uint64(len(b.data)), Media: types.MediaImage, Note: note}
	o.Sign(b.hunter)
	return o
}

func (b *bountyRig) try(k *cryptox.Key, tx *types.Tx) error {
	tx.Nonce = b.pool.PendingState().Peek(k.Address()).Nonce
	if tx.MaxFee == 0 {
		tx.MaxFee = params.Coin
	}
	tx.Sign(k)
	return b.pool.Add(tx)
}

func (b *bountyRig) reveal(t *testing.T) {
	x := b.bounty()
	samples, err := FundSamples(b.st, x.Seed, b.id, x.DataRoot, x.Size)
	if err != nil {
		t.Fatal(err)
	}
	b.send(t, b.hunter, &types.Tx{Kind: types.TxBountyProof, Target: b.id, Samples: samples, MaxFee: 1})
}

// The whole way: likes raise the pot, only the requester accepts, the
// hunter (without coins) reveals, and after the dispute window the pot,
// less the result's storage, is the hunter's.
func TestBountyPaid(t *testing.T) {
	b := newBountyRig(t, 40)
	b.send(t, b.backer, &types.Tx{Kind: types.TxLikes, Pledges: []types.Pledge{{Request: b.id, Amount: params.Coin}}, MaxFee: params.Coin})
	b.mineN(t, 1)
	if x := b.bounty(); x == nil || x.Escrow != 3*params.Coin || x.Pledges.Len() != 2 {
		t.Fatalf("bounty after a like: %+v", x)
	}
	if b.try(b.backer, &types.Tx{Kind: types.TxBountyAccept, Offer: b.offer("")}) == nil {
		t.Fatal("someone else than the requester accepted")
	}
	b.send(t, b.requester, &types.Tx{Kind: types.TxBountyAccept, Offer: b.offer("비 오는 날 찍었어요"), MaxFee: params.Coin})
	b.mineN(t, 1)
	x := b.bounty()
	if x.Status != state.BountyAccepted || x.Held == 0 || x.Hunter != b.hunter.Address() {
		t.Fatalf("after acceptance: %+v", x)
	}
	if b.try(b.requester, &types.Tx{Kind: types.TxBountyAccept, Offer: b.offer("another")}) == nil {
		t.Fatal("a second offer accepted while one is pending")
	}
	b.mineN(t, 1) // the seed: vdf output of the block after the acceptance
	b.pool.Reset(b.c.Tip())
	b.reveal(t)
	b.mineN(t, 1)
	tip := b.c.Tip()
	if x := b.bounty(); x.Status != state.BountyProven || !tip.Data.Has(b.root) {
		t.Fatalf("after the reveal: %+v", x)
	}
	pot := b.bounty().Escrow
	b.mineN(t, b.c.Tip().P.BountyDisputeBlocks+1)
	tip = b.c.Tip()
	if b.bounty() != nil {
		t.Fatal("bounty still open after the dispute window")
	}
	// The hunter's account exists (it sent the reveal), so no account fee.
	if got := tip.Peek(b.hunter.Address()).Balance; got != pot {
		t.Fatalf("hunter got %d of a %d pot", got, pot)
	}
	b.check(t)
}

// A dispute burns the pot; it does not come back to the requester.
func TestBountyDispute(t *testing.T) {
	b := newBountyRig(t, 40)
	b.send(t, b.requester, &types.Tx{Kind: types.TxBountyAccept, Offer: b.offer(""), MaxFee: params.Coin})
	b.mineN(t, 2)
	b.pool.Reset(b.c.Tip())
	b.reveal(t)
	b.mineN(t, 1)
	b.pool.Reset(b.c.Tip())
	if b.try(b.hunter, &types.Tx{Kind: types.TxBountyDispute, Target: b.id}) == nil {
		t.Fatal("someone else than the requester disputed")
	}
	burned, mine := b.c.Tip().Burned, b.c.Tip().Peek(b.requester.Address()).Balance
	pot := b.bounty().Escrow
	b.send(t, b.requester, &types.Tx{Kind: types.TxBountyDispute, Target: b.id, MaxFee: params.Coin})
	b.mineN(t, 1)
	tip := b.c.Tip()
	if b.bounty() != nil || tip.Burned < burned+pot || tip.Peek(b.hunter.Address()).Balance != 0 {
		t.Fatalf("dispute: burned %d -> %d (pot %d)", burned, tip.Burned, pot)
	}
	if tip.Peek(b.requester.Address()).Balance > mine+tip.BlockReward(tip.Height)*2 {
		t.Fatal("the requester got the pot back")
	}
	b.check(t)
}

// A hunter that does not reveal fails its offer (it cannot be accepted
// again); the bounty reopens, and past the deadline it refunds.
func TestBountyNoReveal(t *testing.T) {
	b := newBountyRig(t, 12)
	o := b.offer("")
	b.send(t, b.requester, &types.Tx{Kind: types.TxBountyAccept, Offer: o, MaxFee: params.Coin})
	b.mineN(t, b.c.Tip().P.BountyRevealBlocks+2)
	x := b.bounty()
	if x.Status != state.BountyOpen || len(x.Failed) != 1 || x.Failed[0] != o.ID() {
		t.Fatalf("after a missed reveal: %+v", x)
	}
	b.pool.Reset(b.c.Tip())
	if err := b.try(b.requester, &types.Tx{Kind: types.TxBountyAccept, Offer: o}); err == nil {
		t.Fatal("a failed offer accepted again")
	}
	before := b.c.Tip().Peek(b.backer.Address()).Balance
	b.send(t, b.backer, &types.Tx{Kind: types.TxLikes, Pledges: []types.Pledge{{Request: b.id, Amount: params.Coin}}, MaxFee: params.Coin})
	b.mineTo(t, x.Deadline+1)
	if b.bounty() != nil {
		t.Fatal("bounty not refunded at its deadline")
	}
	if got := b.c.Tip().Peek(b.backer.Address()).Balance; got+params.Coin/100 < before {
		t.Fatalf("backer's refund: %d -> %d", before, got)
	}
	b.pool.Reset(b.c.Tip())
	err := b.try(b.requester, &types.Tx{Kind: types.TxBountyAccept, Offer: b.offer("late")})
	if err == nil {
		t.Fatal("accepted after the deadline")
	}
	b.check(t)
}

// Refunds of many backers spread over blocks; the supply holds throughout.
func TestRefundsSpread(t *testing.T) {
	old := state.RefundsPerBlock
	state.RefundsPerBlock = 2
	t.Cleanup(func() { state.RefundsPerBlock = old }) // after the rig's replay check

	b := newBountyRig(t, 6)
	for i := 0; i < 4; i++ {
		b.send(t, b.backer, &types.Tx{Kind: types.TxLikes, Pledges: []types.Pledge{{Request: b.id, Amount: params.Coin / 10}}, MaxFee: params.Coin})
	}
	b.mineN(t, 1)
	b.mineTo(t, b.bounty().Deadline)
	if x := b.bounty(); x == nil || x.Status != state.BountyRefunding || x.RefundAt != 2 || len(b.c.Tip().Refunding) != 1 {
		t.Fatalf("first refund block: %+v", x)
	}
	b.check(t)
	b.mineN(t, 2)
	if b.bounty() != nil || len(b.c.Tip().Refunding) != 0 {
		t.Fatal("refund did not finish")
	}
	b.check(t)
}

// An offer accepted in the last block before the deadline keeps its full
// reveal and dispute windows past the deadline; the requester cannot take
// the pot back in the meantime, nor dispute before a reveal.
func TestBountyLastMinute(t *testing.T) {
	b := newBountyRig(t, 8)
	deadline := b.bounty().Deadline
	if b.try(b.requester, &types.Tx{Kind: types.TxBountyDispute, Target: b.id}) == nil {
		t.Fatal("disputed an open bounty (an early withdrawal)")
	}
	b.mineTo(t, deadline-2)
	b.pool.Reset(b.c.Tip())
	b.send(t, b.requester, &types.Tx{Kind: types.TxBountyAccept, Offer: b.offer("마감 직전"), MaxFee: params.Coin})
	b.mineN(t, 1)
	if h, x := b.c.Height(), b.bounty(); h != deadline-1 || x.Status != state.BountyAccepted {
		t.Fatalf("at %d (deadline %d): %+v", h, deadline, x)
	}
	b.mineN(t, 2) // past the deadline, still within the reveal window
	if x := b.bounty(); x == nil || x.Status != state.BountyAccepted {
		t.Fatalf("an accepted bounty refunded at its deadline: %+v", x)
	}
	b.pool.Reset(b.c.Tip())
	if b.try(b.requester, &types.Tx{Kind: types.TxBountyDispute, Target: b.id}) == nil {
		t.Fatal("disputed before the reveal")
	}
	b.reveal(t)
	b.mineN(t, 1)
	pot := b.bounty().Escrow
	b.mineN(t, b.c.Tip().P.BountyDisputeBlocks+1)
	if got := b.c.Tip().Peek(b.hunter.Address()).Balance; b.bounty() != nil || got != pot {
		t.Fatalf("late reveal paid %d of %d", got, pot)
	}
	b.check(t)
}
