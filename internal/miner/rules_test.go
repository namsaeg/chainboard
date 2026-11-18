package miner

import (
	"crypto/rand"
	"testing"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

func (r *rig) send(t *testing.T, key *cryptox.Key, tx *types.Tx) {
	t.Helper()
	tip := r.c.Tip()
	tx.Nonce = r.pool.PendingState().Peek(key.Address()).Nonce
	tx.Sign(key)
	if err := r.pool.Add(tx); err != nil {
		t.Fatalf("%s rejected: %v", tx.Kind, err)
	}
	_ = tip
}

func (r *rig) mineN(t *testing.T, n uint64) { r.mineTo(t, r.c.Height()+n) }

func (r *rig) check(t *testing.T) {
	t.Helper()
	if err := r.c.Tip().CheckSupply(); err != nil {
		t.Fatal(err)
	}
}

func TestUploadExtendExpire(t *testing.T) {
	p := FastParams()
	r := newRig(t, p, 2)
	r.mineTo(t, 10)
	k := r.m.Key
	data := make([]byte, 300_000)
	rand.Read(data)
	root, err := r.st.PutData(data)
	if err != nil {
		t.Fatal(err)
	}
	r.pool.Reset(r.c.Tip())
	r.send(t, k, &types.Tx{Kind: types.TxUpload, DataRoot: root, Size: uint64(len(data)), Tier: 1, Media: types.MediaImage, MaxFee: 100 * params.Coin})
	r.mineN(t, 1)
	tip := r.c.Tip()
	info, ok := tip.Data[root]
	if !ok || info.Bucket == 0 {
		t.Fatalf("upload not placed in a term bucket: %+v", info)
	}
	firstBucket := info.Bucket
	if tip.Buckets[firstBucket].Escrow == 0 {
		t.Fatal("no escrow for term upload")
	}
	// Anyone can extend: a second account funded by the miner extends it to 1 year.
	other := cryptox.KeyFromSeed(append(make([]byte, 31), 99))
	r.pool.Reset(tip)
	r.send(t, k, &types.Tx{Kind: types.TxTransfer, To: other.Address(), Amount: 50 * params.Coin, MaxFee: params.Coin})
	r.mineN(t, 1)
	r.pool.Reset(r.c.Tip())
	r.send(t, other, &types.Tx{Kind: types.TxExtend, DataRoot: root, Tier: 12, MaxFee: 100 * params.Coin})
	r.mineN(t, 1)
	tip = r.c.Tip()
	if tip.Data[root].Bucket <= firstBucket {
		t.Fatal("extend did not move the data to a later bucket")
	}
	// Mine past the first bucket's expiry: the data survives in its new bucket.
	for r.c.Height() < tip.BucketExpiry(firstBucket)+1 {
		r.mineN(t, 1)
	}
	tip = r.c.Tip()
	if _, ok := tip.Buckets[firstBucket]; ok {
		t.Fatal("expired bucket still present")
	}
	if _, ok := tip.Data[root]; !ok {
		t.Fatal("extended data was dropped with the old bucket")
	}
	r.check(t)

	// A one-month upload that nobody extends expires.
	data2 := make([]byte, 10_000)
	rand.Read(data2)
	root2, _ := r.st.PutData(data2)
	r.pool.Reset(tip)
	r.send(t, k, &types.Tx{Kind: types.TxUpload, DataRoot: root2, Size: uint64(len(data2)), Tier: 1, Media: types.MediaImage, MaxFee: 100 * params.Coin})
	r.mineN(t, 1)
	b2 := r.c.Tip().Data[root2].Bucket
	for r.c.Height() < r.c.Tip().BucketExpiry(b2)+1 {
		r.mineN(t, 1)
	}
	if _, ok := r.c.Tip().Data[root2]; ok {
		t.Fatal("unextended data did not expire")
	}
	r.check(t)
}

func TestDuplicateSurchargeAndStake(t *testing.T) {
	p := FastParams()
	r := newRig(t, p, 3)
	r.mineTo(t, 10)
	k := r.m.Key
	tip := r.c.Tip()
	r.pool.Reset(tip)
	text := []byte("같은 글 반복")
	first := &types.Tx{Kind: types.TxPost, Lane: types.LaneFee, Board: "b", Text: text}
	first.Nonce = tip.Peek(k.Address()).Nonce
	first.Sign(k)
	var exact uint64
	for i := 0; i < 3; i++ { // MaxFee is part of the size; iterate to a fixed point
		size := uint64(first.EncodedSize())
		exact = tip.BaseFee*size + state.TextStorageFee(p, tip.Price, int(size))
		first.MaxFee = exact
		first.Sign(k)
	}
	if err := r.pool.Add(first); err != nil {
		t.Fatal(err)
	}
	second := &types.Tx{Kind: types.TxPost, Lane: types.LaneFee, Board: "b", Text: text, MaxFee: exact, Nonce: first.Nonce + 1}
	second.Sign(k)
	if err := r.pool.Add(second); err == nil {
		t.Fatal("repeated text was not surcharged")
	}

	// Stake must age before it backs quota, and unstaking takes the unbond delay.
	staker := cryptox.KeyFromSeed(append(make([]byte, 31), 77))
	r.send(t, k, &types.Tx{Kind: types.TxTransfer, To: staker.Address(), Amount: 20 * params.Coin, MaxFee: params.Coin})
	r.mineN(t, 1)
	r.pool.Reset(r.c.Tip())
	r.send(t, staker, &types.Tx{Kind: types.TxStake, Amount: 10 * params.Coin, MaxFee: params.Coin})
	r.mineN(t, 1)
	tip = r.c.Tip()
	acc := tip.Peek(staker.Address())
	if tip.EffectiveStake(&acc, tip.Height+1) != 0 {
		t.Fatal("fresh stake counted before aging")
	}
	r.mineN(t, p.StakeAgingBlocks+p.QuotaRegenBlocks)
	tip = r.c.Tip()
	acc = tip.Peek(staker.Address())
	if tip.QuotaAt(&acc, tip.Height+1) == 0 {
		t.Fatal("aged stake gives no quota")
	}
	r.pool.Reset(tip)
	r.send(t, staker, &types.Tx{Kind: types.TxPost, Lane: types.LaneFree, Board: "b", Text: []byte("무료 글")})
	r.send(t, staker, &types.Tx{Kind: types.TxUnstake, Amount: 10 * params.Coin, MaxFee: params.Coin})
	r.mineN(t, 1)
	before := r.c.Tip().Peek(staker.Address()).Balance
	r.mineN(t, p.UnbondBlocks+1)
	r.pool.Reset(r.c.Tip())
	// Touch the account so it settles.
	r.send(t, staker, &types.Tx{Kind: types.TxTransfer, To: k.Address(), Amount: 1, MaxFee: params.Coin})
	r.mineN(t, 1)
	after := r.c.Tip().Peek(staker.Address()).Balance
	if after < before+9*params.Coin {
		t.Fatalf("unbonded stake not returned: before %d after %d", before, after)
	}
	r.check(t)
}

func TestForfeitAndEpochProofs(t *testing.T) {
	p := FastParams()
	a := newRig(t, p, 4)
	a.mineTo(t, 5)
	// Miner B takes over the same chain; A registers partitions and proves each
	// epoch, while a third address that mined once and then stopped is forfeited.
	b := &Miner{Chain: a.c, Pool: a.pool, Store: a.st, Key: cryptox.KeyFromSeed(append(make([]byte, 31), 5)),
		PledgeBytes: 1 << 40, Logf: t.Logf, Now: a.m.Now}
	quitter := a.m.Key.Address()
	tip := a.c.Tip()
	if len(tip.Peek(quitter).Vesting) == 0 {
		t.Fatal("no vesting to forfeit")
	}
	a.pool.Reset(tip)
	a.send(t, a.m.Key, &types.Tx{Kind: types.TxTransfer, To: b.Key.Address(), Amount: params.Coin, MaxFee: params.Coin})
	a.mineN(t, 1)
	tip = a.c.Tip()
	a.m, b = b, a.m // b now mines; the old miner quits
	// b registers its partitions so it can prove even in epochs it does not win.
	a.pool.Reset(tip)
	a.m.Prepare(tip)
	a.send(t, a.m.Key, &types.Tx{Kind: types.TxRegister, Partitions: a.m.Assigned(), MaxFee: params.Coin})
	target := tip.Height + (p.ForfeitEpochs+2)*p.ProofEpoch
	pool0 := tip.RepairPool
	for a.c.Height() < target {
		a.mineN(t, 1)
	}
	tip = a.c.Tip()
	if len(tip.Peek(quitter).Vesting) != 0 {
		t.Fatal("quitter kept unvested income")
	}
	if tip.RepairPool <= pool0 {
		t.Fatal("forfeit did not reach the repair pool")
	}
	if len(tip.Peek(a.m.Key.Address()).Vesting) == 0 {
		t.Fatal("active miner lost its vesting")
	}
	// A storage node that registered partitions but does not mine keeps its
	// standing with an epoch proof transaction.
	prover := &Miner{Chain: a.c, Pool: a.pool, Store: a.st, Key: cryptox.KeyFromSeed(append(make([]byte, 31), 8)), PledgeBytes: 1 << 40}
	a.pool.Reset(tip)
	a.send(t, a.m.Key, &types.Tx{Kind: types.TxTransfer, To: prover.Key.Address(), Amount: params.Coin, MaxFee: params.Coin})
	a.mineN(t, 1)
	tip = a.c.Tip()
	prover.Prepare(tip)
	a.pool.Reset(tip)
	a.send(t, prover.Key, &types.Tx{Kind: types.TxRegister, Partitions: prover.Assigned(), MaxFee: params.Coin})
	a.mineN(t, 1)
	tip = a.c.Tip()
	a.pool.Reset(tip)
	tx, ok := prover.EpochProof(tip, a.pool.PendingState().Peek(prover.Key.Address()).Nonce)
	if !ok {
		t.Fatal("no epoch proof due for a registered storage node")
	}
	if err := a.pool.Add(tx); err != nil {
		t.Fatalf("epoch proof rejected: %v", err)
	}
	a.mineN(t, 1)
	if got := a.c.Tip().Peek(prover.Key.Address()).LastProof; got != a.c.Tip().Epoch(a.c.Height()) {
		t.Fatalf("epoch proof not recorded: last proof epoch %d", got)
	}
	a.check(t)
}

func TestReorg(t *testing.T) {
	p := FastParams()
	a := newRig(t, p, 6)
	b := newRig(t, p, 7)
	a.mineTo(t, 5)
	b.mineTo(t, 9)
	// Feed b's heavier chain into a.
	for h := uint64(1); h <= b.c.Height(); h++ {
		if err := a.c.Add(b.c.BlockAt(h)); err != nil && err.Error() != "chain: block already known" {
			t.Fatalf("height %d: %v", h, err)
		}
	}
	if a.c.BestHash() != b.c.BestHash() {
		t.Fatalf("no reorg: a at %d, b at %d", a.c.Height(), b.c.Height())
	}
	if a.c.Tip().Supply != b.c.Tip().Supply {
		t.Fatal("states differ after reorg")
	}
}

func TestChainPersistence(t *testing.T) {
	p := FastParams()
	dir := t.TempDir()
	r := newRig(t, p, 9)
	c, err := chainOpen(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	r.c = c
	r.m.Chain = c
	r.pool = newPool(c)
	r.m.Pool = r.pool
	r.mineTo(t, 8)
	c2, err := chainOpen(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	if c2.BestHash() != c.BestHash() {
		t.Fatalf("reopened chain at %d, want %d", c2.Height(), c.Height())
	}
}
