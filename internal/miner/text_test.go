package miner

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/namsaeg/chainboard/internal/chain"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/store"
	"github.com/namsaeg/chainboard/internal/types"
)

// mineToSeal mines through the next seal block.
func (r *rig) mineToSeal(t *testing.T) {
	t.Helper()
	tip := r.c.Tip()
	h := tip.Height + 1
	for !tip.SealsAt(h) {
		h++
	}
	r.mineTo(t, h)
}

func bodyAt(t *testing.T, st *store.Store, pi *state.PostInfo) string {
	t.Helper()
	if pi.TextRoot.IsZero() {
		t.Fatal("text not sealed")
	}
	data, err := st.Data(pi.TextRoot)
	if err != nil {
		t.Fatal(err)
	}
	return string(data[pi.TextOff : pi.TextOff+uint32(pi.TextLen)])
}

// TestTextSealExtendExpire: post bodies are not in blocks; the seal block
// bundles them into the weave per retention bucket, an extension moves a
// body to a longer bucket at the next seal, and a body expires with its
// bucket while the post itself stays on chain.
func TestTextSealExtendExpire(t *testing.T) {
	p := FastParams()
	r := newRig(t, p, 9)
	// Recent block times, so the chain demands bodies and checks bundles.
	r.clock = time.Now().Unix() - 3600
	r.mineTo(t, 6)
	k := r.m.Key

	short := textPost("자유", "한 달만 보관되는 글", types.Hash{})
	keep := textPost("자유", "영구히 보관되는 글", types.Hash{})
	keep.Tier = params.TierPermanent
	later := textPost("자유", "나중에 연장할 글", types.Hash{})
	for _, tx := range []*types.Tx{short, keep, later} {
		tx.MaxFee = params.Coin
		r.send(t, k, tx)
	}
	r.mineN(t, 1)
	if raw := r.c.BlockAt(r.c.Height()).Encode(); bytes.Contains(raw, short.Text) || bytes.Contains(raw, keep.Text) {
		t.Fatal("a block carries a post body")
	}
	tip := r.c.Tip()
	if !postOf(tip, short.ID()).TextRoot.IsZero() {
		t.Fatal("sealed before the seal block")
	}
	r.mineToSeal(t)
	tip = r.c.Tip()
	for _, tx := range []*types.Tx{short, keep, later} {
		if got := bodyAt(t, r.st, postOf(tip, tx.ID())); got != string(tx.Text) {
			t.Fatalf("sealed body %q, want %q", got, tx.Text)
		}
	}
	if b := postOf(tip, keep.ID()).TextBucket; b != 0 {
		t.Fatalf("permanent text in bucket %d", b)
	}

	// Anyone extends a post: the body travels with the extend.
	ext := &types.Tx{Kind: types.TxExtend, Target: later.ID(), Tier: 12, TextHash: later.TextHash,
		TextLen: later.TextLen, Text: later.Text, MaxFee: params.Coin}
	r.send(t, k, ext)
	r.mineToSeal(t)
	tip = r.c.Tip()
	pl := postOf(tip, later.ID())
	if pl.TextTier != 12 || pl.TextBucket <= postOf(tip, short.ID()).TextBucket {
		t.Fatalf("extend not recorded: %+v", pl)
	}
	if got := bodyAt(t, r.st, pl); got != string(later.Text) {
		t.Fatalf("extended body %q", got)
	}

	// A seal block whose bundle does not match the bodies is refused.
	r.send(t, k, func() *types.Tx {
		x := textPost("자유", "봉인 위조 시험", types.Hash{})
		x.MaxFee = params.Coin
		return x
	}())
	for !r.c.Tip().SealsAt(r.c.Height() + 1) {
		r.mineN(t, 1)
	}
	parent := r.c.Tip()
	r.mineN(t, 1)
	good := r.c.BlockAt(r.c.Height())
	if len(good.TextBundles) == 0 {
		t.Fatal("seal block without bundles")
	}
	forged := *good
	forged.TextBundles = append([]types.TextBundle(nil), good.TextBundles...)
	forged.TextBundles[0].Root[0] ^= 1
	sh := forged.SigHash()
	forged.Sig = r.m.Key.Sign(sh[:])
	if err := r.c.Add(&forged); !errors.Is(err, chain.ErrInvalid) {
		t.Fatalf("forged bundle root: %v (parent %d)", err, parent.Height)
	}

	// A node that never saw the bodies waits for them.
	fresh, _ := chain.Open(p, "")
	empty, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fresh.Texts = empty
	var waited bool
	for h := uint64(1); h <= r.c.Height(); h++ {
		b := r.c.BlockAt(h)
		err := fresh.Add(b)
		var mt *chain.MissingTextsError
		if errors.As(err, &mt) {
			waited = true
			for _, x := range mt.Hashes {
				body, ok := r.st.Text(x)
				if !ok {
					t.Fatal("source lost a body")
				}
				if _, err := empty.PutText(body); err != nil {
					t.Fatal(err)
				}
			}
			err = fresh.Add(b)
		}
		if err != nil {
			t.Fatalf("height %d: %v", h, err)
		}
	}
	if !waited || fresh.BestHash() != r.c.BestHash() {
		t.Fatal("fresh node did not demand bodies or did not catch up")
	}

	// The one-month post leaves the state with its body's bucket, and
	// comments on it are refused from then on; longer retention stays.
	b := postOf(r.c.Tip(), short.ID()).TextBucket
	for r.c.Height() < r.c.Tip().BucketExpiry(b) {
		r.mineN(t, 1)
	}
	tip = r.c.Tip()
	if _, ok := tip.Posts.Get(short.ID()); ok {
		t.Fatal("expired post kept in the state")
	}
	if postOf(tip, keep.ID()) == nil || postOf(tip, later.ID()) == nil {
		t.Fatal("longer retention dropped early")
	}
	r.pool.Reset(tip)
	late := textPost("자유", "늦은 댓글", short.ID())
	late.MaxFee, late.Nonce = params.Coin, tip.Peek(k.Address()).Nonce
	late.Sign(k)
	if err := r.pool.Add(late); err == nil {
		t.Fatal("comment on an expired post accepted")
	}
	r.check(t)
}

// TestBurstSurcharge: an account sending faster than the free rate pays a
// growing base fee; storage duties do not count.
func TestBurstSurcharge(t *testing.T) {
	p := FastParams()
	r := newRig(t, p, 10)
	r.mineTo(t, 6)
	k := r.m.Key
	st := r.pool.PendingState()
	probe := &types.Tx{Kind: types.TxTransfer, To: k.Address(), From: k.Pub}
	sent := 0
	for {
		_, burst := r.pool.PendingState().FeeFactors(probe)
		if burst > 1 {
			break
		}
		r.send(t, k, &types.Tx{Kind: types.TxTransfer, To: k.Address(), MaxFee: params.Coin})
		sent++
		if sent > 60 {
			t.Fatal("no surcharge after 60 transactions in one block")
		}
	}
	// Free up to BurstFree, then ×2 from BurstFree+BurstStep points.
	if want := int(p.BurstFree + p.BurstStep - 1); sent != want {
		t.Fatalf("surcharge after %d transactions, want %d", sent, want)
	}
	if _, burst := st.FeeFactors(&types.Tx{Kind: types.TxProof, From: k.Pub}); burst != 1 {
		t.Fatal("storage proofs must not count")
	}
	// The score decays: after a few tau the account is back to normal.
	r.mineN(t, 1)
	r.mineN(t, 3*p.BurstTauBlocks)
	if _, burst := r.c.Tip().FeeFactors(probe); burst != 1 {
		t.Fatalf("surcharge %d did not decay", burst)
	}
}
