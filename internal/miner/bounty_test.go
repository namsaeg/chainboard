package miner

import (
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

func bountyParams() *params.Params {
	p := FastParams()
	p.BountyMaxBlocks, p.BountyRevealBlocks, p.BountyExtendMaxBlocks = 60, 4, 8
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

func (b *bountyRig) revealTx() *types.Tx {
	x := b.bounty()
	return &types.Tx{Kind: types.TxBountyReveal, Target: b.id, DataRoot: x.DataRoot, Size: x.Size, MaxFee: 1}
}

func (b *bountyRig) reveal(t *testing.T) { b.send(t, b.hunter, b.revealTx()) }

// The whole way: likes raise the pot, only the requester accepts, the
// hunter (without coins) reveals, and the pot, less the result's storage,
// the reveal's fee and the hunter's new account, is the hunter's at once.
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
	b.pool.Reset(b.c.Tip())
	if b.try(b.requester, &types.Tx{Kind: types.TxBountyAccept, Offer: b.offer("another")}) == nil {
		t.Fatal("a second offer accepted while one is pending")
	}
	wrong := b.revealTx()
	wrong.Size++
	if b.try(b.hunter, wrong) == nil {
		t.Fatal("a reveal of other data accepted")
	}
	b.reveal(t)
	prev := b.c.Tip()
	b.mineN(t, 1)
	tip := b.c.Tip()
	var base uint64
	for _, tx := range b.c.BlockAt(tip.Height).Txs {
		if tx.Kind == types.TxBountyReveal {
			base = prev.BytesFee(tx)
		}
	}
	burned := prev.Burned
	if b.bounty() != nil || !tip.Data.Has(b.root) {
		t.Fatal("reveal did not store the data and settle the bounty")
	}
	// The reveal's own fee (its block bytes, at the base fee of its block)
	// is the only other cost.
	got := tip.Peek(b.hunter.Address()).Balance
	if fee := x.Escrow - x.Held - tip.NewAccountFee() - got; got > x.Escrow || fee == 0 || fee > 2*base {
		t.Fatalf("hunter got %d of a %d pot (held %d): fee %d, about %d expected", got, x.Escrow, x.Held, fee, base)
	}
	if tip.Burned < burned+tip.NewAccountFee() {
		t.Fatal("the hunter's new account was not paid for")
	}
	b.check(t)
}

// A hunter that does not reveal fails its offer (it cannot be accepted
// again); the bounty reopens with the locked blocks added to its deadline,
// and refunds at that later deadline.
func TestBountyNoReveal(t *testing.T) {
	b := newBountyRig(t, 12)
	deadline := b.bounty().Deadline
	o := b.offer("")
	b.send(t, b.requester, &types.Tx{Kind: types.TxBountyAccept, Offer: o, MaxFee: params.Coin})
	b.mineN(t, 1)
	accepted := b.c.Height()
	b.mineTo(t, accepted+b.p.BountyRevealBlocks+1)
	x := b.bounty()
	if x.Status != state.BountyOpen || len(x.Failed) != 1 || x.Failed[0] != o.ID() {
		t.Fatalf("after a missed reveal: %+v", x)
	}
	if locked := b.p.BountyRevealBlocks + 1; x.Deadline != deadline+locked || x.Extended != locked {
		t.Fatalf("deadline %d (was %d), extended %d", x.Deadline, deadline, x.Extended)
	}
	b.pool.Reset(b.c.Tip())
	if err := b.try(b.requester, &types.Tx{Kind: types.TxBountyAccept, Offer: o}); err == nil {
		t.Fatal("a failed offer accepted again")
	}
	before := b.c.Tip().Peek(b.backer.Address()).Balance
	b.send(t, b.backer, &types.Tx{Kind: types.TxLikes, Pledges: []types.Pledge{{Request: b.id, Amount: params.Coin}}, MaxFee: params.Coin})
	b.mineTo(t, deadline+1)
	if b.bounty() == nil {
		t.Fatal("refunded at the old deadline, not the extended one")
	}
	b.mineTo(t, x.Deadline)
	if b.bounty() != nil {
		t.Fatal("bounty not refunded at its extended deadline")
	}
	if got := b.c.Tip().Peek(b.backer.Address()).Balance; got+params.Coin/100 < before {
		t.Fatalf("backer's refund: %d -> %d", before, got)
	}
	b.pool.Reset(b.c.Tip())
	if b.try(b.requester, &types.Tx{Kind: types.TxBountyAccept, Offer: b.offer("late")}) == nil {
		t.Fatal("accepted after the deadline")
	}
	b.check(t)
}

// Failed reveals give back at most BountyExtendMaxBlocks in all, so a
// requester and a hunter of its own cannot keep the backers' coins locked
// for long.
func TestBountyExtensionCapped(t *testing.T) {
	b := newBountyRig(t, 30)
	deadline := b.bounty().Deadline
	for i := 0; i < 3; i++ {
		b.pool.Reset(b.c.Tip())
		b.send(t, b.requester, &types.Tx{Kind: types.TxBountyAccept, Offer: b.offer(fmt.Sprint("제안 ", i)), MaxFee: params.Coin})
		b.mineN(t, 1)
		b.mineN(t, b.p.BountyRevealBlocks+1)
	}
	x := b.bounty()
	if len(x.Failed) != 3 || x.Extended != b.p.BountyExtendMaxBlocks || x.Deadline != deadline+b.p.BountyExtendMaxBlocks {
		t.Fatalf("after three failures: failed %d, extended %d, deadline %d (was %d)", len(x.Failed), x.Extended, x.Deadline, deadline)
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
// reveal window past the deadline, and is paid when it reveals then.
func TestBountyLastMinute(t *testing.T) {
	b := newBountyRig(t, 8)
	deadline := b.bounty().Deadline
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
	b.reveal(t)
	b.mineN(t, 1)
	if got := b.c.Tip().Peek(b.hunter.Address()).Balance; b.bounty() != nil || got == 0 {
		t.Fatalf("late reveal not paid (%d)", got)
	}
	b.check(t)
}

// Data stored by someone else before the reveal (the requester, from the
// data the hunter shares) does not stop the hunter's payment.
func TestBountyFrontRunStore(t *testing.T) {
	b := newBountyRig(t, 40)
	b.send(t, b.requester, &types.Tx{Kind: types.TxBountyAccept, Offer: b.offer(""), MaxFee: params.Coin})
	b.mineN(t, 1)
	b.pool.Reset(b.c.Tip())
	b.send(t, b.requester, &types.Tx{Kind: types.TxUpload, DataRoot: b.root, Size: uint64(len(b.data)), Media: types.MediaImage,
		Tier: 1, MaxFee: params.Coin})
	b.mineN(t, 1)
	if !b.c.Tip().Data.Has(b.root) {
		t.Fatal("front-running upload not stored")
	}
	b.pool.Reset(b.c.Tip())
	b.reveal(t)
	b.mineN(t, 1)
	if got := b.c.Tip().Peek(b.hunter.Address()).Balance; b.bounty() != nil || got == 0 {
		t.Fatalf("hunter not paid after a front-running store (%d)", got)
	}
	if info, _ := b.c.Tip().Data.Get(b.root); info.Tier != 12 {
		t.Fatalf("data not moved to the bounty's tier: %+v", info)
	}
	b.check(t)
}
