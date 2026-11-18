// Package mempool holds valid transactions waiting for a block. Every
// transaction is kept only while it applies cleanly, in arrival order, on top
// of the current tip; uploads wait until all their chunks are stored.
package mempool

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

type Hash = cryptox.Hash

const maxPool = 20_000

// DataCheck tells whether an upload's data is fully stored and acceptable.
type DataCheck func(t *types.Tx) error

// Pool is safe for concurrent use.
type Pool struct {
	mu      sync.Mutex
	txs     map[Hash]*item
	check   DataCheck
	tip     *state.State
	pending *state.State // tip with all pool txs applied
	ctx     *state.BlockCtx
	seq     uint64
}

type item struct {
	tx   *types.Tx
	seen time.Time
	seq  uint64
}

// New creates a pool on top of a tip.
func New(tip *state.State, check DataCheck) *Pool {
	p := &Pool{txs: map[Hash]*item{}, check: check}
	p.Reset(tip)
	return p
}

// ErrWaitingData means the upload is valid but its chunks are not all here.
var ErrWaitingData = errors.New("mempool: waiting for data chunks")

// Add validates a transaction against the pending state and keeps it.
func (p *Pool) Add(t *types.Tx) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := t.ID()
	if _, ok := p.txs[id]; ok {
		return nil
	}
	if len(p.txs) >= maxPool {
		return errors.New("mempool: full")
	}
	if t.Kind == types.TxUpload && p.check != nil {
		if err := p.check(t); err != nil {
			return err
		}
	}
	if err := p.pending.ApplyTx(t, p.ctx); err != nil { // atomic: no change on error
		return err
	}
	p.seq++
	p.txs[id] = &item{tx: t, seen: time.Now(), seq: p.seq}
	return nil
}

// Has reports whether a tx is pooled.
func (p *Pool) Has(id Hash) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.txs[id]
	return ok
}

// Get returns a pooled tx.
func (p *Pool) Get(id Hash) *types.Tx {
	p.mu.Lock()
	defer p.mu.Unlock()
	if it, ok := p.txs[id]; ok {
		return it.tx
	}
	return nil
}

func (p *Pool) ordered() []*item {
	items := make([]*item, 0, len(p.txs))
	for _, it := range p.txs {
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].seq < items[j].seq })
	return items
}

// Reset re-validates the pool on a new tip, dropping what no longer applies
// (mined, conflicting or expired).
func (p *Pool) Reset(tip *state.State) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tip = tip
	p.pending = tip.Clone()
	p.ctx = p.pending.NewCtx()
	for _, it := range p.ordered() {
		if err := p.pending.ApplyTx(it.tx, p.ctx); err != nil {
			delete(p.txs, it.tx.ID())
		}
	}
}

// Template returns transactions that apply in order on top of tip, within the
// block's lane and media limits.
func (p *Pool) Template(tip *state.State) []*types.Tx {
	p.mu.Lock()
	items := p.ordered()
	p.mu.Unlock()
	st := tip.Clone()
	ctx := st.NewCtx()
	rewardCap := tip.BlockReward(ctx.H)
	var out []*types.Tx
	for _, it := range items {
		if it.tx.Lane == types.LaneFree &&
			ctx.FreeStorage+state.TextStorageFee(st.P, st.Price, it.tx.EncodedSize()) > rewardCap {
			continue // free-lane storage is paid from this block's issuance
		}
		if st.ApplyTx(it.tx, ctx) == nil {
			out = append(out, it.tx)
		}
	}
	return out
}

// Pending returns pooled transactions in arrival order (for "unconfirmed" views).
func (p *Pool) Pending() []*types.Tx {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*types.Tx
	for _, it := range p.ordered() {
		out = append(out, it.tx)
	}
	return out
}

// PendingState returns a copy of the tip with the pool applied.
func (p *Pool) PendingState() *state.State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pending.Clone()
}

// Account reads one account from the pending state (cheap, no copy of the state).
func (p *Pool) Account(a state.Address) state.Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pending.Peek(a)
}

// Len is the number of pooled transactions.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.txs)
}
