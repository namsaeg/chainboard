package state

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	mrand "math/rand/v2"
	"runtime"
	"sort"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/pack"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/types"
	"github.com/namsaeg/chainboard/internal/vdf"
)

// MaxProofTxs bounds storage-proof transactions per block.
const MaxProofTxs = 64

// BlockCtx accumulates per-block totals while transactions are applied.
type BlockCtx struct {
	H           uint64 // height of the block being built
	FreeUsed    uint64
	FeeUsed     uint64
	MediaUsed   uint64
	FreeStorage uint64 // free-lane text storage, paid from issuance
	Tips        uint64
	Burn        uint64
	Proofs      int
	Slashes     int
}

// NewCtx starts a context for the next block.
func (s *State) NewCtx() *BlockCtx { return &BlockCtx{H: s.Height + 1} }

// settle releases matured stake and unbonding, and claims vested income when
// the account is an active storage prover.
func (s *State) settle(a *Account, h uint64) {
	if a.StakePending > 0 && h >= a.PendingSince+s.P.StakeAgingBlocks {
		a.StakeMature += a.StakePending
		a.StakePending = 0
	}
	if len(a.Unbonding) > 0 {
		keep := a.Unbonding[:0]
		for _, u := range a.Unbonding {
			if h >= u.Release {
				a.Balance += u.Amount
			} else {
				keep = append(keep, u)
			}
		}
		a.Unbonding = keep
	}
	if len(a.Vesting) > 0 && s.Epoch(h) <= a.LastProof+1 {
		keep := a.Vesting[:0]
		for _, t := range a.Vesting {
			v := vestedAt(t, h)
			a.Balance += v - t.Claimed
			t.Claimed = v
			if t.Claimed < t.Amount {
				keep = append(keep, t)
			}
		}
		a.Vesting = keep
	}
}

// Settle applies to an account what height h releases (matured stake,
// ended unbonding, vested rewards), as its next transaction would.
func (s *State) Settle(a Address, h uint64) {
	acc := s.Account(a)
	s.settle(acc, h)
}

// dupMultiplier is the repeat surcharge of a post: 2^n when the same text
// (by hash, from anyone) already appeared n times in the window. Texts
// shorter than DupMinBytes are not counted: short replies are common.
func (s *State) dupMultiplier(t *types.Tx, h uint64) (Hash, uint64) {
	if uint64(t.TextLen) < s.P.DupMinBytes {
		return Hash{}, 1
	}
	k := cryptox.Tagged("cb/dup", t.TextHash[:])
	return k, DupMultiplier(s.dupAt(k, h).Count)
}

// dupAt is a text's repeat entry as seen at height h. Entries live in the
// generation of their last height (h / DupWindowBlocks); anything older
// than the previous generation has expired by construction.
func (s *State) dupAt(k Hash, h uint64) dupEntry {
	g := h / s.P.DupWindowBlocks
	var e dupEntry
	switch g {
	case s.dupGen:
		var ok bool
		if e, ok = s.dupCur.Get(k); !ok {
			e, _ = s.dupPrev.Get(k)
		}
	case s.dupGen + 1:
		e, _ = s.dupCur.Get(k)
	}
	if h > e.Last+s.P.DupWindowBlocks {
		e.Count = 0
	}
	return e
}

func (s *State) noteDup(k Hash, h uint64) {
	if k.IsZero() {
		return
	}
	e := s.dupAt(k, h)
	e.Count++
	e.Last = h
	if g := h / s.P.DupWindowBlocks; g > s.dupGen {
		empty := newDupMap()
		if g == s.dupGen+1 {
			s.dupPrev, s.dupCur = s.dupCur, empty
		} else {
			s.dupPrev, s.dupCur = empty, empty
		}
		s.dupGen = g
	}
	s.dupCur.Set(k, e)
}

// TxCost is what a fee-lane transaction pays (base fee, storage, tip).
type TxCost struct {
	Base, Storage, Tip uint64
	Dest               uint32 // bucket that receives the storage fee (0 = endowment)
}

func (c TxCost) Total() uint64 { return c.Base + c.Storage + c.Tip }

// checkParent validates the post a comment answers.
func (s *State) checkParent(t *types.Tx) error {
	if t.Parent.IsZero() {
		return nil
	}
	parent, ok := s.Posts.Get(t.Parent)
	if !ok {
		return errors.New("comment on unknown post")
	}
	if !parent.Parent.IsZero() {
		return errors.New("cannot reply to a comment")
	}
	return nil
}

// notePost records a post (signed or anonymous) whose life ends with
// bucket: where its body is kept, or the one-month bucket without a body.
func (s *State) notePost(t *types.Tx, h uint64, dupKey Hash, bucket uint32) {
	s.noteDup(dupKey, h)
	id := t.ID()
	s.Posts.Set(id, &PostInfo{Parent: t.Parent, TextHash: t.TextHash, TextLen: t.TextLen, TextTier: t.Tier, TextBucket: bucket})
	if bucket != 0 {
		s.bucket(bucket).Posts.Append(id)
	}
}

// ErrFutureNonce: an earlier transaction of the sender has not applied yet
// (it may still be on its way).
var ErrFutureNonce = errors.New("nonce ahead of the account")

// ApplyTx validates and applies one transaction. It is atomic: on error
// neither the state nor ctx is modified.
func (s *State) ApplyTx(t *types.Tx, ctx *BlockCtx) error {
	if err := t.CheckStateless(); err != nil {
		return err
	}
	switch t.Kind {
	case types.TxAnonPost:
		return s.applyAnon(t, ctx)
	case types.TxAnonLikes:
		return s.applyAnonLikes(t, ctx)
	case types.TxRLNSlash:
		return s.applySlash(t, ctx)
	}
	h := ctx.H
	from := t.Sender()
	var acc *Account
	if a, ok := s.Accounts.Get(from); ok {
		acc = a.clone()
	} else {
		acc = &Account{}
	}
	s.settle(acc, h)
	if t.Nonce != acc.Nonce {
		if t.Nonce > acc.Nonce {
			return fmt.Errorf("%w: have %d want %d", ErrFutureNonce, t.Nonce, acc.Nonce)
		}
		return fmt.Errorf("bad nonce: have %d want %d", t.Nonce, acc.Nonce)
	}
	size := uint64(t.LaneSize())
	next := *ctx

	// Fee lane accounting.
	var dupKey Hash
	dupMul := uint64(1)
	if t.Kind == types.TxPost {
		dupKey, dupMul = s.dupMultiplier(t, h)
	}
	burstMul, burstScore := uint64(1), acc.Burst
	if countsForBurst(t.Kind) {
		burstScore = min(s.BurstScore(acc, h)+burstUnit, 1_000_000*burstUnit)
		burstMul = s.BurstMultiplier(burstScore)
	}
	cost := TxCost{Tip: t.Tip}
	var pledged uint64 // moved into funding escrows or tipped
	var fund *Campaign // funded upload or proof: its fees come from the escrow
	var fundBase, fundHeld uint64
	var fundTier uint16
	if t.Kind == types.TxProof {
		if next.Proofs >= MaxProofTxs {
			return errors.New("too many proofs in block")
		}
		next.Proofs++
	} else {
		if next.FeeUsed+size > s.P.TextCapFee() {
			return errors.New("fee lane full")
		}
		next.FeeUsed += size
	}
	cost.Base = satMul(satMul(s.BytesFee(t), dupMul), burstMul)
	if t.Kind == types.TxPost && t.TextLen > 0 {
		cost.Storage, cost.Dest = s.textFee(uint64(t.TextLen), t.Tier, h)
	}

	// Kind-specific checks.
	switch t.Kind {
	case types.TxTransfer:
		if t.To.IsZero() {
			return errors.New("transfer to zero address")
		}
	case types.TxStake:
		if t.Amount == 0 {
			return errors.New("zero stake")
		}
	case types.TxUnstake:
		if t.Amount == 0 || t.Amount > acc.StakeMature+acc.StakePending {
			return errors.New("unstake exceeds stake")
		}
	case types.TxPost:
		if err := s.checkParent(t); err != nil {
			return err
		}
		for _, a := range t.Attach {
			if !s.Data.Has(a) {
				return errors.New("attachment not stored")
			}
		}
	case types.TxLikes:
		var err error
		if pledged, err = s.checkPledges(t, h); err != nil {
			return err
		}
	case types.TxFundUpload, types.TxFundProof:
		c, ok := s.campaign(t.Target)
		if !ok {
			return errors.New("unknown funding request")
		}
		if t.Kind == types.TxFundProof && s.Data.Has(c.DataRoot) {
			return errors.New("that data is already stored")
		}
		// Only the creator chooses when to upload (and so the tier), so no
		// one can force an early, short-tier upload; anyone holding the
		// data may send the proof.
		if t.Kind == types.TxFundUpload && from != c.Creator {
			return errors.New("only the request's creator sends its upload")
		}
		base, held, tier, err := s.fundCost(t, c, h, &next)
		if err != nil {
			return err
		}
		fund, fundBase, fundHeld, fundTier = c, base, held, tier
		cost.Base = 0 // paid from the escrow, below
	case types.TxExtend:
		var fee uint64
		var dest uint32
		var err error
		if t.Target.IsZero() {
			fee, dest, err = s.dataFee(t, h, &next)
		} else {
			fee, dest, err = s.postExtendFee(t, h)
		}
		if err != nil {
			return err
		}
		cost.Storage, cost.Dest = fee, dest
	case types.TxUpload:
		fee, dest, err := s.dataFee(t, h, &next)
		if err != nil {
			return err
		}
		cost.Storage, cost.Dest = fee, dest
	case types.TxRegister:
		if len(t.Partitions) == 0 {
			return errors.New("no partitions")
		}
		for _, p := range t.Partitions {
			if _, _, _, ok := s.PartitionRange(p); !ok {
				return fmt.Errorf("unknown partition %x", p)
			}
		}
	case types.TxProof:
		if err := s.checkEpochProof(from, acc, t.Proof, h); err != nil {
			return err
		}
	case types.TxRLNJoin:
		if err := s.checkJoin(t, acc); err != nil {
			return err
		}
	case types.TxRLNLeave:
		if err := s.checkLeave(t, from); err != nil {
			return err
		}
	}

	// Payment checks.
	total := cost.Total()
	if total > t.MaxFee {
		return fmt.Errorf("fee %d exceeds max fee %d", total, t.MaxFee)
	}
	need := total + pledged
	if t.Kind == types.TxTransfer || t.Kind == types.TxStake {
		need += t.Amount
	}
	if acc.Balance < need {
		return fmt.Errorf("insufficient balance: have %d need %d", acc.Balance, need)
	}

	// Everything checked: commit.
	acc.Balance -= total
	next.Burn += cost.Base
	if fund != nil {
		next.Burn += fundBase
	}
	next.Tips += cost.Tip
	s.fund(cost.Dest, cost.Storage)
	acc.Nonce++
	if countsForBurst(t.Kind) {
		acc.Burst, acc.BurstAt = burstScore, h
	}
	switch t.Kind {
	case types.TxTransfer:
		acc.Balance -= t.Amount
		if t.To == from {
			acc.Balance += t.Amount
		} else {
			s.Account(t.To).Balance += t.Amount
		}
	case types.TxStake:
		acc.Balance -= t.Amount
		if acc.StakePending > 0 && h >= acc.PendingSince+s.P.StakeAgingBlocks {
			acc.StakeMature += acc.StakePending
			acc.StakePending = 0
		}
		acc.StakePending += t.Amount
		acc.PendingSince = h
	case types.TxUnstake:
		amt := t.Amount
		if amt <= acc.StakeMature {
			acc.StakeMature -= amt
		} else {
			acc.StakePending -= amt - acc.StakeMature
			acc.StakeMature = 0
		}
		acc.Unbonding = append(acc.Unbonding, Unbond{Amount: t.Amount, Release: h + s.P.UnbondBlocks})
	case types.TxPost:
		life := s.BucketFor(h, 1, false)
		if t.TextLen > 0 {
			life = cost.Dest
		}
		s.notePost(t, h, dupKey, life)
		if t.TextLen > 0 {
			s.queueText(t.ID(), t.TextHash, t.TextLen, cost.Dest)
		}
	case types.TxUpload:
		s.placeData(t, h)
	case types.TxExtend:
		if t.Target.IsZero() {
			s.placeData(t, h)
		} else {
			s.applyPostExtend(t, cost.Dest)
		}
	case types.TxLikes:
		acc.Balance -= pledged
		s.applyPledges(t, from, h)
	case types.TxFundUpload:
		s.applyFundUpload(t.Target, fund, fundBase, fundHeld, fundTier, h)
	case types.TxFundProof:
		s.applyFundProof(t.Target, fund, fundBase, &next)
	case types.TxRegister:
		if e := s.Epoch(h); acc.PartsEpoch != e+1 {
			acc.ProveParts = acc.Partitions
			acc.PartsEpoch = e + 1
		}
		acc.Partitions = append([]uint64(nil), t.Partitions...)
	case types.TxProof:
		acc.LastProof = s.Epoch(h)
		acc.ProofsInEpoch = s.Epoch(h) + 1
	case types.TxRLNJoin:
		s.applyJoin(t, from, acc, h)
	case types.TxRLNLeave:
		s.applyLeave(t, h)
	}
	s.Accounts.Set(from, acc)
	*ctx = next
	return nil
}

func (s *State) remainingMonths(info *DataInfo, h uint64) uint64 {
	exp := s.BucketExpiry(info.Bucket)
	if exp <= h {
		return 0
	}
	return (exp - h) / s.P.BlocksPerMonth
}

// dataFee prices an upload or extend and returns the bucket it pays into.
func (s *State) dataFee(t *types.Tx, h uint64, ctx *BlockCtx) (uint64, uint32, error) {
	months, perm, _ := params.TierMonths(t.Tier)
	dest := s.BucketFor(h, months, perm)
	info, exists := s.Data.Get(t.DataRoot)
	size := t.Size
	if t.Kind == types.TxUpload {
		if size > s.FileCap() {
			return 0, 0, fmt.Errorf("file larger than cap %d", s.FileCap())
		}
		if !exists {
			if ctx.MediaUsed+size > s.MediaCapPerBlock() {
				return 0, 0, errors.New("media cap for this block reached")
			}
			ctx.MediaUsed += size
			return StorageFee(s.P, s.Price, size, s.P.MediaReplicas, months, perm), dest, nil
		}
		if size != info.Size {
			return 0, 0, errors.New("data root already stored with another size")
		}
		// Same data again: only a longer tier makes sense, priced as an extend.
	} else if !exists {
		return 0, 0, errors.New("extend of unknown data")
	}
	if info.Tier == params.TierPermanent {
		return 0, 0, errors.New("data is already stored permanently; nothing to extend")
	}
	if !perm && dest <= info.Bucket {
		return 0, 0, fmt.Errorf("data is already stored until height %d, which this tier would not outlast; choose a longer tier to extend it",
			s.BucketExpiry(info.Bucket))
	}
	rem := s.remainingMonths(info, h)
	full := StorageFee(s.P, s.Price, info.Size, s.P.MediaReplicas, months, perm)
	credit := StorageFee(s.P, s.Price, info.Size, s.P.MediaReplicas, rem, false)
	if credit > full {
		credit = full
	}
	return full - credit, dest, nil
}

// placeData writes an upload or an extend into the weave.
func (s *State) placeData(t *types.Tx, h uint64) {
	months, perm, _ := params.TierMonths(t.Tier)
	s.place(t.DataRoot, t.Size, t.Media, t.Tier, s.BucketFor(h, months, perm))
}

// place puts data into a bucket, moving it out of the bucket it was in.
func (s *State) place(root Hash, size uint64, media uint8, tier uint16, destID uint32) {
	info, exists := s.Data.Get(root)
	if exists {
		size, media = info.Size, info.Media
		old := s.bucket(info.Bucket)
		r := old.Records.At(info.Record)
		r.Moved = true
		old.Records.Set(info.Record, r)
		old.Bytes -= size
		if info.Bucket == 0 {
			s.PermBytes -= size
		} else {
			s.TermBytes -= size
		}
	}
	b := s.bucket(destID)
	n := chunk.Count(size)
	b.Records.Append(Record{DataRoot: root, Size: size, Start: b.Len, Chunks: n})
	b.Len += n
	b.Bytes += size
	if destID == 0 {
		s.PermBytes += size
	} else {
		s.TermBytes += size
	}
	s.Data.Set(root, &DataInfo{Bucket: destID, Size: size, Media: media, Tier: tier, Record: b.Records.Len() - 1})
}

// Challenge derives the chunk a challenge points at. ok is false when the
// partition or position is not challengeable.
type Challenge struct {
	H0       Hash
	Bucket   uint32
	Position uint64
	Sub      int
	Rec      *Record
	ChunkIdx uint64
	Weight   uint64 // permille
}

// RecallRange is the run of chunks one partition unlocks for one miner at
// one VDF step. Every sub-chunk in it is a ticket.
type RecallRange struct {
	H0         Hash
	Bucket     uint32
	Start, End uint64 // positions [Start, End)
	Weight     uint64 // permille
}

// WeaveChunks is the number of chunk positions in all buckets.
func (s *State) WeaveChunks() uint64 {
	var n uint64
	for _, b := range s.Buckets {
		n += b.Len
	}
	return n
}

// Span is the space a partition's range offset is drawn from: its own
// length, raised to 1/SpanDivisor of the weave and capped at the nominal
// partition size. A partition shorter than its span is hit only in
// proportion to its length, so many small buckets do not multiply the reads
// a miner must do per step; while the weave is small, every partition is hit
// every step and mining can start from the genesis data alone.
func (s *State) Span(length, weave uint64) uint64 {
	floor := (weave + s.P.SpanDivisor - 1) / s.P.SpanDivisor
	return min(s.P.PartitionChunks, max(length, floor))
}

// RangeFor computes the recall range a VDF output unlocks in a partition
// for a miner. weave is WeaveChunks() of this state. ok is false when the
// draw falls past the partition's data or the bucket carries no weight.
func (s *State) RangeFor(out Hash, partition uint64, miner Address, h, weave uint64) (RecallRange, bool) {
	bucket, start, length, ok := s.PartitionRange(partition)
	if !ok {
		return RecallRange{}, false
	}
	w := TermWeight(s.P, bucket == 0, s.BucketExpiry(bucket), h)
	if w == 0 {
		return RecallRange{}, false
	}
	h0 := cryptox.Tagged("cb/chal", out[:], cryptox.U64(partition), miner[:])
	off := binary.BigEndian.Uint64(h0[:8]) % s.Span(length, weave)
	if off >= length {
		return RecallRange{}, false
	}
	end := min(off+s.P.RecallChunks, length)
	return RecallRange{H0: h0, Bucket: bucket, Start: start + off, End: start + end, Weight: w}, true
}

// Ticket checks that sub-chunk sub of position pos is a ticket of r.
func (s *State) Ticket(r RecallRange, pos uint64, sub int) (Challenge, bool) {
	if pos < r.Start || pos >= r.End {
		return Challenge{}, false
	}
	rec, ci, ok := s.Locate(r.Bucket, pos)
	if !ok || rec.Moved || sub < 0 || sub >= chunk.SubCount(rec.Size, ci) {
		return Challenge{}, false
	}
	return Challenge{H0: r.H0, Bucket: r.Bucket, Position: pos, Sub: sub, Rec: rec, ChunkIdx: ci, Weight: r.Weight}, true
}

func (s *State) pointAt(h0 Hash, bucket uint32, start, length, h uint64) (Challenge, bool) {
	pos := start + new(big.Int).SetBytes(h0[:8]).Uint64()%length
	rec, ci, ok := s.Locate(bucket, pos)
	if !ok || rec.Moved {
		return Challenge{}, false
	}
	n := chunk.SubCount(rec.Size, ci)
	if n == 0 {
		return Challenge{}, false
	}
	exp := s.BucketExpiry(bucket)
	w := TermWeight(s.P, bucket == 0, exp, h)
	if w == 0 {
		return Challenge{}, false
	}
	return Challenge{H0: h0, Bucket: bucket, Position: pos, Sub: int(h0[8]) % n, Rec: rec, ChunkIdx: ci, Weight: w}, true
}

// VerifyPacked checks a packed sub-chunk against the weave for a miner.
func (s *State) VerifyPacked(c Challenge, miner Address, packed []byte, path chunk.Proof) bool {
	if len(packed) != params.SubChunkSize {
		return false
	}
	seed := pack.Seed(s.P, c.Bucket, c.Position, miner)
	raw := pack.UnpackSub(seed, c.Sub, packed, chunk.SubLen(c.Rec.Size, c.ChunkIdx, c.Sub))
	return chunk.VerifySub(c.Rec.DataRoot, c.Rec.Size, c.ChunkIdx, c.Sub, raw, path)
}

// SolutionHash is a ticket's draw, compared against the weighted target.
func SolutionHash(c Challenge, packed []byte) Hash {
	return cryptox.Tagged("cb/sol", c.H0[:], cryptox.U64(c.Position), []byte{byte(c.Sub)}, packed)
}

// MeetsTarget reports whether sol < target × weight / 1000.
func MeetsTarget(sol Hash, target *big.Int, weight uint64) bool {
	t := new(big.Int).Mul(target, new(big.Int).SetUint64(weight))
	t.Div(t, big.NewInt(1000))
	return new(big.Int).SetBytes(sol[:]).Cmp(t) < 0
}

// epochChallenge is what a storage node must prove with a proof transaction.
// The challenge is fixed for the whole epoch, so a proof can travel the stem
// and wait in pools: the seed, the challenged partitions (registrations count
// from the next epoch) and the partition lengths are all taken as of the
// epoch's start. A point that lands on extended-away or expired data is
// redrawn, a few times, so the node is not blamed for it.
func (s *State) epochChallenge(a Address, acc *Account) (Challenge, uint64, bool) {
	h := s.Height + 1
	e := s.Epoch(h)
	parts := acc.Partitions
	if acc.PartsEpoch == e+1 {
		parts = acc.ProveParts
	}
	if len(parts) == 0 {
		return Challenge{}, 0, false
	}
	seed := cryptox.Tagged("cb/epochseed", cryptox.U64(e), s.EpochSeed[:])
	for try := uint64(0); try < 8; try++ {
		c0 := cryptox.Tagged("cb/epochchal", seed[:], a[:], cryptox.U64(try))
		part := parts[new(big.Int).SetBytes(c0[8:16]).Uint64()%uint64(len(parts))]
		bucket, idx := SplitPartition(part)
		b, ok := s.Buckets[bucket]
		if !ok {
			continue
		}
		start := idx * s.P.PartitionChunks
		if start >= b.EpochLen {
			continue
		}
		length := min(s.P.PartitionChunks, b.EpochLen-start)
		if c, ok := s.pointAt(c0, bucket, start, length, h); ok {
			return c, part, true
		}
	}
	return Challenge{}, 0, false
}

// EpochChallenge tells a storage node what to prove this epoch.
func (s *State) EpochChallenge(a Address) (Challenge, uint64, bool) {
	acc, ok := s.Accounts.Get(a)
	if !ok {
		return Challenge{}, 0, false
	}
	return s.epochChallenge(a, acc)
}

func (s *State) checkEpochProof(a Address, acc *Account, p *types.StorageProof, h uint64) error {
	if p.Epoch != s.Epoch(h) {
		return errors.New("proof for another epoch")
	}
	if acc.ProofsInEpoch == s.Epoch(h)+1 {
		return errors.New("already proved this epoch")
	}
	c, part, ok := s.epochChallenge(a, acc)
	if !ok {
		return errors.New("no challenge (register partitions first)")
	}
	if p.Partition != part || p.Position != c.Position || int(p.Sub) != c.Sub {
		return errors.New("proof answers the wrong challenge")
	}
	if !s.VerifyPacked(c, a, p.PackedSub, p.Path) {
		return errors.New("storage proof does not verify")
	}
	return nil
}

// ExpectedTarget is the difficulty the next block must use: the average
// target of the last RetargetWindow blocks, scaled by how long they actually
// took versus BlockTime (clamped to a factor of 3 either way). Averaging over
// the window keeps per-block changes small.
func (s *State) ExpectedTarget() *big.Int {
	last := new(big.Int).SetBytes(s.Recent[len(s.Recent)-1].Target[:])
	w := s.P.RetargetWindow
	if len(s.Recent) <= w {
		return last
	}
	sum := new(big.Int)
	for _, m := range s.Recent[len(s.Recent)-w:] {
		sum.Add(sum, new(big.Int).SetBytes(m.Target[:]))
	}
	avg := sum.Div(sum, big.NewInt(int64(w)))
	first := s.Recent[len(s.Recent)-1-w]
	actual := s.Recent[len(s.Recent)-1].Time - first.Time
	expected := int64(w) * int64(s.P.BlockTime.Seconds())
	if actual < expected/3 {
		actual = expected / 3
	}
	if actual > expected*3 {
		actual = expected * 3
	}
	t := avg.Mul(avg, big.NewInt(actual))
	t.Div(t, big.NewInt(expected))
	if t.Cmp(s.P.MaxTarget) > 0 {
		t.Set(s.P.MaxTarget)
	}
	if t.Sign() <= 0 {
		t.SetInt64(1)
	}
	return t
}

// MedianTime is the median timestamp of the last 11 blocks.
func (s *State) MedianTime() int64 {
	n := len(s.Recent)
	k := 11
	if n < k {
		k = n
	}
	ts := make([]int64, k)
	for i := 0; i < k; i++ {
		ts[i] = s.Recent[n-1-i].Time
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	return ts[k/2]
}

// MaxFutureSeconds bounds how far ahead of local time a block may be.
const MaxFutureSeconds = 2 * 60 * 60

// ApplyBlock validates a block against this state and applies it. now is the
// local unix time (0 skips the future-time check, used when replaying).
// ReplayBlock applies a block this node validated in full before: one read
// back from its own database at start-up, or re-applied to rebuild a pruned
// state. The VDF, the block signature, the packed storage proof and the
// anonymous-post proofs are not checked again; everything that changes the
// state is. Without this, start-up would redo every block's VDF.
func (s *State) ReplayBlock(b *types.Block) error {
	s.replay = true
	defer func() { s.replay = false }()
	return s.ApplyBlock(b, 0)
}

func (s *State) ApplyBlock(b *types.Block, now int64) error {
	hd := &b.Header
	h := s.Height + 1
	if hd.Version != 1 {
		return errors.New("bad version")
	}
	if hd.Height != h || hd.Prev != s.Tip {
		return errors.New("block does not extend this state")
	}
	if hd.Time <= s.MedianTime() {
		return errors.New("block time not after median of recent blocks")
	}
	if now > 0 && hd.Time > now+MaxFutureSeconds {
		return errors.New("block time too far in the future")
	}
	target := s.ExpectedTarget()
	if hd.TargetInt().Cmp(target) != 0 {
		return errors.New("wrong difficulty target")
	}
	if len(hd.VDFOutputs) == 0 || hd.VDFStep != s.VDFStep+uint64(len(hd.VDFOutputs)) {
		return errors.New("bad VDF step count")
	}
	if !s.replay && s.vdfCheck >= 0 {
		start := vdf.Reseed(s.VDFOut, s.Tip)
		ok := false
		if n := len(hd.VDFOutputs); s.vdfCheck > 0 && n > s.vdfCheck {
			idx := mrand.Perm(n)[:s.vdfCheck]
			ok = vdf.VerifySteps(start, hd.VDFOutputs, s.P.VDFIterations, idx, runtime.NumCPU())
		} else {
			ok = vdf.VerifyChain(start, hd.VDFOutputs, s.P.VDFIterations)
		}
		if !ok {
			return errors.New("bad VDF outputs")
		}
	}
	if len(hd.Miner) != 32 || len(hd.Sig) != 64 {
		return errors.New("bad miner key")
	}
	sh := hd.SigHash()
	if !s.replay && !cryptox.Verify(hd.Miner, sh[:], hd.Sig) {
		return errors.New("bad block signature")
	}
	miner := hd.MinerAddr()
	rr, ok := s.RangeFor(hd.VDFOutputs[len(hd.VDFOutputs)-1], hd.Sol.Partition, miner, h, s.WeaveChunks())
	if !ok {
		return errors.New("solution partition has no recall range at this step")
	}
	c, ok := s.Ticket(rr, hd.Sol.Position, int(hd.Sol.Sub))
	if !ok {
		return errors.New("solution is not a ticket of its recall range")
	}
	if !s.replay && !s.VerifyPacked(c, miner, hd.Sol.PackedSub, hd.Sol.Path) {
		return errors.New("solution chunk does not verify")
	}
	if !MeetsTarget(SolutionHash(c, hd.Sol.PackedSub), target, c.Weight) {
		return errors.New("solution misses the target")
	}
	if hd.TxRoot != types.TxRoot(b.Txs) || hd.BoardRoot != types.BoardRoot(b.Txs) {
		return errors.New("bad tx or board root")
	}
	ctx := s.NewCtx()
	seen := map[Hash]bool{}
	for i, t := range b.Txs {
		id := t.ID()
		if seen[id] {
			return errors.New("duplicate transaction")
		}
		seen[id] = true
		if err := s.ApplyTx(t, ctx); err != nil {
			return fmt.Errorf("tx %d (%s): %w", i, t.Kind, err)
		}
	}
	if err := s.seal(hd, h); err != nil {
		return err
	}
	if err := s.finish(b, ctx, target); err != nil {
		return err
	}
	// Checked on replay too: it is cheap (only what the block changed is
	// hashed) and keeps the cached commitments warm.
	if !s.noRoot && s.Root() != hd.StateRoot {
		return errors.New("state root does not match the block")
	}
	return nil
}

// SetVDFCheck sets how the next ApplyBlock checks the VDF (see vdfCheck).
func (s *State) SetVDFCheck(n int) { s.vdfCheck = n }

// NextRoot is the state root a block built on this state must carry. The
// header's other fields must be final; its signature and root are not read.
// The proofs the miner has just made are not checked again.
func (s *State) NextRoot(b *types.Block) (Hash, error) {
	c := s.Clone()
	c.replay, c.noRoot = true, true
	probe := *b
	if len(probe.Sig) != 64 { // not signed yet: the signature is not checked here
		probe.Sig = make([]byte, 64)
	}
	if err := c.ApplyBlock(&probe, 0); err != nil {
		return Hash{}, err
	}
	return c.Root(), nil
}

// finish runs the end-of-block rules: expiry, payouts, issuance, vesting,
// forfeits, and the price, fee and difficulty inputs of the next block.
func (s *State) finish(b *types.Block, ctx *BlockCtx, target *big.Int) error {
	h := ctx.H
	hd := &b.Header

	s.releaseExits(h)
	s.fundDue(h, hd.Prev)

	// Expire term buckets.
	var ids []uint32
	for id := range s.Buckets {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var payouts uint64
	for _, id := range ids {
		if id == 0 {
			continue
		}
		bk := s.Buckets[id]
		exp := s.BucketExpiry(id)
		if exp <= h {
			s.RepairPool += bk.Escrow
			for i := 0; i < bk.Records.Len(); i++ {
				r := bk.Records.Ptr(i)
				if info, ok := s.Data.Get(r.DataRoot); ok && info.Bucket == id && !r.Moved {
					s.Data.Delete(r.DataRoot)
				}
			}
			for i := 0; i < bk.Posts.Len(); i++ {
				p := bk.Posts.At(i)
				if pi, ok := s.Posts.Get(p); ok && pi.TextBucket == id {
					s.Posts.Delete(p)
				}
			}
			s.TermBytes -= bk.Bytes
			delete(s.Buckets, id)
			continue
		}
		if rel := bk.Escrow / (exp - h); rel > 0 {
			s.bucket(id).Escrow -= rel
			payouts += rel
		}
	}
	// Permanent data is paid from the endowment at the current price.
	due := mulDiv(s.PermBytes*s.P.MediaReplicas, s.Price, params.GB) / s.P.BlocksPerYear
	if due > s.Endowment {
		due = s.Endowment
	}
	s.Endowment -= due
	payouts += due
	repair := mulDiv(s.RepairPool, s.P.RepairReleasePPM, 1_000_000) / s.P.BlocksPerDay
	s.RepairPool -= repair

	// Issuance.
	reward := s.BlockReward(h)
	if ctx.FreeStorage > reward {
		return errors.New("free-lane storage exceeds block reward")
	}
	s.Supply += reward // the anonymous lane's storage is already in escrow
	s.Supply -= ctx.Burn
	s.Burned += ctx.Burn

	income := reward - ctx.FreeStorage + payouts + repair + ctx.Tips
	miner := s.Account(hd.MinerAddr())
	miner.LastProof = s.Epoch(h) // winning a block is a storage proof
	s.settle(miner, h)
	liquid := mulDiv(income, s.P.LiquidPermille, 1000)
	miner.Balance += liquid
	if v := income - liquid; v > 0 {
		// At most one tranche per epoch keeps the list short.
		n := len(miner.Vesting)
		if n > 0 && s.Epoch(miner.Vesting[n-1].Start) == s.Epoch(h) {
			miner.Vesting[n-1].Amount += v
		} else {
			miner.Vesting = append(miner.Vesting, Tranche{Amount: v, Start: h, End: h + s.P.VestingBlocks})
		}
		s.Vesters.Set(hd.MinerAddr(), true)
	}

	// Forfeit unvested income of storage nodes that stopped proving.
	if h%s.P.ProofEpoch == 0 {
		// Only accounts that ever earned vesting income can forfeit, so this
		// walks those (miners), not every account.
		ep := s.Epoch(h)
		var drop []Address
		s.Vesters.Range(func(addr Address, _ bool) bool {
			a, ok := s.Accounts.Get(addr)
			switch {
			case !ok || len(a.Vesting) == 0:
				drop = append(drop, addr)
			case ep > a.LastProof+s.P.ForfeitEpochs:
				w := s.Account(addr)
				for _, t := range w.Vesting {
					s.RepairPool += t.Amount - t.Claimed
				}
				w.Vesting = nil
				drop = append(drop, addr)
			}
			return true
		})
		for _, addr := range drop {
			s.Vesters.Delete(addr)
		}
	}
	// The last block of an epoch fixes the next epoch's challenge inputs.
	if (h+1)%s.P.ProofEpoch == 0 {
		s.EpochSeed = hd.Hash()
		for id, b := range s.Buckets {
			if b.EpochLen != b.Len {
				s.bucket(id).EpochLen = b.Len
			}
		}
	}

	// Price, base fee, congestion, proven storage.
	steps := uint64(len(hd.VDFOutputs))
	obs := s.ProvenBytes(types.Work(target), steps)
	w := uint64(s.P.RetargetWindow)
	if obs > s.Proven {
		s.Proven += (obs - s.Proven) / w
	} else {
		s.Proven -= (s.Proven - obs) / w
	}
	s.Price = NextPrice(s.P, s.Price, &s.PriceEMA, ObservedPrice(s.P, reward+payouts, s.Proven))
	s.BaseFee = NextBaseFee(s.P, s.BaseFee, ctx.FeeUsed)
	s.Congestion = NextCongestion(s.P, s.Congestion, ctx.FreeUsed)
	s.rolloverEpoch(h + 1)

	s.Recent = append(s.Recent, HeaderMeta{Time: hd.Time, Target: hd.Target, Steps: steps})
	if keep := s.P.RetargetWindow + 12; len(s.Recent) > keep {
		s.Recent = append([]HeaderMeta(nil), s.Recent[len(s.Recent)-keep:]...)
	}
	s.CumWork.Add(s.CumWork, types.Work(target))
	s.Height = h
	s.Tip = hd.Hash()
	s.VDFOut = hd.VDFOutputs[len(hd.VDFOutputs)-1]
	s.VDFStep = hd.VDFStep
	return nil
}
