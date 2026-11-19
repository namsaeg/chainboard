package miner

import (
	"errors"
	"testing"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"

	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/rln"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

func anon(t *testing.T, st *state.State, sec fr.Element, text string, msgID uint64) *types.Tx {
	t.Helper()
	tx := &types.Tx{Kind: types.TxAnonPost, Board: "익명", Tier: params.TierFree}
	tx.SetText([]byte(text))
	if err := st.ProveAnon(tx, sec, msgID); err != nil {
		t.Fatal(err)
	}
	return tx
}

// TestRLNLane covers membership, rate limiting, double-signal slashing and
// exit of the anonymous lane, with real Groth16 proofs.
func TestRLNLane(t *testing.T) {
	p := FastParams()
	p.RLNEpochBlocks = 1000 // the whole test runs inside one epoch
	r := newRig(t, p, 4)
	r.mineTo(t, 10)
	k := r.m.Key
	r.pool.Reset(r.c.Tip())
	r.send(t, k, &types.Tx{Kind: types.TxStake, Amount: 10 * params.Coin, MaxFee: params.Coin})
	r.mineN(t, 1+p.StakeAgingBlocks)

	var sec, sec2 fr.Element
	sec.SetUint64(424242)
	sec2.SetUint64(777)
	idc := rln.ToBytes(rln.IdentityCommitment(sec))
	idc2 := rln.ToBytes(rln.IdentityCommitment(sec2))
	r.pool.Reset(r.c.Tip())
	r.send(t, k, &types.Tx{Kind: types.TxRLNJoin, Amount: 3 * params.Coin, Commitment: idc, Limit: 3, MaxFee: params.Coin})
	r.send(t, k, &types.Tx{Kind: types.TxRLNJoin, Amount: 1 * params.Coin, Commitment: idc2, Limit: 1, MaxFee: params.Coin})
	r.mineN(t, 1)
	st := r.c.Tip()
	if memberOf(st, idc) == nil || memberOf(st, idc).Status != state.MemberActive {
		t.Fatal("membership not active")
	}

	// Two posts within the limit; a third is prepared now (msgID 2) and used
	// after the member is slashed.
	r.pool.Reset(st)
	a0 := anon(t, st, sec, "첫 익명 글", 0)
	a1 := anon(t, st, sec, "두 번째 익명 글", 1)
	late := anon(t, st, sec, "슬래시 뒤에 도착한 글", 2)
	for _, a := range []*types.Tx{a0, a1} {
		if err := r.pool.Add(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.pool.Add(anon(t, st, sec, "첫 익명 글", 0)); !errors.Is(err, state.ErrDuplicateAnon) {
		t.Fatalf("resend of the same post: %v", err)
	}
	over := &types.Tx{Kind: types.TxAnonPost, Board: "익명", Tier: params.TierFree}
	over.SetText([]byte("x"))
	if st.ProveAnon(over, sec, 3) == nil {
		t.Fatal("message beyond the limit was provable")
	}

	// Reusing message 0 for other content reveals the secret.
	var caught *state.DoubleSignal
	r.pool.OnDoubleSignal = func(ds *state.DoubleSignal) { caught = ds }
	err := r.pool.Add(anon(t, st, sec, "도배 글", 0))
	var ds *state.DoubleSignal
	if !errors.As(err, &ds) || caught == nil {
		t.Fatalf("double signal not detected: %v", err)
	}
	secret, ok := ds.Secret()
	if !ok || secret != rln.ToBytes(sec) {
		t.Fatal("secret not recovered")
	}
	r.mineN(t, 1)
	st = r.c.Tip()
	if pi := postOf(st, a0.ID()); pi == nil || pi.Author != (types.Address{}) {
		t.Fatal("anonymous post not mined or not anonymous")
	}

	// Slash: the member is removed, half the bond burned, old roots dropped.
	burned := st.Burned
	r.pool.Reset(st)
	if err := r.pool.Add(&types.Tx{Kind: types.TxRLNSlash, Secret: secret}); err != nil {
		t.Fatal(err)
	}
	r.mineN(t, 1)
	st = r.c.Tip()
	if m := memberOf(st, idc); m.Status != state.MemberGone || m.Bond != 0 {
		t.Fatal("member not slashed")
	}
	if st.Burned < burned+3*params.Coin/2 || len(st.RLNRoots) != 1 {
		t.Fatalf("slash: burned %d -> %d, roots %d", burned, st.Burned, len(st.RLNRoots))
	}
	r.pool.Reset(st)
	if err := r.pool.Add(late); err == nil {
		t.Fatal("post against a pre-slash root accepted")
	}
	if err := r.pool.Add(&types.Tx{Kind: types.TxRLNSlash, Secret: secret}); err == nil {
		t.Fatal("double slash accepted")
	}
	// A spent commitment can never join again.
	rejoin := &types.Tx{Kind: types.TxRLNJoin, Amount: params.Coin, Commitment: idc, Limit: 1, MaxFee: params.Coin,
		Nonce: r.pool.PendingState().Peek(k.Address()).Nonce}
	rejoin.Sign(k)
	if r.pool.Add(rejoin) == nil {
		t.Fatal("slashed commitment joined again")
	}

	// The second member posts, leaves, may still post while unbonding, and
	// is released afterwards.
	if err := r.pool.Add(anon(t, st, sec2, "두 번째 회원 글", 0)); err != nil {
		t.Fatal(err)
	}
	r.send(t, k, &types.Tx{Kind: types.TxRLNLeave, Commitment: idc2, MaxFee: params.Coin})
	r.mineN(t, 1)
	st = r.c.Tip()
	m2 := memberOf(st, idc2)
	if m2.Status != state.MemberLeaving || m2.Bond != params.Coin {
		t.Fatal("leave not recorded")
	}
	r.mineTo(t, m2.Release)
	st = r.c.Tip()
	m := memberOf(st, idc2)
	if leaf := st.RLNTree.Leaf(m.Index); m.Status != state.MemberGone || m.Bond != 0 || !leaf.IsZero() {
		t.Fatal("membership not released")
	}
	r.check(t)
}
