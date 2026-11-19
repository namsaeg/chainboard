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
	p.PackMemoryKiB = 8
	p.GenesisTarget = new(big.Int).Lsh(big.NewInt(1), 254)
	// Compressed calendar: a "day" is 10 blocks, a month 20, a year 240.
	p.BlocksPerDay, p.BlocksPerMonth, p.BlocksPerYear = 10, 20, 240
	p.HalvingBlocks = 1_000_000
	p.PriceEMABlocks = 50
	return p
}

type rig struct {
	p       *params.Params
	c       *chain.Chain
	pool    *mempool.Pool
	st      *store.Store
	m       *Miner
	clock   int64
	history []version
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
	t.Cleanup(func() {
		if !t.Failed() {
			r.checkVersions(t)
		}
	})
	r.pool = mempool.New(c.Tip(), nil)
	r.pool.Bodies = st
	c.Texts = st
	key := cryptox.KeyFromSeed(append(make([]byte, 31), seed))
	r.m = &Miner{Chain: c, Pool: r.pool, Store: st, Key: key, PledgeBytes: 1 << 40,
		Logf: t.Logf, Now: func() int64 { r.clock += int64(p.BlockTime.Seconds()); return r.clock }}
	return r
}

// textPost is a fee-lane post kept for a month.
func textPost(board, text string, parent types.Hash) *types.Tx {
	t := &types.Tx{Kind: types.TxPost, Lane: types.LaneFee, Board: board, Tier: params.TierFree, Parent: parent}
	t.SetText([]byte(text))
	return t
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
		if cur := r.c.Tip(); cur != tip {
			r.history = append(r.history, version{cur, cur.Digest()})
		}
	}
}

// version is a state the chain handed out, with its digest at the time.
type version struct {
	st *state.State
	d  state.Hash
}

// checkVersions runs after every rig test: no later block changed a state
// version handed out earlier (copy-on-write held), and replaying the chain
// from genesis reaches the same state (application is deterministic).
func (r *rig) checkVersions(t *testing.T) {
	for _, v := range r.history {
		if v.st.Digest() != v.d {
			t.Fatalf("state at height %d changed after it was handed out", v.st.Height)
		}
	}
	fresh, err := chain.Open(r.p, "")
	if err != nil {
		t.Fatal(err)
	}
	for h := uint64(1); h <= r.c.Height(); h++ {
		if err := fresh.Add(r.c.BlockAt(h)); err != nil {
			t.Fatalf("replay at %d: %v", h, err)
		}
	}
	if fresh.Tip().Digest() != r.c.Tip().Digest() {
		t.Fatal("replaying the chain reached a different state")
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
	post := textPost("free", "안녕하세요 chainboard", types.Hash{})
	post.Nonce, post.MaxFee = acc.Nonce, 10*params.Coin
	post.Sign(r.m.Key)
	r.pool.Reset(tip)
	if err := r.pool.Add(post); err != nil {
		t.Fatal(err)
	}
	r.mineTo(t, tip.Height+1)
	tip = r.c.Tip()
	if _, ok := tip.Posts.Get(post.ID()); !ok {
		t.Fatal("post not mined")
	}
	// A comment, and a reply to that comment (not allowed).
	acc = tip.Peek(addr)
	cm := textPost("free", "댓글", post.ID())
	cm.Nonce, cm.MaxFee = acc.Nonce, 10*params.Coin
	cm.Sign(r.m.Key)
	r.pool.Reset(tip)
	if err := r.pool.Add(cm); err != nil {
		t.Fatal(err)
	}
	bad := textPost("free", "답글", cm.ID())
	bad.Nonce, bad.MaxFee = acc.Nonce+1, 10*params.Coin
	bad.Sign(r.m.Key)
	if err := r.pool.Add(bad); err == nil {
		t.Fatal("reply to a comment accepted")
	}
	r.mineTo(t, tip.Height+1)
	tip = r.c.Tip()
	if pi := postOf(tip, cm.ID()); pi.Parent != post.ID() {
		t.Fatal("comment not recorded")
	}
	if err := tip.CheckSupply(); err != nil {
		t.Fatal(err)
	}
}
