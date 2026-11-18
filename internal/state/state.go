// Package state is the chain state and its transition function. It is pure:
// applying the same blocks to the same state always gives the same result.
package state

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"math/big"
	"sort"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/rln"
	"github.com/namsaeg/chainboard/internal/types"
)

type (
	Hash    = cryptox.Hash
	Address = cryptox.Address
)

// Tranche is one batch of vesting income.
type Tranche struct {
	Amount, Claimed uint64
	Start, End      uint64
}

// Unbond is stake on its way back to the balance.
type Unbond struct {
	Amount, Release uint64
}

// Account is an address's state.
type Account struct {
	Balance uint64
	Nonce   uint64

	StakeMature   uint64
	StakePending  uint64
	PendingSince  uint64
	Unbonding     []Unbond
	Vesting       []Tranche
	LastProof     uint64 // last epoch with a storage proof (block or proof tx)
	Partitions    []uint64
	ProofsInEpoch uint64 // epoch of the last proof tx (one per epoch)
	// A registration takes effect for epoch challenges from the next epoch:
	// during the epoch it changed in (PartsEpoch-1), ProveParts are challenged.
	ProveParts []uint64
	PartsEpoch uint64
	// Burst score in thousandths of a point, as of BurstAt (see params).
	Burst   uint64
	BurstAt uint64
}

func (a *Account) clone() *Account {
	c := *a
	c.Unbonding = append([]Unbond(nil), a.Unbonding...)
	c.Vesting = append([]Tranche(nil), a.Vesting...)
	c.Partitions = append([]uint64(nil), a.Partitions...)
	c.ProveParts = append([]uint64(nil), a.ProveParts...)
	return &c
}

// Record is one data root placed in a bucket.
type Record struct {
	DataRoot Hash
	Size     uint64
	Start    uint64 // first chunk position in the bucket
	Chunks   uint64
	Moved    bool // extended into another bucket; no longer challengeable here
}

// Bucket is an append-only chunk space. Bucket 0 is permanent; bucket k > 0
// expires as a whole at height k × BlocksPerMonth.
type Bucket struct {
	ID  uint32
	Len uint64 // positions used
	// EpochLen is Len when the current proof epoch began; epoch challenges
	// only point below it, so they stay the same for the whole epoch.
	EpochLen uint64
	Records  []Record
	Escrow   uint64 // prepaid fees still to be paid out to storage
	Bytes    uint64 // live (not moved) bytes
}

func (b *Bucket) clone() *Bucket {
	c := *b
	c.Records = append([]Record(nil), b.Records...)
	return &c
}

// DataInfo is where a data root currently lives.
type DataInfo struct {
	Bucket uint32
	Size   uint64
	Media  uint8
	Tier   uint16
	Record int // index in the bucket's records
}

// PostInfo is what the chain remembers about a post.
type PostInfo struct {
	Board    string
	Author   Address
	Parent   Hash
	Height   uint64
	Likes    uint64
	Comments uint64

	// The body: its commitment, retention, and where the seal put it
	// (TextRoot is zero until the window's seal block).
	TextHash   Hash
	TextLen    uint16
	TextTier   uint16
	TextBucket uint32
	TextRoot   Hash
	TextOff    uint32
}

type dupEntry struct{ Count, Last uint64 }

// HeaderMeta keeps what difficulty and time rules need from recent blocks.
type HeaderMeta struct {
	Time   int64
	Target Hash
	Steps  uint64
}

// State is the full chain state at one block.
type State struct {
	// replay is set while re-applying a block this node already validated:
	// the expensive proofs are not checked again (see ReplayBlock).
	replay bool
	// textCheck, while set, verifies each text bundle a seal places.
	textCheck func(BundleSpec, Hash) error

	P         *params.Params
	Height    uint64
	Tip       Hash
	VDFOut    Hash // last VDF output
	VDFStep   uint64
	EpochSeed Hash // hash of the block that opened the current proof epoch

	Accounts map[Address]*Account
	Buckets  map[uint32]*Bucket
	Data     map[Hash]*DataInfo
	Posts    map[Hash]*PostInfo
	Likes    map[[52]byte]bool
	dups     map[Hash]dupEntry

	TextQueue []TextEntry // bodies waiting for the next seal block

	Supply     uint64 // minted minus burned
	Burned     uint64
	Endowment  uint64
	RepairPool uint64
	Price      uint64 // units per raw GB-year
	PriceEMA   uint64
	BaseFee    uint64 // units per byte, fee lane
	Congestion uint64 // anonymous-lane pressure, permille (shrinks the RLN cap)
	PermBytes  uint64 // live permanent media bytes
	TermBytes  uint64 // live term media bytes
	Proven     uint64 // estimated proven storage, bytes
	Recent     []HeaderMeta
	CumWork    *big.Int

	// Anonymous lane (RLN).
	Members     map[Hash]*Member // by identity commitment; copy-on-write entries
	RLNTree     *rln.Tree
	RLNRoots    []RootEntry               // accepted membership roots, newest (current) last
	RLNNulls    map[uint64]map[Hash]Share // epoch -> nullifier -> share
	RLNExits    []exit                    // releases in height order
	RLNCap      uint64                    // per-member cap of RLNCapEpoch
	RLNCapPrev  uint64                    // cap of the epoch before
	RLNCapEpoch uint64
}

// Clone deep-copies the state.
func (s *State) Clone() *State {
	c := *s
	c.replay = false
	c.textCheck = nil
	c.TextQueue = append([]TextEntry(nil), s.TextQueue...)
	c.Accounts = make(map[Address]*Account, len(s.Accounts))
	for k, v := range s.Accounts {
		c.Accounts[k] = v.clone()
	}
	c.Buckets = make(map[uint32]*Bucket, len(s.Buckets))
	for k, v := range s.Buckets {
		c.Buckets[k] = v.clone()
	}
	c.Data = make(map[Hash]*DataInfo, len(s.Data))
	for k, v := range s.Data {
		d := *v
		c.Data[k] = &d
	}
	c.Posts = make(map[Hash]*PostInfo, len(s.Posts))
	for k, v := range s.Posts {
		c.Posts[k] = v // entries are replaced, never modified in place
	}
	c.Likes = make(map[[52]byte]bool, len(s.Likes))
	for k, v := range s.Likes {
		c.Likes[k] = v
	}
	c.dups = make(map[Hash]dupEntry, len(s.dups))
	for k, v := range s.dups {
		c.dups[k] = v
	}
	c.Recent = append([]HeaderMeta(nil), s.Recent...)
	c.CumWork = new(big.Int).Set(s.CumWork)
	c.Members = make(map[Hash]*Member, len(s.Members))
	for k, v := range s.Members {
		c.Members[k] = v // entries are replaced, never modified in place
	}
	c.RLNTree = s.RLNTree.Clone()
	c.RLNRoots = append([]RootEntry(nil), s.RLNRoots...)
	c.RLNNulls = make(map[uint64]map[Hash]Share, len(s.RLNNulls))
	for e, m := range s.RLNNulls {
		cm := make(map[Hash]Share, len(m))
		for k, v := range m {
			cm[k] = v
		}
		c.RLNNulls[e] = cm
	}
	c.RLNExits = append([]exit(nil), s.RLNExits...)
	return &c
}

// Account returns an account, creating it on write paths.
func (s *State) Account(a Address) *Account {
	acc, ok := s.Accounts[a]
	if !ok {
		acc = &Account{}
		s.Accounts[a] = acc
	}
	return acc
}

// Peek returns a copy of an account without creating it.
func (s *State) Peek(a Address) Account {
	if acc, ok := s.Accounts[a]; ok {
		return *acc.clone()
	}
	return Account{}
}

// Epoch is the storage-proof epoch of a height.
func (s *State) Epoch(h uint64) uint64 { return h / s.P.ProofEpoch }

// BucketExpiry is the height at which a bucket expires (0 for permanent).
func (s *State) BucketExpiry(id uint32) uint64 {
	if id == 0 {
		return 0
	}
	return uint64(id) * s.P.BlocksPerMonth
}

// BucketFor picks the bucket for data stored for months from height h.
func (s *State) BucketFor(h uint64, months uint64, perm bool) uint32 {
	if perm {
		return 0
	}
	expiry := h + months*s.P.BlocksPerMonth
	id := (expiry + s.P.BlocksPerMonth - 1) / s.P.BlocksPerMonth
	return uint32(id)
}

// PartitionID packs a bucket and a partition index.
func PartitionID(bucket uint32, idx uint64) uint64 { return uint64(bucket)<<32 | idx }

// SplitPartition unpacks a partition id.
func SplitPartition(id uint64) (uint32, uint64) { return uint32(id >> 32), id & 0xFFFFFFFF }

// PartitionRange returns [start, start+length) positions of a partition.
func (s *State) PartitionRange(id uint64) (bucket uint32, start, length uint64, ok bool) {
	bucket, idx := SplitPartition(id)
	b, exists := s.Buckets[bucket]
	if !exists {
		return 0, 0, 0, false
	}
	start = idx * s.P.PartitionChunks
	if start >= b.Len {
		return 0, 0, 0, false
	}
	length = s.P.PartitionChunks
	if b.Len-start < length {
		length = b.Len - start
	}
	return bucket, start, length, true
}

// Partitions lists every partition that currently exists.
func (s *State) Partitions() []uint64 {
	var out []uint64
	ids := make([]uint32, 0, len(s.Buckets))
	for id := range s.Buckets {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		b := s.Buckets[id]
		for i := uint64(0); i*s.P.PartitionChunks < b.Len; i++ {
			out = append(out, PartitionID(id, i))
		}
	}
	return out
}

// Locate finds the record holding a position of a bucket.
func (s *State) Locate(bucket uint32, pos uint64) (*Record, uint64, bool) {
	b, ok := s.Buckets[bucket]
	if !ok || pos >= b.Len {
		return nil, 0, false
	}
	i := sort.Search(len(b.Records), func(i int) bool { return b.Records[i].Start+b.Records[i].Chunks > pos })
	if i >= len(b.Records) {
		return nil, 0, false
	}
	r := &b.Records[i]
	if pos < r.Start {
		return nil, 0, false
	}
	return r, pos - r.Start, true
}

// NeedBytes is the raw storage the live media needs.
func (s *State) NeedBytes() uint64 {
	return (s.PermBytes + s.TermBytes) * s.P.MediaReplicas
}

// MediaCapPerBlock is the automatic media cap: half of the proven storage,
// divided by the replication factor, minus what is stored, spread over a year.
func (s *State) MediaCapPerBlock() uint64 {
	usable := mulDiv(s.Proven, s.P.MediaHeadroomPerm, 1000) / s.P.MediaReplicas
	stored := s.PermBytes + s.TermBytes
	var c uint64
	if usable > stored {
		c = (usable - stored) / s.P.BlocksPerYear
	}
	if c < s.P.MinMediaPerBlock {
		c = s.P.MinMediaPerBlock
	}
	return c
}

// FileCap is the largest single upload.
func (s *State) FileCap() uint64 {
	return s.MediaCapPerBlock() * s.P.BlocksPerDay / s.P.FileCapDivisor
}

func vestedAt(t Tranche, h uint64) uint64 {
	if h >= t.End {
		return t.Amount
	}
	if h <= t.Start {
		return 0
	}
	return mulDiv(t.Amount, h-t.Start, t.End-t.Start)
}

// Claimable is vested income not yet claimed.
func (s *State) Claimable(a *Account, h uint64) uint64 {
	var c uint64
	for _, t := range a.Vesting {
		c += vestedAt(t, h) - t.Claimed
	}
	return c
}

// Locked is income still vesting.
func (s *State) Locked(a *Account, h uint64) uint64 {
	var c uint64
	for _, t := range a.Vesting {
		c += t.Amount - vestedAt(t, h)
	}
	return c
}

// Genesis builds the state at height 0, including the genesis data that lets
// mining start before anyone has uploaded anything.
func Genesis(p *params.Params) (*State, []byte) {
	s := &State{
		P:          p,
		Accounts:   map[Address]*Account{},
		Buckets:    map[uint32]*Bucket{},
		Data:       map[Hash]*DataInfo{},
		Posts:      map[Hash]*PostInfo{},
		Likes:      map[[52]byte]bool{},
		dups:       map[Hash]dupEntry{},
		Price:      p.InitialPrice,
		PriceEMA:   p.InitialPrice,
		BaseFee:    p.BaseFeeInit,
		Congestion: 1000,
		CumWork:    new(big.Int),
		Members:    map[Hash]*Member{},
		RLNTree:    rln.NewTree(),
		RLNNulls:   map[uint64]map[Hash]Share{},
		RLNCap:     p.RLNMaxLimit,
		RLNCapPrev: p.RLNMaxLimit,
	}
	s.RLNCapEpoch = s.RLNEpoch(1)
	s.RLNRoots = []RootEntry{{Root: s.RLNRoot()}}
	data := GenesisData(p)
	root, _ := chunk.Root(data)
	n := chunk.Count(uint64(len(data)))
	s.Buckets[0] = &Bucket{ID: 0, Len: n, EpochLen: n, Records: []Record{{DataRoot: root, Size: uint64(len(data)), Start: 0, Chunks: n}}, Bytes: uint64(len(data))}
	s.Data[root] = &DataInfo{Bucket: 0, Size: uint64(len(data)), Media: types.MediaOther, Tier: params.TierPermanent, Record: 0}
	s.VDFOut = cryptox.Tagged("cb/genesis/vdf", []byte(p.Name))
	s.Tip = GenesisHash(p)
	s.Recent = []HeaderMeta{{Time: GenesisTime, Target: types.HashToTarget(p.GenesisTarget)}}
	s.Proven = ProvenBytes(p, types.Work(p.GenesisTarget), 1)
	return s, data
}

// GenesisTime is the timestamp of every network's genesis.
const GenesisTime = 1_793_491_200 // 2026-11-01T00:00:00Z

// GenesisHash identifies the genesis block of a network.
func GenesisHash(p *params.Params) Hash {
	return cryptox.Tagged("cb/genesis", []byte(p.Name), cryptox.U64(uint64(p.Magic)))
}

// GenesisData is deterministic filler data everyone can regenerate.
func GenesisData(p *params.Params) []byte {
	key := cryptox.Tagged("cb/genesis/data", []byte(p.Name))
	block, _ := aes.NewCipher(key[:])
	out := make([]byte, p.GenesisDataChunks*params.ChunkSize)
	cipher.NewCTR(block, make([]byte, 16)).XORKeyStream(out, out)
	return out
}

// CheckSupply verifies that every unit of supply is accounted for.
func (s *State) CheckSupply() error {
	var total uint64
	for _, a := range s.Accounts {
		total += a.Balance + a.StakeMature + a.StakePending
		for _, u := range a.Unbonding {
			total += u.Amount
		}
		for _, t := range a.Vesting {
			total += t.Amount - t.Claimed
		}
	}
	for _, b := range s.Buckets {
		total += b.Escrow
	}
	for _, m := range s.Members {
		total += m.Bond
	}
	total += s.Endowment + s.RepairPool
	if total != s.Supply {
		return fmt.Errorf("supply mismatch: accounted %d, supply %d", total, s.Supply)
	}
	return nil
}
