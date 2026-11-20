package miner

import (
	"crypto/rand"
	"errors"
	"github.com/namsaeg/chainboard/internal/chain"
	"github.com/namsaeg/chainboard/internal/store"
	"testing"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

func fundParams() *params.Params {
	p := FastParams()
	p.FundMaxBlocks = 40
	return p
}

// fundRig is a chain with a funded backer and a creator who has data but
// no coins at all.
type fundRig struct {
	*rig
	backer, other, creator *cryptox.Key
	req                    *types.FundRequest
}

func newFundRig(t *testing.T, size int, minTier uint16, deadlineIn uint64) *fundRig {
	p := fundParams()
	f := &fundRig{rig: newRig(t, p, 8)}
	f.mineTo(t, 10)
	f.backer, f.other = f.m.Key, cryptox.KeyFromSeed(append(make([]byte, 31), 91))
	f.creator = cryptox.KeyFromSeed(append(make([]byte, 31), 92))
	f.send(t, f.backer, &types.Tx{Kind: types.TxTransfer, To: f.other.Address(), Amount: 10 * params.Coin, MaxFee: params.Coin})
	f.mineN(t, 1)
	data := make([]byte, size)
	rand.Read(data)
	root, err := f.st.PutData(data)
	if err != nil {
		t.Fatal(err)
	}
	f.req = &types.FundRequest{DataRoot: root, Size: uint64(size), Media: types.MediaImage, MinTier: minTier,
		Deadline: f.c.Height() + deadlineIn, Board: "후원", Note: "그림 한 장을 영구히 남기고 싶어요"}
	f.req.Sign(f.creator)
	f.pool.Reset(f.c.Tip())
	return f
}

func (f *fundRig) pledge(t *testing.T, k *cryptox.Key, amount uint64, carry bool) {
	tx := &types.Tx{Kind: types.TxLikes, Pledges: []types.Pledge{{Request: f.req.ID(), Amount: amount}}, MaxFee: params.Coin}
	if carry {
		tx.Requests = []*types.FundRequest{f.req}
	}
	f.send(t, k, tx)
}

func (f *fundRig) campaign() *state.Campaign {
	c, _ := f.c.Tip().Funds.Get(f.req.ID())
	return c
}

func (f *fundRig) store() *types.Tx {
	return &types.Tx{Kind: types.TxFundStore, Target: f.req.ID(), DataRoot: f.req.DataRoot, Size: f.req.Size, MaxFee: 1}
}

func (f *fundRig) try(k *cryptox.Key, tx *types.Tx) error {
	tx.Nonce = f.pool.PendingState().Peek(k.Address()).Nonce
	tx.Sign(k)
	return f.pool.Add(tx)
}

// A creator without coins is funded by two backers and stores the data in
// one step; what the tier did not use returns in proportion, and nothing
// can be taken back afterwards.
func TestFundSuccess(t *testing.T) {
	f := newFundRig(t, 350_000, 12, 30)
	f.pledge(t, f.backer, 3*params.Coin, true)
	f.pledge(t, f.other, 1*params.Coin, false)
	f.mineN(t, 1)
	c := f.campaign()
	if c == nil || c.Escrow != 4*params.Coin || c.Pledges.Len() != 2 {
		t.Fatalf("campaign after pledges: %+v", c)
	}
	f.check(t)
	otherBefore := f.c.Tip().Peek(f.other.Address()).Balance
	if f.try(f.other, f.store()) == nil {
		t.Fatal("someone else than the creator stored the data")
	}
	wrong := f.store()
	wrong.Size--
	if f.try(f.creator, wrong) == nil {
		t.Fatal("a store of other data accepted")
	}
	tier, _, _ := f.pool.PendingState().FundTier(c, f.c.Height()+1, f.pool.PendingState().BytesFee(f.store()))
	// The creator, with no coins, stores; the escrow pays.
	f.send(t, f.creator, f.store())
	f.mineN(t, 1)
	tip := f.c.Tip()
	if f.campaign() != nil {
		t.Fatal("campaign still open after its store")
	}
	info, ok := tip.Data.Get(f.req.DataRoot)
	if !ok || info.Tier != tier || tier < 12 {
		t.Fatalf("funded data not stored at its tier %d: %+v", tier, info)
	}
	refund := tip.Peek(f.other.Address()).Balance - otherBefore
	if refund == 0 || refund >= params.Coin {
		t.Fatalf("other backer's refund %d of a 1-coin pledge", refund)
	}
	f.pool.Reset(tip)
	late := &types.Tx{Kind: types.TxLikes, Pledges: []types.Pledge{{Request: f.req.ID(), Amount: params.Coin}}, MaxFee: params.Coin}
	if f.try(f.other, late) == nil {
		t.Fatal("pledge after the store accepted")
	}
	f.check(t)
	// The data stays; nothing returns later.
	f.mineN(t, 4)
	if tip2 := f.c.Tip(); tip2.Peek(f.other.Address()).Balance != tip.Peek(f.other.Address()).Balance || !tip2.Data.Has(f.req.DataRoot) {
		t.Fatal("something moved after the campaign was done")
	}
	f.check(t)
}

// Below the minimum tier no store is possible; at the deadline every
// pledge returns in full.
func TestFundRefundAtDeadline(t *testing.T) {
	f := newFundRig(t, 400_000, params.TierPermanent, 8)
	amount := f.c.Tip().MinPledge() * 10
	f.pledge(t, f.other, amount, true)
	f.mineN(t, 1)
	before := f.c.Tip().Peek(f.other.Address()).Balance
	f.pool.Reset(f.c.Tip())
	if f.try(f.creator, f.store()) == nil {
		t.Fatal("store accepted below the minimum tier")
	}
	f.mineTo(t, f.req.Deadline)
	if f.campaign() != nil {
		t.Fatal("campaign kept past its deadline")
	}
	if back := f.c.Tip().Peek(f.other.Address()).Balance - before; back != amount {
		t.Fatalf("refund %d of a %d pledge", back, amount)
	}
	f.check(t)
}

// TestPaidDataWaitsForChunks: a node accepts a recent block that stores
// funded data only once every chunk of that data is here. The block is not
// invalid meanwhile: it waits, and goes in as soon as the data is complete.
func TestPaidDataWaitsForChunks(t *testing.T) {
	f := newFundRig(t, 300_000, 1, 30)
	f.pledge(t, f.backer, 2*params.Coin, true)
	f.mineN(t, 1)
	f.send(t, f.creator, f.store())
	f.mineN(t, 1)
	if f.c.Tip().Data.Has(f.req.DataRoot) == false {
		t.Fatal("store not mined")
	}

	gate := &dataGate{inner: f.st, hold: f.req.DataRoot}
	fresh, err := chain.Open(f.p, "")
	if err != nil {
		t.Fatal(err)
	}
	fresh.Texts, fresh.Data, fresh.TextWindowSec = f.st, gate, 1<<50
	var waited bool
	for h := uint64(1); h <= f.c.Height(); h++ {
		err := fresh.Add(f.c.BlockAt(h))
		var md *chain.MissingDataError
		if errors.As(err, &md) {
			if md.Root != f.req.DataRoot || md.Size != f.req.Size || waited {
				t.Fatalf("waiting for %x (%d bytes)", md.Root, md.Size)
			}
			waited = true
			if fresh.Height() != h-1 {
				t.Fatal("block taken without its data")
			}
			gate.hold = types.Hash{} // the data arrives
			err = fresh.Add(f.c.BlockAt(h))
		}
		if err != nil {
			t.Fatalf("block %d: %v", h, err)
		}
	}
	if !waited || fresh.Tip().Root() != f.c.Tip().Root() {
		t.Fatal("the store block did not wait for its data, or the chains differ")
	}
}

// dataGate reports one data root incomplete while held.
type dataGate struct {
	inner *store.Store
	hold  types.Hash
}

func (g *dataGate) Complete(root types.Hash) bool { return root != g.hold && g.inner.Complete(root) }
