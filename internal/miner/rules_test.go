package miner

import (
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/namsaeg/chainboard/internal/chain"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/mempool"
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
	info, ok := tip.Data.Get(root)
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
	if dataOf(tip, root).Bucket <= firstBucket {
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
	if _, ok := tip.Data.Get(root); !ok {
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
	b2 := dataOf(r.c.Tip(), root2).Bucket
	for r.c.Height() < r.c.Tip().BucketExpiry(b2)+1 {
		r.mineN(t, 1)
	}
	if _, ok := r.c.Tip().Data.Get(root2); ok {
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
	text := "같은 광고 문구를 계속 반복해서 올립니다" // over DupMinBytes
	first := textPost("b", text, types.Hash{})
	first.Nonce = tip.Peek(k.Address()).Nonce
	first.Sign(k)
	var exact uint64
	for i := 0; i < 3; i++ { // MaxFee is part of the size; iterate to a fixed point
		exact = tip.BytesFee(first) + state.StorageFee(p, tip.Price, uint64(first.TextLen), p.TextReplicas, 1, false)
		first.MaxFee = exact
		first.Sign(k)
	}
	if err := r.pool.Add(first); err != nil {
		t.Fatal(err)
	}
	second := textPost("b", text, types.Hash{})
	second.MaxFee, second.Nonce = exact, first.Nonce+1
	second.Sign(k)
	if err := r.pool.Add(second); err == nil {
		t.Fatal("repeated text was not surcharged")
	}
	if dup, _ := r.pool.PendingState().FeeFactors(second); dup != 2 {
		t.Fatalf("repeat factor %d, want 2", dup)
	}
	short := textPost("b", "ㅋㅋㅋ", types.Hash{})
	short.From = k.Pub
	if dup, _ := r.pool.PendingState().FeeFactors(short); dup != 1 {
		t.Fatalf("short text counted as a repeat (factor %d)", dup)
	}

	// Stake must age before it can bond an anonymous-lane membership, and
	// unstaking takes the unbond delay.
	staker := cryptox.KeyFromSeed(append(make([]byte, 31), 77))
	r.send(t, k, &types.Tx{Kind: types.TxTransfer, To: staker.Address(), Amount: 20 * params.Coin, MaxFee: params.Coin})
	r.mineN(t, 1)
	r.pool.Reset(r.c.Tip())
	r.send(t, staker, &types.Tx{Kind: types.TxStake, Amount: 10 * params.Coin, MaxFee: params.Coin})
	r.mineN(t, 1)
	r.pool.Reset(r.c.Tip())
	join := &types.Tx{Kind: types.TxRLNJoin, Amount: 2 * params.Coin, Commitment: types.Hash{31: 1}, Limit: 2, MaxFee: params.Coin,
		Nonce: r.pool.PendingState().Peek(staker.Address()).Nonce}
	join.Sign(staker)
	if err := r.pool.Add(join); err == nil {
		t.Fatal("fresh stake bonded before aging")
	}
	r.mineN(t, p.StakeAgingBlocks)
	r.pool.Reset(r.c.Tip())
	bond := r.c.Tip().BondFor(2) // follows the storage price
	r.send(t, staker, &types.Tx{Kind: types.TxRLNJoin, Amount: bond, Commitment: types.Hash{31: 1}, Limit: 2, MaxFee: params.Coin})
	r.send(t, staker, &types.Tx{Kind: types.TxUnstake, Amount: 10*params.Coin - bond, MaxFee: params.Coin})
	r.mineN(t, 1)
	before := r.c.Tip().Peek(staker.Address()).Balance
	r.mineN(t, p.UnbondBlocks+1)
	r.pool.Reset(r.c.Tip())
	// Touch the account so it settles.
	r.send(t, staker, &types.Tx{Kind: types.TxTransfer, To: k.Address(), Amount: 1, MaxFee: params.Coin})
	r.mineN(t, 1)
	after := r.c.Tip().Peek(staker.Address()).Balance
	if m := memberOf(r.c.Tip(), types.Hash{31: 1}); m == nil || m.Bond != bond {
		t.Fatal("membership bond not recorded")
	}
	if after < before+10*params.Coin-bond-params.Coin {
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
	a.m = b // b now mines; the old miner quits
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
	// A registration is challenged from the next epoch on.
	if _, ok := prover.EpochProof(tip, 0); ok {
		t.Fatal("epoch proof due in the epoch of registration")
	}
	for regEpoch := tip.Epoch(tip.Height); a.c.Tip().Epoch(a.c.Height()+1) == regEpoch; {
		a.mineN(t, 1)
	}
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
	c.MemoryDepth = 3 // older bodies are read back from the database
	r.c = c
	r.m.Chain = c
	r.pool = newPool(c)
	r.m.Pool = r.pool
	r.mineTo(t, 20)
	first := c.BlockAt(1)
	if first == nil || first.Height != 1 || c.Block(first.Hash()) == nil {
		t.Fatal("evicted block not read back")
	}
	if got := c.After(c.Locator()[len(c.Locator())-1:], 50); len(got) != 20 || got[0].Hash() != first.Hash() {
		t.Fatalf("serving blocks to a syncing peer: %d", len(got))
	}
	want := c.BestHash()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := chainOpen(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	c2.MemoryDepth = 3
	r.c = c2 // the version check replays from this one
	if c2.BestHash() != want || c2.Height() != 20 {
		t.Fatalf("reopened chain at %d, want 20", c2.Height())
	}
}

// A block must carry the root of the state it produces.
func TestStateRootChecked(t *testing.T) {
	p := FastParams()
	r := newRig(t, p, 4)
	r.mineTo(t, 3)
	c, err := chain.Open(p, "")
	if err != nil {
		t.Fatal(err)
	}
	for h := uint64(1); h < 3; h++ {
		if err := c.Add(r.c.BlockAt(h)); err != nil {
			t.Fatal(err)
		}
	}
	b := *r.c.BlockAt(3)
	b.StateRoot[0] ^= 1
	sh := b.SigHash()
	b.Sig = r.m.Key.Sign(sh[:])
	if err := c.Add(&b); err == nil || !strings.Contains(err.Error(), "state root") {
		t.Fatalf("wrong state root accepted: %v", err)
	}
	if err := c.Add(r.c.BlockAt(3)); err != nil {
		t.Fatal(err)
	}
	if c.Tip().Root() != r.c.Tip().Root() {
		t.Fatal("same blocks, different roots")
	}
}

// History is accepted on a VDF sample and checked in full in the
// background: a block with a forged step is then removed with its
// descendants, and the tip moves back.
func TestSampledVDFCaughtLater(t *testing.T) {
	p := FastParams()
	p.GenesisTarget = new(big.Int).Lsh(big.NewInt(1), 247) // a few steps per block
	r := newRig(t, p, 6)
	r.mineTo(t, 2)
	at := uint64(0) // a block with at least two VDF steps
	for at == 0 {
		r.mineN(t, 1)
		if h := r.c.Height(); len(r.c.BlockAt(h).VDFOutputs) >= 2 {
			at = h
		}
		if r.c.Height() > 200 {
			t.Fatal("no block with two VDF steps")
		}
	}
	c, err := chain.Open(p, "")
	if err != nil {
		t.Fatal(err)
	}
	c.SampleSteps = -1       // accept on no sample at all
	c.FullCheckSec = 1 << 40 // and recheck every block in full
	for h := uint64(1); h < at; h++ {
		if err := c.Add(r.c.BlockAt(h)); err != nil {
			t.Fatal(err)
		}
	}
	forged := *r.c.BlockAt(at)
	forged.VDFOutputs = append([]types.Hash(nil), forged.VDFOutputs...)
	forged.VDFOutputs[0][0] ^= 1
	sh := forged.SigHash()
	forged.Sig = r.m.Key.Sign(sh[:])
	if err := c.Add(&forged); err != nil {
		t.Fatalf("history block not accepted on a sample: %v", err)
	}
	if c.Height() != at {
		t.Fatal("forged block not at the tip")
	}
	if n := c.VerifyPending(); n == 0 {
		t.Fatal("nothing checked in the background")
	}
	if c.Height() != at-1 || c.Block(forged.Hash()) != nil {
		t.Fatalf("forged block kept: height %d", c.Height())
	}
	if err := c.Add(&forged); err == nil {
		t.Fatal("removed block accepted again")
	}
	// The honest block is fine, and passes its full check.
	if err := c.Add(r.c.BlockAt(at)); err != nil {
		t.Fatal(err)
	}
	c.VerifyPending()
	if c.Height() != at {
		t.Fatal("honest block removed")
	}
}

// TestOrphans: a transaction that overtakes its sender's earlier one waits
// outside the pool and enters (and is announced) once the gap fills.
func TestOrphans(t *testing.T) {
	r := newRig(t, FastParams(), 4)
	r.mineTo(t, 5)
	k := r.m.Key
	r.pool.Reset(r.c.Tip())
	var promoted []*types.Tx
	r.pool.OnPromote = func(tx *types.Tx) { promoted = append(promoted, tx) }
	base := r.pool.PendingState().Peek(k.Address()).Nonce
	var txs []*types.Tx
	for i := uint64(0); i < 3; i++ {
		tx := &types.Tx{Kind: types.TxTransfer, To: types.Address{1}, Amount: 1, MaxFee: params.Coin, Nonce: base + i}
		tx.Sign(k)
		txs = append(txs, tx)
	}
	for _, tx := range []*types.Tx{txs[2], txs[1]} {
		if err := r.pool.Add(tx); !errors.Is(err, mempool.ErrOrphan) {
			t.Fatalf("nonce %d ahead: %v", tx.Nonce, err)
		}
	}
	if r.pool.Has(txs[1].ID()) {
		t.Fatal("orphan in the pool")
	}
	if err := r.pool.Add(txs[0]); err != nil {
		t.Fatal(err)
	}
	if !r.pool.Has(txs[1].ID()) || !r.pool.Has(txs[2].ID()) || len(promoted) != 2 {
		t.Fatalf("orphans not promoted (%d)", len(promoted))
	}
	r.mineN(t, 1)
	if r.c.Tip().Peek(k.Address()).Nonce != base+3 {
		t.Fatal("promoted transactions not mined")
	}
}

// TestTips: a tip moves coins to the named receiver, at least the minimum,
// never to the sender itself.
func TestTips(t *testing.T) {
	r := newRig(t, FastParams(), 4)
	r.mineTo(t, 5)
	k := r.m.Key
	r.pool.Reset(r.c.Tip())
	to := types.Address{9}
	min := r.c.Tip().MinPledge()
	ref := types.LikeRef{Height: 3, Prefix: [4]byte{1, 2, 3, 4}}
	small := &types.Tx{Kind: types.TxLikes, Tips: []types.Tip{{Ref: ref, To: to, Amount: min - 1}}, MaxFee: params.Coin,
		Nonce: r.pool.PendingState().Peek(k.Address()).Nonce}
	small.Sign(k)
	if r.pool.Add(small) == nil {
		t.Fatal("tip below the minimum accepted")
	}
	self := &types.Tx{Kind: types.TxLikes, Tips: []types.Tip{{Ref: ref, To: k.Address(), Amount: min}}, MaxFee: params.Coin,
		Nonce: small.Nonce}
	self.Sign(k)
	if r.pool.Add(self) == nil {
		t.Fatal("tip to oneself accepted")
	}
	r.send(t, k, &types.Tx{Kind: types.TxLikes, Likes: []types.LikeRef{{Height: 2}}, Tips: []types.Tip{{Ref: ref, To: to, Amount: 5 * min}}, MaxFee: params.Coin})
	r.mineN(t, 1)
	if got := r.c.Tip().Peek(to).Balance; got != 5*min {
		t.Fatalf("receiver has %d, want %d", got, 5*min)
	}
	r.check(t)
}
