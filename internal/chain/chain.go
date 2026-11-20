// Package chain keeps every valid block, picks the heaviest chain, and serves
// the state at its tip. Reorganisations are handled by recomputing state from
// the nearest saved snapshot.
package chain

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
	"github.com/namsaeg/chainboard/internal/vdf"
)

type Hash = cryptox.Hash

var (
	ErrKnown   = errors.New("chain: block already known")
	ErrOrphan  = errors.New("chain: parent unknown")
	ErrInvalid = errors.New("chain: invalid block")
)

const (
	snapshotEvery = 50   // keep a state snapshot every N heights
	recentStates  = 24   // and for the last N blocks of the best chain
	memoryDepth   = 2048 // snapshots and block bodies are kept in memory this deep
	maxOrphans    = 512  // blocks waiting for their parent
)

type entry struct {
	block  *types.Block // nil once evicted to the database (see memoryDepth)
	height uint64
	work   *big.Int
	parent Hash
}

// TextSource gives the post bodies a node holds.
type TextSource interface {
	Text(Hash) ([]byte, bool)
}

// MissingTextsError: a recent block cannot be accepted until these bodies
// are here. The block is not invalid; it waits.
type MissingTextsError struct{ Hashes []Hash }

func (e *MissingTextsError) Error() string {
	return fmt.Sprintf("chain: waiting for %d post bodies", len(e.Hashes))
}

// DataSource tells whether all chunks of a data root are here, each
// verified against the root.
type DataSource interface {
	Complete(root Hash) bool
}

// MissingDataError: a recent block stores paid data (a fund store or a
// bounty reveal) whose chunks are not all here. Like MissingTextsError, the
// block is not invalid; it waits until the data arrives.
type MissingDataError struct {
	Root Hash
	Size uint64
}

func (e *MissingDataError) Error() string {
	return fmt.Sprintf("chain: waiting for the data %x", e.Root[:8])
}

// TextWindow is how recent (by block time) a block must be for this node to
// demand its post bodies. Bodies of older blocks may have expired; they are
// fetched for display when wanted, not for validation.
const TextWindow = 24 * 3600

// Chain is safe for concurrent use.
type Chain struct {
	P           *params.Params
	GenesisData []byte
	// SetAside names the database of another genesis moved away by Open.
	SetAside string
	// Texts, if set, holds the bodies that recent blocks must come with: a
	// node accepts a recent block only with all of its bodies, and a seal
	// block only if its bundles match them.
	Texts TextSource
	// Data, if set, holds data chunks: a node accepts a recent block that
	// stores paid data (fund stores, bounty reveals) only once all of that
	// data is here. A payout so never happens for data that is not there.
	Data DataSource
	// MemoryDepth overrides memoryDepth (tests).
	MemoryDepth uint64
	// TextWindowSec overrides TextWindow (tests).
	TextWindowSec int64
	// KeepBundle, if set, receives each text bundle a seal check rebuilt,
	// so the node can serve its chunks to miners right away.
	KeepBundle func([]byte)
	// Logf receives background verification results (optional).
	Logf func(string, ...any)
	// SampleSteps overrides how many VDF steps a sampled block gets
	// checked on arrival (tests; -1 checks none).
	SampleSteps int
	// FullCheckSec overrides vdfFullAge (tests).
	FullCheckSec int64

	mu       sync.RWMutex
	db       *bolt.DB // nil for an in-memory chain
	genesis  Hash
	blocks   map[Hash]*entry
	states   map[Hash]*state.State
	bad      map[Hash]error
	orphans  map[Hash][]*types.Block // by missing parent
	best     Hash
	evicted  uint64 // best-chain bodies at or below this height are not in memory
	bestPath []Hash // hash by height on the best chain
	subs     []chan *state.State

	// Blocks whose VDF was only sampled, waiting for the full check, and
	// whether this node verifies too slowly to fully check new blocks.
	unverified []Hash
	slow       bool
	wake       chan struct{}
}

// VDF checking. A full check costs as much hashing as the block's VDF took
// to compute (about a block time of one fast core), spread over this
// node's cores. History (blocks older than vdfHistoryAge by their time) and,
// on a node too slow to keep up, new blocks too are accepted after checking
// vdfSampleSteps random steps. Blocks of the last vdfFullAge are then
// checked in full in the background, and one that fails is removed with its
// descendants. Older history stays sampled: checking it all would take as
// long as the chain took to make. A forged step in each of many blocks is
// still caught almost surely (each block's sample hits it with about
// vdfSampleSteps/steps).
const (
	vdfHistoryAge  = 6 * 3600
	vdfFullAge     = 7 * 24 * 3600
	vdfSampleSteps = 4
)

var (
	bBlocks  = []byte("blocks")  // block hash -> encoded block
	bHeights = []byte("heights") // height || block hash -> nil, for loading in order
	bInfo    = []byte("info")    // "genesis" -> genesis hash of the stored chain
	bPending = []byte("vdf")     // block hash -> nil: VDF only sampled so far
)

// Open loads (or creates) a chain in dir/chain.db. An empty dir keeps the
// chain in memory only.
func Open(p *params.Params, dir string) (*Chain, error) {
	g, data := state.Genesis(p)
	c := &Chain{
		P: p, GenesisData: data, genesis: g.Tip,
		blocks:  map[Hash]*entry{g.Tip: {height: 0, work: new(big.Int)}},
		states:  map[Hash]*state.State{g.Tip: g},
		bad:     map[Hash]error{},
		orphans: map[Hash][]*types.Block{},
		best:    g.Tip, bestPath: []Hash{g.Tip},
		wake: make(chan struct{}, 1),
	}
	if dir == "" {
		return c, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := openFor(filepath.Join(dir, "chain.db"), g.Tip, &c.SetAside)
	if err != nil {
		return nil, err
	}
	c.db = db
	if err := indexHeights(db); err != nil {
		db.Close()
		return nil, err
	}
	// Stream the stored blocks in height order: only recent bodies stay in
	// memory, so start-up does not hold the whole chain.
	var keys [][]byte
	_ = db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bHeights).ForEach(func(k, _ []byte) error {
			keys = append(keys, append([]byte(nil), k...))
			return nil
		})
	})
	for _, k := range keys {
		var raw []byte
		_ = db.View(func(tx *bolt.Tx) error {
			raw = append([]byte(nil), tx.Bucket(bBlocks).Get(k[8:])...)
			return nil
		})
		if blk, err := types.DecodeBlock(raw); err == nil {
			// Invalid stored blocks are ignored; the network refills gaps.
			_ = c.add(blk, 0, addReplay)
		}
	}
	// Blocks whose VDF was only sampled before the last shutdown are
	// checked again in the background.
	_ = db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bPending).ForEach(func(k, _ []byte) error {
			var h Hash
			copy(h[:], k)
			if _, ok := c.blocks[h]; ok {
				c.unverified = append(c.unverified, h)
			}
			return nil
		})
	})
	return c, nil
}

// indexHeights creates the buckets and, for a database written before the
// height index existed, builds it once.
func indexHeights(db *bolt.DB) error {
	return db.Update(func(tx *bolt.Tx) error {
		blocks, err := tx.CreateBucketIfNotExists(bBlocks)
		if err != nil {
			return err
		}
		heights, err := tx.CreateBucketIfNotExists(bHeights)
		if err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(bPending); err != nil {
			return err
		}
		if k, _ := heights.Cursor().First(); k != nil {
			return nil
		}
		return blocks.ForEach(func(h, v []byte) error {
			if blk, err := types.DecodeBlock(v); err == nil {
				return heights.Put(heightKey(blk.Height, h), nil)
			}
			return nil
		})
	})
}

func heightKey(height uint64, hash []byte) []byte {
	return append(binary.BigEndian.AppendUint64(nil, height), hash...)
}

// blockOf returns an entry's block, reading it back from the database if it
// was evicted from memory. c.mu must be held.
func (c *Chain) blockOf(e *entry, h Hash) *types.Block {
	if e.block != nil || c.db == nil {
		return e.block
	}
	var b *types.Block
	_ = c.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bBlocks).Get(h[:]); v != nil {
			b, _ = types.DecodeBlock(v)
		}
		return nil
	})
	return b
}

// Close closes the block database.
// openFor opens the chain database for this genesis. A database written
// for another genesis (an older testnet) is renamed out of the way, and
// its new name is reported in setAside.
func openFor(path string, genesis Hash, setAside *string) (*bolt.DB, error) {
	open := func() (*bolt.DB, error) {
		db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
		if err != nil {
			return nil, fmt.Errorf("chain: %w", err)
		}
		return db, nil
	}
	db, err := open()
	if err != nil {
		return nil, err
	}
	var other []byte
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bInfo)
		if err != nil {
			return err
		}
		switch g := b.Get([]byte("genesis")); {
		case g == nil:
			return b.Put([]byte("genesis"), genesis[:])
		case !bytes.Equal(g, genesis[:]):
			other = append([]byte(nil), g...)
		}
		return nil
	})
	if err != nil || other == nil {
		if err != nil {
			db.Close()
		}
		return db, err
	}
	db.Close()
	old := fmt.Sprintf("%s.%x.old", path, other[:4])
	if err := os.Rename(path, old); err != nil {
		return nil, fmt.Errorf("chain: setting aside a database of another network: %w", err)
	}
	*setAside = old
	return openFor(path, genesis, setAside)
}

func (c *Chain) Close() error {
	if c.db == nil {
		return nil
	}
	return c.db.Close()
}

// Genesis returns the genesis hash.
func (c *Chain) Genesis() Hash { return c.genesis }

// Tip returns the state at the best block. Callers must not modify it.
func (c *Chain) Tip() *state.State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.states[c.best]
}

// Height of the best chain.
func (c *Chain) Height() uint64 { return c.Tip().Height }

// Subscribe returns a channel that receives the new tip state on every change.
func (c *Chain) Subscribe() <-chan *state.State {
	ch := make(chan *state.State, 4)
	c.mu.Lock()
	c.subs = append(c.subs, ch)
	c.mu.Unlock()
	return ch
}

// Has reports whether a block is known.
func (c *Chain) Has(h Hash) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.blocks[h]
	return ok
}

// Block returns a known block.
func (c *Chain) Block(h Hash) *types.Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if e, ok := c.blocks[h]; ok {
		return c.blockOf(e, h)
	}
	return nil
}

// BlockAt returns the best-chain block at a height.
func (c *Chain) BlockAt(height uint64) *types.Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if height == 0 || height >= uint64(len(c.bestPath)) {
		return nil
	}
	h := c.bestPath[height]
	return c.blockOf(c.blocks[h], h)
}

// Locator lists best-chain hashes, dense near the tip and sparse further back.
func (c *Chain) Locator() []Hash {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []Hash
	step := 1
	for i := len(c.bestPath) - 1; i >= 0; i -= step {
		out = append(out, c.bestPath[i])
		if len(out) > 10 {
			step *= 2
		}
	}
	if out[len(out)-1] != c.genesis {
		out = append(out, c.genesis)
	}
	return out
}

// After returns up to max best-chain blocks after the first locator hash we
// share with the caller.
func (c *Chain) After(locator []Hash, max int) []*types.Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	start := uint64(0)
	for _, h := range locator {
		e, ok := c.blocks[h]
		if ok && e.height < uint64(len(c.bestPath)) && c.bestPath[e.height] == h {
			start = e.height
			break
		}
	}
	var out []*types.Block
	for i := start + 1; i < uint64(len(c.bestPath)) && len(out) < max; i++ {
		h := c.bestPath[i]
		out = append(out, c.blockOf(c.blocks[h], h))
	}
	return out
}

// Add validates a block and attaches it. It returns ErrOrphan when the parent
// is missing (the block is kept until the parent arrives).
func (c *Chain) Add(b *types.Block) error {
	return c.add(b, time.Now().Unix(), addNet)
}

// AddOwn adds a block this node mined: its VDF and storage proof were made
// here, so they are not checked again; everything else is.
func (c *Chain) AddOwn(b *types.Block) error {
	return c.add(b, time.Now().Unix(), addOwn)
}

type addMode int

const (
	addNet    addMode = iota // from the network: checked in full (VDF maybe sampled)
	addReplay                // read back from our own database
	addOwn                   // mined here
)

func (c *Chain) add(b *types.Block, now int64, mode addMode) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.addLocked(b, now, mode)
	if err != nil {
		return err
	}
	// Attach any orphans that were waiting for this block.
	queue := []Hash{b.Hash()}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		kids := c.orphans[h]
		delete(c.orphans, h)
		for _, k := range kids {
			if c.addLocked(k, now, mode) == nil {
				queue = append(queue, k.Hash())
			}
		}
	}
	return nil
}

func (c *Chain) addLocked(b *types.Block, now int64, mode addMode) error {
	persist := mode != addReplay
	h := b.Hash()
	if _, ok := c.blocks[h]; ok {
		return ErrKnown
	}
	if err, ok := c.bad[h]; ok {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	pe, ok := c.blocks[b.Prev]
	if !ok {
		if len(c.orphans) < maxOrphans {
			c.orphans[b.Prev] = append(c.orphans[b.Prev], b)
		}
		return ErrOrphan
	}
	if _, bad := c.bad[b.Prev]; bad {
		c.bad[h] = errors.New("parent invalid")
		return fmt.Errorf("%w: parent invalid", ErrInvalid)
	}
	parent, err := c.stateAt(b.Prev)
	if err != nil {
		return err
	}
	st := parent.Clone()
	apply := func() error { return st.ApplyBlock(b, now) }
	recheck := false
	switch {
	case mode == addOwn:
		st.SetVDFCheck(-1)
	case mode == addNet && (c.slow || (now > 0 && b.Time < now-vdfHistoryAge)):
		n := vdfSampleSteps
		if c.SampleSteps != 0 {
			n = c.SampleSteps
		}
		st.SetVDFCheck(n)
		recheck = b.Time >= now-c.fullAge()
	}
	if !persist {
		// Read back from our own database: validated before it was stored.
		apply = func() error { return st.ReplayBlock(b) }
	} else if c.Texts != nil && now > 0 && b.Time >= now-c.textWindow() {
		var missing []Hash
		for _, t := range b.Txs {
			if t.NeedsBody() {
				if _, ok := c.Texts.Text(t.TextHash); !ok {
					missing = append(missing, t.TextHash)
				}
			}
		}
		if len(missing) > 0 {
			return &MissingTextsError{Hashes: missing}
		}
		st.SetTextCheck(c.checkBundle)
		if c.Data != nil {
			st.SetDataCheck(c.checkData)
		}
	}
	began := time.Now()
	err = apply()
	took := time.Since(began)
	st.SetTextCheck(nil)
	st.SetDataCheck(nil)
	var mt *MissingTextsError
	if errors.As(err, &mt) {
		return mt
	}
	var md *MissingDataError
	if errors.As(err, &md) {
		return md
	}
	if err != nil {
		c.bad[h] = err
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	work := new(big.Int).Add(pe.work, types.Work(b.TargetInt()))
	c.blocks[h] = &entry{block: b, height: b.Height, work: work, parent: b.Prev}
	c.states[h] = st
	if persist && c.db != nil {
		_ = c.db.Update(func(tx *bolt.Tx) error {
			if err := tx.Bucket(bBlocks).Put(h[:], b.Encode()); err != nil {
				return err
			}
			if recheck {
				if err := tx.Bucket(bPending).Put(h[:], nil); err != nil {
					return err
				}
			}
			return tx.Bucket(bHeights).Put(heightKey(b.Height, h[:]), nil)
		})
	}
	if recheck {
		c.unverified = append(c.unverified, h)
		select {
		case c.wake <- struct{}{}:
		default:
		}
	} else if mode == addNet {
		// A node that needs over half a block time to check a new block
		// samples new blocks too, until the backlog is checked.
		c.slow = took > c.P.BlockTime/2
	}
	if work.Cmp(c.blocks[c.best].work) > 0 {
		c.setBest(h)
	}
	c.prune()
	return nil
}

// stateAt returns the state after block h, recomputing it if needed.
func (c *Chain) stateAt(h Hash) (*state.State, error) {
	if s, ok := c.states[h]; ok {
		return s, nil
	}
	var path []*types.Block
	cur := h
	for {
		if s, ok := c.states[cur]; ok {
			st := s.Clone()
			for i := len(path) - 1; i >= 0; i-- {
				if err := st.ReplayBlock(path[i]); err != nil {
					return nil, err
				}
			}
			c.states[h] = st
			return st, nil
		}
		e, ok := c.blocks[cur]
		var b *types.Block
		if ok {
			b = c.blockOf(e, cur)
		}
		if b == nil {
			return nil, errors.New("chain: no snapshot reachable")
		}
		path = append(path, b)
		cur = e.parent
	}
}

// setBest makes h the tip. The best path is spliced at the fork point, so
// this costs the depth of the reorganisation, not the length of the chain.
func (c *Chain) setBest(h Hash) {
	c.best = h
	var add []Hash
	cur := h
	for {
		ce := c.blocks[cur]
		if ce.height < uint64(len(c.bestPath)) && c.bestPath[ce.height] == cur {
			break
		}
		add = append(add, cur)
		cur = ce.parent
	}
	c.bestPath = c.bestPath[:c.blocks[cur].height+1]
	for i := len(add) - 1; i >= 0; i-- {
		c.bestPath = append(c.bestPath, add[i])
	}
	// Bodies of best-chain blocks deeper than memoryDepth live in the
	// database only.
	if c.db != nil {
		tipH := uint64(len(c.bestPath) - 1)
		for ; c.evicted+c.depth() < tipH; c.evicted++ {
			if e := c.blocks[c.bestPath[c.evicted+1]]; e != nil {
				e.block = nil
			}
		}
	}
	st := c.states[h]
	for _, ch := range c.subs {
		select {
		case ch <- st:
		default:
		}
	}
}

// prune drops state snapshots that are neither recent best-chain states nor
// periodic checkpoints, nor tips of side branches.
// prune keeps the genesis state, a snapshot every snapshotEvery heights
// within memoryDepth, and the last recentStates states. A reorganisation
// deeper than memoryDepth replays from genesis: slow, but still correct.
func (c *Chain) prune() {
	tipH := uint64(len(c.bestPath) - 1)
	keep := map[Hash]bool{c.genesis: true}
	lo := uint64(0)
	if tipH > c.depth() {
		lo = tipH - c.depth()
	}
	for i := (lo + snapshotEvery - 1) / snapshotEvery * snapshotEvery; i <= tipH; i += snapshotEvery {
		keep[c.bestPath[i]] = true
	}
	for i := tipH; i+recentStates > tipH; i-- {
		keep[c.bestPath[i]] = true
		if i == 0 {
			break
		}
	}
	for h := range c.states {
		if keep[h] {
			continue
		}
		e := c.blocks[h]
		// Keep recent side-branch states so competing blocks validate cheaply.
		if e != nil && e.height+recentStates > tipH {
			continue
		}
		delete(c.states, h)
	}
}

// BestHash returns the tip hash.
func (c *Chain) BestHash() Hash {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.best
}

// Work returns the cumulative work of the best chain.
func (c *Chain) Work() *big.Int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return new(big.Int).Set(c.blocks[c.best].work)
}

func (c *Chain) depth() uint64 {
	if c.MemoryDepth > 0 {
		return c.MemoryDepth
	}
	return memoryDepth
}

func (c *Chain) textWindow() int64 {
	if c.TextWindowSec > 0 {
		return c.TextWindowSec
	}
	return TextWindow
}

// checkData reports a fund store's or bounty reveal's data missing until
// every chunk is here.
func (c *Chain) checkData(root Hash, size uint64) error {
	if c.Data.Complete(root) {
		return nil
	}
	return &MissingDataError{Root: root, Size: size}
}

// checkBundle rebuilds a seal block's text bundle from our bodies and
// compares its data root with the header's.
func (c *Chain) checkBundle(b state.BundleSpec, root Hash) error {
	data, missing := state.BundleBytes(b, c.Texts.Text)
	if len(missing) > 0 {
		return &MissingTextsError{Hashes: missing}
	}
	if r, _ := chunk.Root(data); r != root {
		return fmt.Errorf("text bundle for bucket %d does not match its bodies", b.Bucket)
	}
	if c.KeepBundle != nil {
		c.KeepBundle(data)
	}
	return nil
}

func (c *Chain) fullAge() int64 {
	if c.FullCheckSec > 0 {
		return c.FullCheckSec
	}
	return vdfFullAge
}

// VerifyLoop checks, in the background, the VDF of blocks that were
// accepted on a sample, oldest first, leaving one core to the rest of the
// node. A block that fails is removed with everything built on it.
func (c *Chain) VerifyLoop(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		for c.verifyOne() {
			if ctx.Err() != nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-tick.C:
		}
	}
}

// VerifyPending fully checks every sampled block now and reports how many
// it checked.
func (c *Chain) VerifyPending() int {
	n := 0
	for c.verifyOne() {
		n++
	}
	return n
}

// verifyOne fully checks the oldest sampled block; false when none waits.
func (c *Chain) verifyOne() bool {
	c.mu.Lock()
	var h Hash
	var b *types.Block
	var start Hash
	for len(c.unverified) > 0 && b == nil {
		h = c.unverified[0]
		c.unverified = c.unverified[1:]
		e, ok := c.blocks[h]
		if !ok {
			continue // already removed
		}
		b = c.blockOf(e, h)
		if b == nil {
			continue
		}
		last := c.states[c.genesis].VDFOut
		if pe := c.blocks[b.Prev]; pe != nil && b.Prev != c.genesis {
			if pb := c.blockOf(pe, b.Prev); pb != nil {
				last = pb.VDFOutputs[len(pb.VDFOutputs)-1]
			}
		}
		start = vdf.Reseed(last, b.Prev)
	}
	if len(c.unverified) == 0 {
		c.slow = false
	}
	c.mu.Unlock()
	if b == nil {
		return false
	}
	idx := make([]int, len(b.VDFOutputs))
	for i := range idx {
		idx[i] = i
	}
	ok := vdf.VerifySteps(start, b.VDFOutputs, c.P.VDFIterations, idx, runtime.NumCPU()-1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok {
		if c.db != nil {
			_ = c.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bPending).Delete(h[:]) })
		}
		return true
	}
	c.invalidate(h, errors.New("VDF outputs fail the full check"))
	return true
}

// invalidate removes a block and its descendants and moves the tip to the
// heaviest block left. c.mu must be held.
func (c *Chain) invalidate(bad Hash, why error) {
	kids := map[Hash][]Hash{}
	for h, e := range c.blocks {
		kids[e.parent] = append(kids[e.parent], h)
	}
	queue := []Hash{bad}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		queue = append(queue, kids[h]...)
		e := c.blocks[h]
		c.bad[h] = why
		delete(c.blocks, h)
		delete(c.states, h)
		if c.db != nil && e != nil {
			_ = c.db.Update(func(tx *bolt.Tx) error {
				_ = tx.Bucket(bPending).Delete(h[:])
				_ = tx.Bucket(bHeights).Delete(heightKey(e.height, h[:]))
				return tx.Bucket(bBlocks).Delete(h[:])
			})
		}
	}
	if c.Logf != nil {
		c.Logf("chain: block %x and its descendants removed: %v", bad[:6], why)
	}
	if _, ok := c.blocks[c.best]; ok {
		return
	}
	best := c.genesis
	for h, e := range c.blocks {
		if e.work.Cmp(c.blocks[best].work) > 0 {
			best = h
		}
	}
	if _, err := c.stateAt(best); err == nil {
		c.setBest(best)
	}
}
