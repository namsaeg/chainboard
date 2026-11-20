package state

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"github.com/namsaeg/chainboard/internal/pmap"
	"sort"
)

// Digest hashes the whole state in a fixed order. Equal states have equal
// digests however they were reached, so it checks that replaying blocks is
// deterministic and that writing one state version never changes another.
// It is O(size of the state): a check, not something to run per block.
func (s *State) Digest() Hash {
	h := sha256.New()
	fmt.Fprintf(h, "%d %x %x %x %d %x|", s.Height, s.Tip, s.VDFOut, s.EpochSeed, s.VDFStep, s.CumWork.Bytes())
	fmt.Fprintf(h, "%d %d %d %d %d %d %d %d %d %d %d|", s.Supply, s.Burned, s.Endowment, s.RepairPool, s.Price,
		s.PriceEMA, s.BaseFee, s.Congestion, s.PermBytes, s.TermBytes, s.Proven)
	fmt.Fprintf(h, "%v|%v|%v|%v|%d %d %d %d %d|", s.Recent, s.TextQueue, s.RLNRoots, s.RLNExits,
		s.RLNCap, s.RLNCapPrev, s.RLNCapEpoch, s.MemberCount, s.MemberSlots)
	root := s.RLNRoot()
	fmt.Fprintf(h, "%x|", root)

	sorted := func(lines []string) {
		sort.Strings(lines)
		for _, l := range lines {
			h.Write([]byte(l))
		}
		h.Write([]byte{'|'})
	}
	var lines []string
	s.Accounts.Range(func(k Address, v *Account) bool {
		lines = append(lines, fmt.Sprintf("%x %+v\n", k, *v))
		return true
	})
	sorted(lines)
	lines = nil
	s.Vesters.Range(func(k Address, _ bool) bool { lines = append(lines, fmt.Sprintf("%x\n", k)); return true })
	sorted(lines)
	lines = nil
	for id, b := range s.Buckets {
		var rec bytes.Buffer
		for i := 0; i < b.Records.Len(); i++ {
			fmt.Fprintf(&rec, "%+v;", b.Records.At(i))
		}
		for i := 0; i < b.Posts.Len(); i++ {
			fmt.Fprintf(&rec, "p%x;", b.Posts.At(i))
		}
		lines = append(lines, fmt.Sprintf("%d %d %d %d %d %s\n", id, b.Len, b.EpochLen, b.Escrow, b.Bytes, rec.String()))
	}
	sorted(lines)
	lines = nil
	s.Data.Range(func(k Hash, v *DataInfo) bool { lines = append(lines, fmt.Sprintf("%x %+v\n", k, *v)); return true })
	sorted(lines)
	lines = nil
	s.Posts.Range(func(k Hash, v *PostInfo) bool { lines = append(lines, fmt.Sprintf("%x %+v\n", k, *v)); return true })
	sorted(lines)
	lines = nil
	s.Funds.Range(func(k Hash, c *Campaign) bool {
		var ps bytes.Buffer
		for i := 0; i < c.Pledges.Len(); i++ {
			fmt.Fprintf(&ps, "%+v;", c.Pledges.At(i))
		}
		lines = append(lines, fmt.Sprintf("%x %x %x %d %d %d %d %d %d %d %d %d %d %d %x %d %d %s\n", k, c.Creator, c.DataRoot, c.Size,
			c.Media, c.MinTier, c.Deadline, c.Status, c.Escrow, c.Raised, c.Upload, c.Tier, c.Bucket, c.Held, c.Seed,
			c.RefundPot, c.RefundAt, ps.String()))
		return true
	})
	s.Bounties.Range(func(k Hash, b *Bounty) bool {
		var ps bytes.Buffer
		for i := 0; i < b.Pledges.Len(); i++ {
			fmt.Fprintf(&ps, "%+v;", b.Pledges.At(i))
		}
		lines = append(lines, fmt.Sprintf("bounty %x %x %d %d %d %d %d %x %x %x %d %d %d %x %d %d %d %x %d %d %s\n", k, b.Requester, b.Deadline,
			b.Tier, b.Status, b.Escrow, b.Raised, b.Offer, b.Hunter, b.DataRoot, b.Size, b.Media, b.Accepted, b.Seed, b.Bucket,
			b.Held, b.Proven, b.Failed, b.RefundPot, b.RefundAt, ps.String()))
		return true
	})
	lines = append(lines, fmt.Sprintf("refunding %x\n", s.Refunding))
	s.FundDue.Range(func(h uint64, ids []Hash) bool {
		lines = append(lines, fmt.Sprintf("due %d %x\n", h, ids))
		return true
	})
	sorted(lines)
	lines = nil
	// Repeat counts as they read at the next height (generations are an
	// implementation detail; what matters is what each key reads as).
	next := s.Height + 1
	for _, m := range []pmap.Map[Hash, dupEntry]{s.dupCur, s.dupPrev} {
		m.Range(func(k Hash, _ dupEntry) bool {
			if e := s.dupAt(k, next); e.Count > 0 {
				lines = append(lines, fmt.Sprintf("%x %+v\n", k, e))
			}
			return true
		})
	}
	sort.Strings(lines)
	lines = dedup(lines)
	sorted(lines)
	lines = nil
	s.Members.Range(func(k Hash, v *Member) bool { lines = append(lines, fmt.Sprintf("%x %+v\n", k, *v)); return true })
	sorted(lines)
	lines = nil
	for e, m := range s.RLNNulls {
		m.Range(func(k Hash, v Share) bool { lines = append(lines, fmt.Sprintf("%d %x %+v\n", e, k, v)); return true })
	}
	sorted(lines)
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

func dedup(sorted []string) []string {
	out := sorted[:0]
	for i, l := range sorted {
		if i == 0 || l != sorted[i-1] {
			out = append(out, l)
		}
	}
	return out
}
