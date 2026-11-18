package state

import (
	"errors"
	"fmt"
	"sort"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/types"
)

// Texts are not kept in blocks. A post commits to the hash of its body and
// pays for the body's retention tier; the body travels next to the
// transaction and waits in nodes' text stores. Every TextSealBlocks, the seal
// block places the window's bodies in the weave: one bundle per retention
// bucket, the bodies concatenated in queue order. From then on miners keep
// and prove them like uploads, and they expire with their bucket.

// TextEntry is a body waiting for the next seal.
type TextEntry struct {
	Post   Hash
	Hash   Hash
	Len    uint16
	Bucket uint32
}

// BundleSpec is one bundle the next seal places: which bodies, in order.
type BundleSpec struct {
	Bucket  uint32
	Size    uint64
	Entries []TextEntry
}

// SealsAt reports whether the block at height h is a seal block.
func (s *State) SealsAt(h uint64) bool { return (h+1)%s.P.TextSealBlocks == 0 }

// textFee prices keeping n body bytes at a tier and names the bucket paid.
func (s *State) textFee(n uint64, tier uint16, h uint64) (uint64, uint32) {
	months, perm, _ := params.TierMonths(tier)
	return StorageFee(s.P, s.Price, n, s.P.TextReplicas, months, perm), s.BucketFor(h, months, perm)
}

// AnonTextFee is the storage of an anonymous post, paid from issuance.
func (s *State) AnonTextFee(t *types.Tx) uint64 {
	fee, _ := s.textFee(uint64(t.TextLen), params.TierFree, s.Height+1)
	return fee
}

// fund puts a storage fee where it pays out: the endowment for permanent
// data, else the escrow of its bucket.
func (s *State) fund(dest uint32, fee uint64) {
	if dest == 0 {
		s.Endowment += fee
		return
	}
	b, ok := s.Buckets[dest]
	if !ok {
		b = &Bucket{ID: dest}
		s.Buckets[dest] = b
	}
	b.Escrow += fee
}

func (s *State) queueText(post, hash Hash, n uint16, bucket uint32) {
	s.TextQueue = append(s.TextQueue, TextEntry{Post: post, Hash: hash, Len: n, Bucket: bucket})
}

// PendingBundles groups the queue by bucket, as the seal block at height h
// must place it. Entries whose bucket is gone by then are dropped.
func (s *State) PendingBundles(h uint64) []BundleSpec {
	by := map[uint32]*BundleSpec{}
	for _, e := range s.TextQueue {
		if e.Bucket != 0 && s.BucketExpiry(e.Bucket) <= h {
			continue
		}
		b := by[e.Bucket]
		if b == nil {
			b = &BundleSpec{Bucket: e.Bucket}
			by[e.Bucket] = b
		}
		b.Size += uint64(e.Len)
		b.Entries = append(b.Entries, e)
	}
	out := make([]BundleSpec, 0, len(by))
	for _, b := range by {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bucket < out[j].Bucket })
	return out
}

// seal checks the header's bundles against the queue and places them. The
// bundle roots cannot be checked from state alone: nodes that hold the
// bodies check them before accepting a recent block (see chain).
func (s *State) seal(hd *types.Header, h uint64) error {
	if !s.SealsAt(h) {
		if len(hd.TextBundles) != 0 {
			return errors.New("text bundles outside a seal block")
		}
		return nil
	}
	want := s.PendingBundles(h)
	if len(want) != len(hd.TextBundles) {
		return fmt.Errorf("seal block places %d text bundles, want %d", len(hd.TextBundles), len(want))
	}
	for i, w := range want {
		got := hd.TextBundles[i]
		if got.Bucket != w.Bucket || got.Size != w.Size || got.Root.IsZero() {
			return fmt.Errorf("text bundle %d does not match the queue", i)
		}
	}
	if s.textCheck != nil && !s.replay {
		for i, w := range want {
			if err := s.textCheck(w, hd.TextBundles[i].Root); err != nil {
				return err
			}
		}
	}
	for i, w := range want {
		root := hd.TextBundles[i].Root
		b, ok := s.Buckets[w.Bucket]
		if !ok {
			b = &Bucket{ID: w.Bucket}
			s.Buckets[w.Bucket] = b
		}
		n := chunk.Count(w.Size)
		b.Records = append(b.Records, Record{DataRoot: root, Size: w.Size, Start: b.Len, Chunks: n})
		b.Len += n
		b.Bytes += w.Size
		if w.Bucket == 0 {
			s.PermBytes += w.Size
		} else {
			s.TermBytes += w.Size
		}
		off := uint32(0)
		for _, e := range w.Entries {
			if pi, ok := s.Posts[e.Post]; ok {
				c := *pi
				c.TextRoot, c.TextOff = root, off
				s.Posts[e.Post] = &c
			}
			off += uint32(e.Len)
		}
	}
	s.TextQueue = nil
	return nil
}

// SetTextCheck installs (or with nil removes) the bundle verifier used by
// the next ApplyBlock.
func (s *State) SetTextCheck(f func(BundleSpec, Hash) error) { s.textCheck = f }

// BundleBytes assembles a bundle from bodies looked up by hash.
func BundleBytes(b BundleSpec, body func(Hash) ([]byte, bool)) ([]byte, []Hash) {
	out := make([]byte, 0, b.Size)
	var missing []Hash
	for _, e := range b.Entries {
		t, ok := body(e.Hash)
		if !ok || len(t) != int(e.Len) {
			missing = append(missing, e.Hash)
			continue
		}
		out = append(out, t...)
	}
	return out, missing
}

// TextExpired reports whether a post's body has passed its retention.
func (s *State) TextExpired(pi *PostInfo) bool {
	return pi.TextLen > 0 && pi.TextBucket != 0 && s.BucketExpiry(pi.TextBucket) <= s.Height
}

// TextUntil is the height a post's body is kept until (0: permanent).
func (s *State) TextUntil(pi *PostInfo) uint64 {
	if pi.TextBucket == 0 {
		return 0
	}
	return s.BucketExpiry(pi.TextBucket)
}

// postExtendFee prices lengthening a post's text retention.
func (s *State) postExtendFee(t *types.Tx, h uint64) (uint64, uint32, error) {
	pi, ok := s.Posts[t.Target]
	if !ok {
		return 0, 0, errors.New("extend of unknown post")
	}
	if pi.TextLen == 0 || pi.TextHash != t.TextHash || pi.TextLen != t.TextLen {
		return 0, 0, errors.New("extend does not match the post's text")
	}
	if pi.TextTier == params.TierPermanent {
		return 0, 0, errors.New("text is already kept permanently")
	}
	if s.TextExpired(pi) {
		return 0, 0, errors.New("text has expired")
	}
	months, perm, _ := params.TierMonths(t.Tier)
	dest := s.BucketFor(h, months, perm)
	if !perm && dest <= pi.TextBucket {
		return 0, 0, fmt.Errorf("text is already kept until height %d, which this tier would not outlast",
			s.BucketExpiry(pi.TextBucket))
	}
	n := uint64(pi.TextLen)
	full := StorageFee(s.P, s.Price, n, s.P.TextReplicas, months, perm)
	var rem uint64
	if exp := s.BucketExpiry(pi.TextBucket); exp > h {
		rem = (exp - h) / s.P.BlocksPerMonth
	}
	credit := min(StorageFee(s.P, s.Price, n, s.P.TextReplicas, rem, false), full)
	return full - credit, dest, nil
}

func (s *State) applyPostExtend(t *types.Tx, dest uint32) {
	c := *s.Posts[t.Target]
	c.TextTier, c.TextBucket = t.Tier, dest
	s.Posts[t.Target] = &c
	s.queueText(t.Target, t.TextHash, t.TextLen, dest)
}

// burstUnit is one point of burst score.
const burstUnit = 1000

// decayBurst ages a score by dh blocks: × ((tau-1)/tau)^dh, in fixed point.
func decayBurst(score, dh, tau uint64) uint64 {
	if score == 0 || dh == 0 {
		return score
	}
	if tau < 2 || dh >= 40*tau {
		return 0
	}
	f := ((tau - 1) << 32) / tau
	r := uint64(1) << 32
	for e := dh; e > 0; e >>= 1 {
		if e&1 == 1 {
			r = r * f >> 32
		}
		f = f * f >> 32
	}
	return score * r >> 32
}

// countsForBurst reports whether a kind raises the sender's burst score.
// Storage duties do not.
func countsForBurst(k types.TxKind) bool { return k != types.TxProof && k != types.TxRegister }

// BurstScore is an account's score at height h before its next transaction.
func (s *State) BurstScore(a *Account, h uint64) uint64 {
	if h < a.BurstAt {
		return a.Burst
	}
	return decayBurst(a.Burst, h-a.BurstAt, s.P.BurstTauBlocks)
}

// BurstMultiplier is the base-fee factor for a score that includes the
// transaction being priced.
func (s *State) BurstMultiplier(score uint64) uint64 {
	free := s.P.BurstFree * burstUnit
	if score <= free {
		return 1
	}
	k := (score - free) / (s.P.BurstStep * burstUnit)
	return 1 << min(k, 20)
}

// FeeFactors are the surcharges a signed transaction from its sender would
// pay at the next block: repeat (same text) and burst (sending fast).
func (s *State) FeeFactors(t *types.Tx) (dup, burst uint64) {
	h := s.Height + 1
	dup, burst = 1, 1
	if t.Kind == types.TxPost {
		_, dup = s.dupMultiplier(t, h)
	}
	if countsForBurst(t.Kind) {
		acc := s.Peek(t.Sender())
		burst = s.BurstMultiplier(s.BurstScore(&acc, h) + burstUnit)
	}
	return dup, burst
}

// satMul multiplies, saturating instead of wrapping.
func satMul(a, b uint64) uint64 {
	if a != 0 && b > ^uint64(0)/a {
		return ^uint64(0)
	}
	return a * b
}
