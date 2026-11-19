package miner

import (
	"crypto/rand"
	"testing"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

func fundParams() *params.Params {
	p := FastParams()
	p.FundMaxBlocks, p.FundProofBlocks = 40, 6
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

func (f *fundRig) prove(t *testing.T) *types.Tx {
	c := f.campaign()
	samples, err := FundSamples(f.st, c, f.req.ID())
	if err != nil {
		t.Fatal(err)
	}
	return &types.Tx{Kind: types.TxFundProof, Target: f.req.ID(), Samples: samples, MaxFee: 1}
}

// A creator without coins is funded by two backers, uploads, proves, and
// the data is stored; what the tier did not use returns in proportion, and
// nothing can be taken back afterwards.
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
	early := &types.Tx{Kind: types.TxFundUpload, Target: f.req.ID(), MaxFee: 1, Nonce: f.pool.PendingState().Peek(f.other.Address()).Nonce}
	early.Sign(f.other)
	if f.pool.Add(early) == nil {
		t.Fatal("someone else than the creator sent the upload")
	}
	// The creator, with no coins, sends the upload; the escrow pays.
	f.send(t, f.creator, &types.Tx{Kind: types.TxFundUpload, Target: f.req.ID(), MaxFee: 1})
	f.mineN(t, 1)
	c = f.campaign()
	if c.Status != state.FundUploaded || c.Tier == 0 {
		t.Fatalf("after upload: %+v", c)
	}
	late := &types.Tx{Kind: types.TxLikes, Pledges: []types.Pledge{{Request: f.req.ID(), Amount: params.Coin}}, MaxFee: params.Coin,
		Nonce: f.pool.PendingState().Peek(f.other.Address()).Nonce}
	late.Sign(f.other)
	if f.pool.Add(late) == nil {
		t.Fatal("pledge after the upload accepted")
	}
	f.mineN(t, 2) // the seed: the block after the upload
	if f.campaign().Seed.IsZero() {
		t.Fatal("no proof seed")
	}
	f.pool.Reset(f.c.Tip())
	f.send(t, f.creator, f.prove(t))
	f.mineN(t, 1)
	tip := f.c.Tip()
	if f.campaign() != nil {
		t.Fatal("campaign still open after its proof")
	}
	info, ok := tip.Data.Get(f.req.DataRoot)
	if !ok || info.Tier != c.Tier {
		t.Fatal("funded data not stored at its tier")
	}
	refund := tip.Peek(f.other.Address()).Balance - otherBefore
	if refund == 0 || refund >= params.Coin {
		t.Fatalf("other backer's refund %d of a 1-coin pledge", refund)
	}
	f.check(t)
	// The data stays; nothing returns later.
	f.mineN(t, f.p.FundProofBlocks+2)
	if tip2 := f.c.Tip(); tip2.Peek(f.other.Address()).Balance != tip.Peek(f.other.Address()).Balance || !tip2.Data.Has(f.req.DataRoot) {
		t.Fatal("something moved after the campaign was done")
	}
	f.check(t)
}

// Below the minimum tier no upload is possible; at the deadline every
// pledge returns in full.
func TestFundRefundAtDeadline(t *testing.T) {
	f := newFundRig(t, 400_000, params.TierPermanent, 8)
	amount := f.c.Tip().MinPledge() * 10
	f.pledge(t, f.other, amount, true)
	f.mineN(t, 1)
	before := f.c.Tip().Peek(f.other.Address()).Balance
	up := &types.Tx{Kind: types.TxFundUpload, Target: f.req.ID(), MaxFee: 1, Nonce: 0}
	up.Sign(f.creator)
	if f.pool.Add(up) == nil {
		t.Fatal("upload accepted below the minimum tier")
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

// An upload without its proof in the window returns the escrow minus the
// fee the upload spent.
func TestFundRefundWithoutProof(t *testing.T) {
	f := newFundRig(t, 300_000, 1, 30)
	f.pledge(t, f.backer, 2*params.Coin, true)
	f.mineN(t, 1)
	before := f.c.Tip().Peek(f.backer.Address())
	f.send(t, f.creator, &types.Tx{Kind: types.TxFundUpload, Target: f.req.ID(), MaxFee: 1})
	f.mineN(t, 1)
	up := f.c.Tip().Height
	f.mineTo(t, up+f.p.FundProofBlocks+1)
	if f.campaign() != nil {
		t.Fatal("campaign kept past its proof window")
	}
	if f.c.Tip().Data.Has(f.req.DataRoot) {
		t.Fatal("unproven data stored")
	}
	after := f.c.Tip().Peek(f.backer.Address())
	if gained := after.Balance + 0 - before.Balance; gained < 2*params.Coin*9/10 {
		t.Fatalf("refund %d", gained)
	}
	f.check(t)
}
