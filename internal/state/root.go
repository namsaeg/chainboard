package state

import (
	"crypto/sha256"
	"sort"

	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/pmap"
)

// The state root commits to the whole state after a block, so a client that
// trusts a header can check any part of the state against it. The large
// tables are persistent tries that cache the commitment of every node, so a
// block costs hashing in proportion to what it changed. Tip and EpochSeed
// are left out: they are block hashes, and the header that carries the root
// cannot commit to its own hash.

func sumOf(tag string, w *codec.Writer) pmap.Sum {
	return cryptox.Tagged("cb/state/"+tag, w.B)
}

func accountSum(a Address, x *Account) pmap.Sum {
	var w codec.Writer
	w.Fixed(a[:])
	w.Var(x.Balance)
	w.Var(x.Nonce)
	w.Var(x.StakeMature)
	w.Var(x.StakePending)
	w.Var(x.PendingSince)
	w.Var(uint64(len(x.Unbonding)))
	for _, u := range x.Unbonding {
		w.Var(u.Amount)
		w.Var(u.Release)
	}
	w.Var(uint64(len(x.Vesting)))
	for _, t := range x.Vesting {
		w.Var(t.Amount)
		w.Var(t.Claimed)
		w.Var(t.Start)
		w.Var(t.End)
	}
	w.Var(x.LastProof)
	w.Var(uint64(len(x.Partitions)))
	for _, p := range x.Partitions {
		w.U64(p)
	}
	w.Var(x.ProofsInEpoch)
	w.Var(uint64(len(x.ProveParts)))
	for _, p := range x.ProveParts {
		w.U64(p)
	}
	w.Var(x.PartsEpoch)
	w.Var(x.Burst)
	w.Var(x.BurstAt)
	return sumOf("account", &w)
}

func flagSum(a Address, _ bool) pmap.Sum {
	var w codec.Writer
	w.Fixed(a[:])
	return sumOf("vester", &w)
}

func dataSum(root Hash, d *DataInfo) pmap.Sum {
	var w codec.Writer
	w.Fixed(root[:])
	w.U32(d.Bucket)
	w.Var(d.Size)
	w.U8(d.Media)
	w.U16(d.Tier)
	w.Var(uint64(d.Record))
	return sumOf("data", &w)
}

func postSum(id Hash, p *PostInfo) pmap.Sum {
	var w codec.Writer
	w.Fixed(id[:])
	w.Fixed(p.Parent[:])
	w.Fixed(p.TextHash[:])
	w.U16(p.TextLen)
	w.U16(p.TextTier)
	w.U32(p.TextBucket)
	w.Fixed(p.TextRoot[:])
	w.U32(p.TextOff)
	return sumOf("post", &w)
}

func dupSum(k Hash, e dupEntry) pmap.Sum {
	var w codec.Writer
	w.Fixed(k[:])
	w.Var(e.Count)
	w.Var(e.Last)
	return sumOf("dup", &w)
}

func memberSum(idc Hash, m *Member) pmap.Sum {
	var w codec.Writer
	w.Fixed(idc[:])
	w.Fixed(m.Owner[:])
	w.U32(m.Index)
	w.U16(m.Limit)
	w.Var(m.Bond)
	w.U8(uint8(m.Status))
	w.Var(m.Release)
	return sumOf("member", &w)
}

func shareSum(null Hash, s Share) pmap.Sum {
	var w codec.Writer
	w.Fixed(null[:])
	w.Fixed(s.X[:])
	w.Fixed(s.Y[:])
	return sumOf("share", &w)
}

func recordSum(r Record) pmap.Sum {
	var w codec.Writer
	w.Fixed(r.DataRoot[:])
	w.Var(r.Size)
	w.Var(r.Start)
	w.Var(r.Chunks)
	w.Bool(r.Moved)
	return sumOf("record", &w)
}

func hashSum(h Hash) pmap.Sum { return h }

func newDupMap() pmap.Map[Hash, dupEntry] { return pmap.Committed(pmap.Hash32[Hash], dupSum) }
func newNullMap() pmap.Map[Hash, Share]   { return pmap.Committed(pmap.Hash32[Hash], shareSum) }

// Root is the state commitment a block header carries.
func (s *State) Root() Hash {
	var w codec.Writer
	w.Var(s.Height)
	w.Fixed(s.VDFOut[:])
	w.Var(s.VDFStep)
	for _, v := range []uint64{s.Supply, s.Burned, s.Endowment, s.RepairPool, s.Price, s.PriceEMA, s.BaseFee,
		s.Congestion, s.PermBytes, s.TermBytes, s.Proven, s.dupGen, s.MemberCount, s.MemberSlots,
		s.RLNCap, s.RLNCapPrev, s.RLNCapEpoch} {
		w.Var(v)
	}
	w.Bytes(s.CumWork.Bytes())
	w.Var(uint64(len(s.Recent)))
	for _, m := range s.Recent {
		w.U64(uint64(m.Time))
		w.Fixed(m.Target[:])
		w.Var(m.Steps)
	}
	w.Var(uint64(len(s.TextQueue)))
	for _, e := range s.TextQueue {
		w.Fixed(e.Post[:])
		w.Fixed(e.Hash[:])
		w.U16(e.Len)
		w.U32(e.Bucket)
	}
	for _, r := range []pmap.Sum{s.Accounts.Root(), s.Vesters.Root(), s.Data.Root(), s.Posts.Root(),
		s.dupCur.Root(), s.dupPrev.Root(), s.Members.Root(), s.RLNRoot(), s.Funds.Root(), s.FundDue.Root()} {
		w.Fixed(r[:])
	}
	ids := make([]uint32, 0, len(s.Buckets))
	for id := range s.Buckets {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	w.Var(uint64(len(ids)))
	for _, id := range ids {
		b := s.Buckets[id]
		w.U32(id)
		w.Var(b.Len)
		w.Var(b.EpochLen)
		w.Var(b.Escrow)
		w.Var(b.Bytes)
		rr, pr := b.Records.Root(recordSum), b.Posts.Root(hashSum)
		w.Fixed(rr[:])
		w.Fixed(pr[:])
	}
	w.Var(uint64(len(s.RLNRoots)))
	for _, e := range s.RLNRoots {
		w.Fixed(e.Root[:])
		w.Var(e.Replaced)
	}
	w.Var(uint64(len(s.RLNExits)))
	for _, e := range s.RLNExits {
		w.Var(e.At)
		w.Fixed(e.IDC[:])
	}
	epochs := make([]uint64, 0, len(s.RLNNulls))
	for e := range s.RLNNulls {
		epochs = append(epochs, e)
	}
	sort.Slice(epochs, func(i, j int) bool { return epochs[i] < epochs[j] })
	w.Var(uint64(len(epochs)))
	for _, e := range epochs {
		w.Var(e)
		r := s.RLNNulls[e].Root()
		w.Fixed(r[:])
	}
	return sha256.Sum256(append([]byte("cb/state/root"), w.B...))
}
