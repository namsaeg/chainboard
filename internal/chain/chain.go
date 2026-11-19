// Package chain keeps every valid block, picks the heaviest chain, and serves
// the state at its tip. Reorganisations are handled by recomputing state from
// the nearest saved snapshot.
package chain

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
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
	// MemoryDepth overrides memoryDepth (tests).
	MemoryDepth uint64
	// TextWindowSec overrides TextWindow (tests).
	TextWindowSec int64
	// KeepBundle, if set, receives each text bundle a seal check rebuilt,
	// so the node can serve its chunks to miners right away.
	KeepBundle func([]byte)

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
}

var (
	bBlocks  = []byte("blocks")  // block hash -> encoded block
	bHeights = []byte("heights") // height || block hash -> nil, for loading in order
	bInfo    = []byte("info")    // "genesis" -> genesis hash of the stored chain
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
			_ = c.add(blk, 0, false)
		}
	}
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
	return c.add(b, time.Now().Unix(), true)
}

func (c *Chain) add(b *types.Block, now int64, persist bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.addLocked(b, now, persist)
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
			if c.addLocked(k, now, persist) == nil {
				queue = append(queue, k.Hash())
			}
		}
	}
	return nil
}

func (c *Chain) addLocked(b *types.Block, now int64, persist bool) error {
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
	}
	err = apply()
	st.SetTextCheck(nil)
	var mt *MissingTextsError
	if errors.As(err, &mt) {
		return mt
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
			return tx.Bucket(bHeights).Put(heightKey(b.Height, h[:]), nil)
		})
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
