package miner

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/namsaeg/chainboard/internal/chain"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/mempool"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/store"
	"github.com/namsaeg/chainboard/internal/types"
)

// FastParams is a devnet tuned for unit tests.
func FastParams() *params.Params {
	p := params.Devnet()
	p.VDFIterations = 50
	p.BalloonSpace = 16
	p.GenesisTarget = new(big.Int).Lsh(big.NewInt(1), 254)
	// Compressed calendar: a "day" is 10 blocks, a month 20, a year 240.
	p.BlocksPerDay, p.BlocksPerMonth, p.BlocksPerYear = 10, 20, 240
	p.HalvingBlocks = 1_000_000
	p.PriceEMABlocks = 50
	return p
}

type rig struct {
	p     *params.Params
	c     *chain.Chain
	pool  *mempool.Pool
	st    *store.Store
	m     *Miner
	clock int64
}

func newRig(t *testing.T, p *params.Params, seed byte) *rig {
	c, err := chain.Open(p, "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutData(c.GenesisData); err != nil {
		t.Fatal(err)
	}
	r := &rig{p: p, c: c, st: st, clock: state.GenesisTime}
	r.pool = mempool.New(c.Tip(), nil)
	key := cryptox.KeyFromSeed(append(make([]byte, 31), seed))
	r.m = &Miner{Chain: c, Pool: r.pool, Store: st, Key: key, PledgeBytes: 1 << 40,
		Logf: t.Logf, Now: func() int64 { r.clock += int64(p.BlockTime.Seconds()); return r.clock }}
	return r
}

// mineTo mines until the chain reaches height h.
func (r *rig) mineTo(t *testing.T, h uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for r.c.Height() < h {
		if ctx.Err() != nil {
			t.Fatalf("timeout at height %d", r.c.Height())
		}
		tip := r.c.Tip()
		r.pool.Reset(tip)
		r.m.Prepare(tip)
		r.m.mineOn(ctx, tip, make(chan *state.State))
	}
}

func TestMineAndPost(t *testing.T) {
	p := FastParams()
	r := newRig(t, p, 1)
	r.mineTo(t, 15)
	tip := r.c.Tip()
	if err := tip.CheckSupply(); err != nil {
		t.Fatal(err)
	}
	addr := r.m.Key.Address()
	acc := tip.Peek(addr)
	if acc.Balance == 0 || len(acc.Vesting) == 0 {
		t.Fatalf("miner got no income: %+v", acc)
	}
	// Fee-lane post.
	post := &types.Tx{Kind: types.TxPost, Nonce: acc.Nonce, Lane: types.LaneFee, MaxFee: 10 * params.Coin,
		Board: "free", Text: []byte("안녕하세요 chainboard")}
	post.Sign(r.m.Key)
	r.pool.Reset(tip)
	if err := r.pool.Add(post); err != nil {
		t.Fatal(err)
	}
	r.mineTo(t, tip.Height+1)
	tip = r.c.Tip()
	if _, ok := tip.Posts[post.ID()]; !ok {
		t.Fatal("post not mined")
	}
	// Free-lane comment backed by locked income (counts as stake).
	acc = tip.Peek(addr)
	cm := &types.Tx{Kind: types.TxPost, Nonce: acc.Nonce, Lane: types.LaneFree, Board: "free",
		Text: []byte("무료 차선 댓글"), Parent: post.ID()}
	cm.Sign(r.m.Key)
	r.pool.Reset(tip)
	if err := r.pool.Add(cm); err != nil {
		t.Fatal(err)
	}
	// Replying to a comment is not allowed.
	bad := &types.Tx{Kind: types.TxPost, Nonce: acc.Nonce + 1, Lane: types.LaneFree, Board: "free",
		Text: []byte("답글"), Parent: cm.ID()}
	bad.Sign(r.m.Key)
	if err := r.pool.Add(bad); err == nil {
		t.Fatal("reply to a comment accepted")
	}
	r.mineTo(t, tip.Height+1)
	tip = r.c.Tip()
	if tip.Posts[post.ID()].Comments != 1 {
		t.Fatal("comment not counted")
	}
	if err := tip.CheckSupply(); err != nil {
		t.Fatal(err)
	}
}
