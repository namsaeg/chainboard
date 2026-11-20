// Package miner packs the partitions this node keeps and mines on them: it
// runs the VDF, checks the challenges each step unlocks, and builds a block
// when a packed chunk meets the weighted target.
package miner

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/namsaeg/chainboard/internal/chain"
	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/mempool"
	"github.com/namsaeg/chainboard/internal/pack"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/store"
	"github.com/namsaeg/chainboard/internal/types"
	"github.com/namsaeg/chainboard/internal/vdf"
)

// Miner mines with one key on the partitions that fit in its pledge.
type Miner struct {
	Chain       *chain.Chain
	Pool        *mempool.Pool
	Store       *store.Store
	Key         *cryptox.Key
	PledgeBytes uint64
	OnBlock     func(*types.Block)
	Logf        func(format string, args ...any)
	Now         func() int64 // unix time source (tests use a synthetic clock)

	mu       sync.Mutex
	assigned []uint64
	stats    Stats
}

// Stats is shown in the UI.
type Stats struct {
	Assigned   int    `json:"assigned_partitions"`
	Packed     uint64 `json:"packed_chunks"`
	Missing    uint64 `json:"missing_chunks"`
	Steps      uint64 `json:"vdf_steps"`
	Checks     uint64 `json:"checks"`
	Blocks     uint64 `json:"blocks_found"`
	LastResult string `json:"last_result"`
}

func (m *Miner) logf(f string, a ...any) {
	if m.Logf != nil {
		m.Logf(f, a...)
	} else {
		log.Printf(f, a...)
	}
}

// Stats returns a snapshot of mining statistics.
func (m *Miner) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

// Assigned returns the partitions this node keeps.
func (m *Miner) Assigned() []uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]uint64(nil), m.assigned...)
}

// Assign picks partitions within the pledge: permanent data first, then the
// longest-lived buckets, which is also what earns the most mining weight.
func Assign(st *state.State, pledge uint64) []uint64 {
	parts := st.Partitions()
	sort.SliceStable(parts, func(i, j int) bool {
		bi, _ := state.SplitPartition(parts[i])
		bj, _ := state.SplitPartition(parts[j])
		if (bi == 0) != (bj == 0) {
			return bi == 0
		}
		return bi > bj
	})
	var out []uint64
	var used uint64
	for _, p := range parts {
		_, _, length, _ := st.PartitionRange(p)
		size := length * params.ChunkSize
		if used+size > pledge && len(out) > 0 {
			break
		}
		out = append(out, p)
		used += size
	}
	return out
}

// packBatch packed replicas (8 MiB) are written per database transaction.
const packBatch = 32

// Prepare reassigns partitions and packs every chunk that is stored raw but
// not yet packed. It returns the data roots that still miss chunks.
func (m *Miner) Prepare(st *state.State) map[cryptox.Hash][]uint64 {
	parts := Assign(st, m.PledgeBytes)
	addr := m.Key.Address()
	missing := map[cryptox.Hash][]uint64{}
	var packed, miss uint64
	var batch []store.Packed
	flush := func() {
		if len(batch) > 0 && m.Store.PutPacked(addr, batch...) == nil {
			packed += uint64(len(batch))
		}
		batch = batch[:0]
	}
	for _, p := range parts {
		bucket, start, length, ok := st.PartitionRange(p)
		if !ok {
			continue
		}
		for pos := start; pos < start+length; pos++ {
			if m.Store.HasPacked(addr, bucket, pos) {
				packed++
				continue
			}
			rec, ci, ok := st.Locate(bucket, pos)
			if !ok || rec.Moved {
				continue
			}
			raw, err := m.Store.Chunk(rec.DataRoot, ci)
			if err != nil {
				missing[rec.DataRoot] = append(missing[rec.DataRoot], ci)
				miss++
				continue
			}
			seed := pack.Seed(st.P, bucket, pos, addr)
			batch = append(batch, store.Packed{Bucket: bucket, Position: pos, Data: pack.Pack(seed, raw)})
			if len(batch) == packBatch {
				flush()
			}
		}
	}
	flush()
	m.mu.Lock()
	old := m.assigned
	m.assigned = parts
	m.stats.Assigned, m.stats.Packed, m.stats.Missing = len(parts), packed, miss
	m.mu.Unlock()
	// Replicas of buckets that expired are no longer needed.
	for _, p := range old {
		if b, _ := state.SplitPartition(p); b != 0 {
			if _, live := st.Buckets[b]; !live {
				_ = m.Store.DropBucket(addr, b)
			}
		}
	}
	return missing
}

// Run mines until ctx ends. Each new tip restarts the VDF from that tip.
func (m *Miner) Run(ctx context.Context) {
	tips := m.Chain.Subscribe()
	for ctx.Err() == nil {
		tip := m.Chain.Tip()
		m.Prepare(tip)
		m.mineOn(ctx, tip, tips)
	}
}

func (m *Miner) mineOn(ctx context.Context, tip *state.State, tips <-chan *state.State) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	target := tip.ExpectedTarget()
	clock := m.startVDF(ctx, vdf.Reseed(tip.VDFOut, tip.Tip), tip.P.VDFIterations)
	parts := m.Assigned()
	weave := tip.WeaveChunks()
	done := 0 // steps whose recall ranges have been checked
	for {
		select {
		case <-ctx.Done():
			return
		case <-tips:
			return
		case <-clock.ready:
		}
		outputs := clock.sofar()
		for ; done < len(outputs); done++ {
			for _, p := range parts {
				c, packed, ok := m.search(tip, outputs[done], p, weave, target)
				if !ok {
					continue
				}
				b, err := m.build(tip, target, outputs[:done+1], p, c, packed)
				if err != nil {
					m.logf("miner: build block: %v", err)
					continue
				}
				if err := m.Chain.AddOwn(b); err != nil {
					m.setResult("rejected: " + err.Error())
					m.logf("miner: own block rejected: %v", err)
					return
				}
				m.mu.Lock()
				m.stats.Blocks++
				m.stats.LastResult = "found block " + b.Hash().String()[:12]
				m.mu.Unlock()
				if m.OnBlock != nil {
					m.OnBlock(b)
				}
				return
			}
		}
	}
}

// search reads the recall range one VDF output unlocks in a partition (one
// sequential read) and checks every sub-chunk of it against the target.
func (m *Miner) search(tip *state.State, out cryptox.Hash, part, weave uint64, target *big.Int) (state.Challenge, []byte, bool) {
	addr := m.Key.Address()
	rr, ok := tip.RangeFor(out, part, addr, tip.Height+1, weave)
	if !ok {
		return state.Challenge{}, nil, false
	}
	replicas, err := m.Store.ReadPacked(addr, rr.Bucket, rr.Start, rr.End-rr.Start)
	if err != nil {
		return state.Challenge{}, nil, false
	}
	checks := uint64(0)
	defer func() { m.addChecks(checks) }()
	for i, rep := range replicas {
		if rep == nil {
			continue
		}
		pos := rr.Start + uint64(i)
		for sub := 0; sub < params.ChunkSize/params.SubChunkSize; sub++ {
			c, ok := tip.Ticket(rr, pos, sub)
			if !ok {
				break // past the end of the record's data
			}
			packed := rep[sub*params.SubChunkSize : (sub+1)*params.SubChunkSize]
			checks++
			if state.MeetsTarget(state.SolutionHash(c, packed), target, c.Weight) {
				return c, append([]byte(nil), packed...), true
			}
		}
	}
	return state.Challenge{}, nil, false
}

// vdfClock runs the VDF in its own goroutine, so storage reads for the
// challenges never slow the clock: a miner's challenge rate is set by its
// VDF speed alone, and checks only need to keep up on average.
type vdfClock struct {
	mu    sync.Mutex
	outs  []cryptox.Hash
	ready chan struct{} // signalled after each step (coalesced)
}

func (m *Miner) startVDF(ctx context.Context, in cryptox.Hash, iterations uint64) *vdfClock {
	c := &vdfClock{ready: make(chan struct{}, 1)}
	go func() {
		for ctx.Err() == nil {
			in = vdf.Step(in, iterations)
			c.mu.Lock()
			c.outs = append(c.outs, in)
			c.mu.Unlock()
			m.mu.Lock()
			m.stats.Steps++
			m.mu.Unlock()
			select {
			case c.ready <- struct{}{}:
			default:
			}
		}
	}()
	return c
}

// sofar returns the outputs computed so far. Later steps only append past
// its end, so the caller may read it without the lock.
func (c *vdfClock) sofar() []cryptox.Hash {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.outs[:len(c.outs):len(c.outs)]
}

func (m *Miner) addChecks(n uint64) {
	m.mu.Lock()
	m.stats.Checks += n
	m.mu.Unlock()
}

func (m *Miner) setResult(s string) {
	m.mu.Lock()
	m.stats.LastResult = s
	m.mu.Unlock()
}

func (m *Miner) build(tip *state.State, target *big.Int, outputs []cryptox.Hash,
	partition uint64, c state.Challenge, packed []byte) (*types.Block, error) {
	path, err := m.Store.SubProof(c.Rec.DataRoot, c.ChunkIdx, c.Sub)
	if err != nil {
		return nil, err
	}
	txs := m.Pool.Template(tip)
	now := time.Now().Unix()
	if m.Now != nil {
		now = m.Now()
	}
	if med := tip.MedianTime(); now <= med {
		now = med + 1
	}
	b := &types.Block{Txs: txs}
	hd := &b.Header
	bundles, err := m.sealBundles(tip, txs)
	if err != nil {
		return nil, err
	}
	hd.TextBundles = bundles
	hd.Version = 1
	hd.Height = tip.Height + 1
	hd.Prev = tip.Tip
	hd.Time = now
	target.FillBytes(hd.Target[:])
	hd.TxRoot = types.TxRoot(txs)
	hd.BoardRoot = types.BoardRoot(txs)
	hd.VDFStep = tip.VDFStep + uint64(len(outputs))
	hd.VDFOutputs = append([]cryptox.Hash(nil), outputs...)
	hd.Sol = types.Solution{Partition: partition, Position: c.Position, Sub: uint8(c.Sub),
		PackedSub: packed, Path: chunk.Proof{SubPath: path.SubPath, ChunkPath: path.ChunkPath}}
	hd.Miner = append([]byte(nil), m.Key.Pub...)
	if hd.StateRoot, err = tip.NextRoot(b); err != nil {
		return nil, err
	}
	sh := hd.SigHash()
	hd.Sig = m.Key.Sign(sh[:])
	return b, nil
}

// sealBundles builds the text bundles of a seal block from the bodies in the
// store, and keeps each bundle as data so it can be served and packed.
func (m *Miner) sealBundles(tip *state.State, txs []*types.Tx) ([]types.TextBundle, error) {
	h := tip.Height + 1
	if !tip.SealsAt(h) {
		return nil, nil
	}
	st := tip.Clone()
	ctx := st.NewCtx()
	for _, t := range txs {
		if err := st.ApplyTx(t, ctx); err != nil {
			return nil, fmt.Errorf("template tx no longer applies: %w", err)
		}
	}
	var out []types.TextBundle
	for _, spec := range st.PendingBundles(h) {
		data, missing := state.BundleBytes(spec, m.Store.Text)
		if len(missing) > 0 {
			return nil, fmt.Errorf("seal needs %d post bodies this node lacks", len(missing))
		}
		root, err := m.Store.PutData(data)
		if err != nil {
			return nil, err
		}
		out = append(out, types.TextBundle{Bucket: spec.Bucket, Root: root, Size: spec.Size})
	}
	return out, nil
}

// EpochProof builds this epoch's storage proof transaction, if one is due.
func (m *Miner) EpochProof(st *state.State, nonce uint64) (*types.Tx, bool) {
	addr := m.Key.Address()
	acc := st.Peek(addr)
	epoch := st.Epoch(st.Height + 1)
	if len(acc.Partitions) == 0 || acc.LastProof >= epoch || acc.ProofsInEpoch == epoch+1 {
		return nil, false
	}
	c, part, ok := st.EpochChallenge(addr)
	if !ok || !m.Store.HasPacked(addr, c.Bucket, c.Position) {
		return nil, false
	}
	packed, err := m.Store.PackedSub(addr, c.Bucket, c.Position, c.Sub)
	if err != nil {
		return nil, false
	}
	path, err := m.Store.SubProof(c.Rec.DataRoot, c.ChunkIdx, c.Sub)
	if err != nil {
		return nil, false
	}
	t := &types.Tx{Kind: types.TxProof, Nonce: nonce, Lane: types.LaneFee,
		Proof: &types.StorageProof{Epoch: epoch, Partition: part, Position: c.Position, Sub: uint8(c.Sub), PackedSub: packed, Path: path}}
	t.MaxFee = max(st.BaseFee, st.PermByteFee()) * 2 * uint64(t.EncodedSize()+200)
	t.Sign(m.Key)
	return t, true
}
