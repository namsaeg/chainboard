// Package mempool holds valid transactions waiting for a block. Every
// transaction is kept only while it applies cleanly, in arrival order, on top
// of the current tip; uploads wait until all their chunks are stored.
package mempool

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

type Hash = cryptox.Hash

const maxPool = 20_000

// Orphans: transactions whose sender has an earlier nonce not seen yet.
// Relayed transactions can overtake each other (stem embargoes end in any
// order), so they wait here, bounded, until the gap fills.
const (
	maxOrphans      = 512
	maxSenderOrphan = 16
	orphanTTL       = 10 * time.Minute
)

// ErrOrphan: kept until the sender's earlier transaction arrives.
var ErrOrphan = errors.New("mempool: waiting for an earlier transaction of the sender")

// DataCheck tells whether the data a transaction stores (an upload, a fund
// store, a bounty reveal) is fully here and acceptable.
type DataCheck func(t *types.Tx) error

// Pool is safe for concurrent use.
type Pool struct {
	// OnDoubleSignal, if set, is called (outside the pool lock) when an
	// anonymous post double-signals: the member's secret can be recovered
	// and the member slashed.
	OnDoubleSignal func(*state.DoubleSignal)
	// OnPromote, if set, is called (outside the pool lock) for each orphan
	// that entered the pool once the gap before it filled.
	OnPromote func(*types.Tx)
	// Bodies, if set, keeps the post bodies of accepted transactions, so
	// blocks that include them can be checked and sealed.
	Bodies interface {
		PutText([]byte) (Hash, error)
	}

	mu      sync.Mutex
	txs     map[Hash]*item
	check   DataCheck
	tip     *state.State
	pending *state.State // tip with all pool txs applied
	ctx     *state.BlockCtx
	seq     uint64
	orphans map[cryptox.Address]map[uint64]orphan
	nOrphan int
}

type orphan struct {
	tx *types.Tx
	at time.Time
}

type item struct {
	tx   *types.Tx
	seen time.Time
	seq  uint64
}

// New creates a pool on top of a tip.
func New(tip *state.State, check DataCheck) *Pool {
	p := &Pool{txs: map[Hash]*item{}, check: check, orphans: map[cryptox.Address]map[uint64]orphan{}}
	p.Reset(tip)
	return p
}

// ErrWaitingData means a transaction storing data is valid but its chunks
// are not all here.
var ErrWaitingData = errors.New("mempool: waiting for data chunks")

// Add validates a transaction against the pending state and keeps it.
func (p *Pool) Add(t *types.Tx) error {
	err := p.add(t)
	if err == nil && t.NeedsBody() && p.Bodies != nil {
		if _, err := p.Bodies.PutText(t.Text); err != nil {
			return err
		}
	}
	if errors.Is(err, state.ErrFutureNonce) {
		p.keepOrphan(t)
		return fmt.Errorf("%w (%v)", ErrOrphan, err)
	}
	if err == nil && t.Kind.Signed() {
		p.promote(t.Sender())
	}
	var ds *state.DoubleSignal
	if errors.As(err, &ds) && p.OnDoubleSignal != nil {
		p.OnDoubleSignal(ds)
	}
	return err
}

func (p *Pool) add(t *types.Tx) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := t.ID()
	if _, ok := p.txs[id]; ok {
		return nil
	}
	if len(p.txs) >= maxPool {
		return errors.New("mempool: full")
	}
	if t.NeedsBody() {
		if err := t.CheckBody(); err != nil {
			return fmt.Errorf("mempool: %w", err)
		}
	}
	if t.CarriesData() && p.check != nil {
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
// (mined, conflicting or expired). A pooled anonymous post that now collides
// with a mined one is still reported as a double signal.
func (p *Pool) Reset(tip *state.State) {
	defer p.promoteAll()
	var signals []*state.DoubleSignal
	p.mu.Lock()
	p.tip = tip
	p.pending = tip.Clone()
	p.ctx = p.pending.NewCtx()
	for _, it := range p.ordered() {
		if err := p.pending.ApplyTx(it.tx, p.ctx); err != nil {
			delete(p.txs, it.tx.ID())
			var ds *state.DoubleSignal
			if errors.As(err, &ds) {
				signals = append(signals, ds)
			}
		}
	}
	p.mu.Unlock()
	if p.OnDoubleSignal != nil {
		for _, ds := range signals {
			p.OnDoubleSignal(ds)
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
		if it.tx.Kind == types.TxAnonPost &&
			ctx.FreeStorage+st.AnonTextFee(it.tx) > rewardCap {
			continue // anonymous-lane storage is paid from this block's issuance
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

// TipHash is the block the pool is built on.
func (p *Pool) TipHash() Hash {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tip.Tip
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

// keepOrphan holds a transaction whose sender has an earlier one missing.
func (p *Pool) keepOrphan(t *types.Tx) {
	if t.NeedsBody() && t.CheckBody() != nil {
		return
	}
	from := t.Sender()
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.orphans[from]
	if _, ok := m[t.Nonce]; ok || len(m) >= maxSenderOrphan {
		return
	}
	if p.nOrphan >= maxOrphans {
		p.dropOldestOrphan()
	}
	if m == nil {
		m = map[uint64]orphan{}
		p.orphans[from] = m
	}
	m[t.Nonce] = orphan{tx: t, at: time.Now()}
	p.nOrphan++
}

func (p *Pool) dropOldestOrphan() {
	var who cryptox.Address
	var nonce uint64
	var at time.Time
	for a, m := range p.orphans {
		for n, o := range m {
			if at.IsZero() || o.at.Before(at) {
				who, nonce, at = a, n, o.at
			}
		}
	}
	p.dropOrphan(who, nonce)
}

func (p *Pool) dropOrphan(a cryptox.Address, nonce uint64) {
	if m, ok := p.orphans[a]; ok {
		if _, ok := m[nonce]; ok {
			delete(m, nonce)
			p.nOrphan--
		}
		if len(m) == 0 {
			delete(p.orphans, a)
		}
	}
}

// promote moves a sender's orphans that now follow on into the pool.
func (p *Pool) promote(from cryptox.Address) {
	for {
		p.mu.Lock()
		next := p.pending.Peek(from).Nonce
		o, ok := p.orphans[from][next]
		if ok {
			p.dropOrphan(from, next)
		}
		p.mu.Unlock()
		if !ok {
			return
		}
		if err := p.Add(o.tx); err != nil {
			return
		}
		if p.OnPromote != nil {
			p.OnPromote(o.tx)
		}
	}
}

// promoteAll forgets stale orphans and promotes the ones a new tip enabled.
func (p *Pool) promoteAll() {
	p.mu.Lock()
	var ready []cryptox.Address
	for a, m := range p.orphans {
		have := p.pending.Peek(a).Nonce
		for n, o := range m {
			if n < have || time.Since(o.at) > orphanTTL {
				p.dropOrphan(a, n)
			}
		}
		if _, ok := m[have]; ok {
			ready = append(ready, a)
		}
	}
	p.mu.Unlock()
	for _, a := range ready {
		p.promote(a)
	}
}
