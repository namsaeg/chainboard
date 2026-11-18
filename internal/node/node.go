// Package node wires the chain, mempool, store, miner and p2p host together,
// keeps the wallet, and indexes posts for the UI.
package node

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log"
	mrand "math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/namsaeg/chainboard/internal/chain"
	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/media"
	"github.com/namsaeg/chainboard/internal/mempool"
	"github.com/namsaeg/chainboard/internal/miner"
	"github.com/namsaeg/chainboard/internal/p2p"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/store"
	"github.com/namsaeg/chainboard/internal/types"
)

// Config configures a node.
type Config struct {
	Network      string
	DataDir      string
	Listen       string
	Seeds        []string
	Mine         bool
	PledgeBytes  uint64
	SendDelayMax time.Duration // own transactions leave after a random delay up to this
	Embargo      time.Duration // Dandelion++ embargo override (tests)
	Params       *params.Params
	Logf         func(string, ...any)
}

// Node is a running chainboard node.
type Node struct {
	cfg    Config
	P      *params.Params
	Chain  *chain.Chain
	Pool   *mempool.Pool
	Store  *store.Store
	Host   *p2p.Host
	Miner  *miner.Miner
	Wallet *cryptox.Key
	Index  *Index

	mu       sync.Mutex
	queue    []queued // own transactions waiting for their send time
	nextFree time.Time
	regFor   []uint64 // partitions we last registered
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

type queued struct {
	tx     *types.Tx
	sendAt time.Time
}

// New opens everything under cfg.DataDir.
func New(cfg Config) (*Node, error) {
	p := cfg.Params
	if p == nil {
		p = params.ByName(cfg.Network)
	}
	if p == nil {
		return nil, fmt.Errorf("unknown network %q", cfg.Network)
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	dir := filepath.Join(cfg.DataDir, p.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	wallet, err := cryptox.LoadOrCreateKey(filepath.Join(dir, "wallet.key"))
	if err != nil {
		return nil, err
	}
	nodeKey, err := cryptox.LoadOrCreateKey(filepath.Join(dir, "node.key"))
	if err != nil {
		return nil, err
	}
	ch, err := chain.Open(p, dir)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	// 0.1.0 kept data as files under store/; import what is complete.
	if old := filepath.Join(dir, "store"); dirExists(filepath.Join(old, "data")) {
		if n, err := st.ImportV010(old); err == nil {
			_ = os.Rename(old, filepath.Join(dir, "store.v010"))
			cfg.Logf("node: imported %d files from the 0.1.0 store", n)
		}
	}
	if groot, _ := chunk.Root(ch.GenesisData); !st.Complete(groot) {
		if _, err := st.PutData(ch.GenesisData); err != nil {
			return nil, err
		}
	}
	n := &Node{cfg: cfg, P: p, Chain: ch, Store: st, Wallet: wallet, Index: NewIndex()}
	n.Pool = mempool.New(ch.Tip(), n.checkUpload)
	host, err := p2p.New(p2p.Config{P: p, Chain: ch, Pool: n.Pool, Store: st,
		NodeKey: ed25519.PrivateKey(nodeKey.Priv), Listen: cfg.Listen, Seeds: cfg.Seeds, Logf: cfg.Logf, Embargo: cfg.Embargo})
	if err != nil {
		return nil, err
	}
	n.Host = host
	n.Miner = &miner.Miner{Chain: ch, Pool: n.Pool, Store: st, Key: wallet, PledgeBytes: cfg.PledgeBytes,
		OnBlock: host.BroadcastBlock, Logf: cfg.Logf}
	n.Index.Sync(ch)
	return n, nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// checkUpload is the pool's data policy: all chunks present and an accepted
// media type.
func (n *Node) checkUpload(t *types.Tx) error {
	if !n.Store.Complete(t.DataRoot) {
		return mempool.ErrWaitingData
	}
	first, err := n.Store.Chunk(t.DataRoot, 0)
	if err != nil {
		return mempool.ErrWaitingData
	}
	if _, _, err := media.Sniff(first); err != nil {
		return err
	}
	return nil
}

// Start runs networking, the tip watcher, the send queue and (optionally) mining.
func (n *Node) Start() error {
	if err := n.Host.Start(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	tips := n.Chain.Subscribe()
	n.wg.Add(2)
	go n.tipLoop(ctx, tips)
	go n.sendLoop(ctx)
	if n.cfg.Mine {
		n.wg.Add(1)
		go func() { defer n.wg.Done(); n.Miner.Run(ctx) }()
	}
	return nil
}

// Stop shuts the node down.
func (n *Node) Stop() {
	if n.cancel != nil {
		n.cancel()
	}
	n.Host.Stop()
	n.wg.Wait()
	_ = n.Chain.Close()
	_ = n.Store.Close()
}

func (n *Node) tipLoop(ctx context.Context, tips <-chan *state.State) {
	defer n.wg.Done()
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case st := <-tips:
			n.Pool.Reset(st)
			n.Index.Sync(n.Chain)
			n.storageDuties(st)
		case <-t.C:
			n.storageDuties(n.Chain.Tip())
		}
	}
}

// storageDuties fetches chunks for our partitions, registers them, and sends
// the epoch storage proof when one is due.
func (n *Node) storageDuties(st *state.State) {
	if !n.cfg.Mine {
		return
	}
	missing := n.Miner.Prepare(st)
	if len(missing) > 0 {
		n.Host.FetchChunks(missing)
	}
	assigned := n.Miner.Assigned()
	acc := n.Pool.Account(n.Wallet.Address())
	if !sameParts(assigned, n.regFor) && !sameParts(assigned, acc.Partitions) && acc.Balance > 0 && len(assigned) > 0 {
		tx := &types.Tx{Kind: types.TxRegister, Partitions: assigned}
		if err := n.sendNow(tx); err == nil {
			n.regFor = assigned
		}
	}
	if tx, ok := n.Miner.EpochProof(st, n.nextNonce()); ok {
		_ = n.Host.SubmitOwn(tx)
	}
}

func sameParts(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// nextNonce is the nonce for a new own transaction: pending pool state plus
// what is still queued here.
func (n *Node) nextNonce() uint64 {
	base := n.Pool.Account(n.Wallet.Address()).Nonce
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, q := range n.queue {
		if q.tx.Nonce >= base {
			base = q.tx.Nonce + 1
		}
	}
	return base
}

// prepare fills in nonce and fee and signs.
func (n *Node) prepare(t *types.Tx) error {
	t.Nonce = n.nextNonce()
	if t.Lane == types.LaneFee && t.MaxFee == 0 {
		st := n.Pool.PendingState()
		// Quote with headroom for base-fee moves while the transaction waits.
		t.MaxFee = 1 // placeholder so the size estimate includes the field
		t.Sign(n.Wallet)
		est, err := n.Quote(t)
		if err != nil {
			return err
		}
		t.MaxFee = est*2 + st.BaseFee*64
	}
	t.Sign(n.Wallet)
	return t.CheckStateless()
}

// Quote prices a fee-lane transaction at the current pending state: base fee,
// storage and tip, without the amount it moves.
func (n *Node) Quote(t *types.Tx) (uint64, error) {
	st := n.Pool.PendingState()
	acc := st.Account(n.Wallet.Address())
	acc.Balance = ^uint64(0) >> 2 // quote even without funds
	start := acc.Balance
	probe := *t
	probe.Nonce = acc.Nonce
	probe.MaxFee = ^uint64(0) >> 8
	probe.Sign(n.Wallet)
	if err := st.ApplyTx(&probe, st.NewCtx()); err != nil {
		return 0, err
	}
	spent := start - st.Accounts[n.Wallet.Address()].Balance
	switch t.Kind {
	case types.TxTransfer, types.TxStake:
		spent -= t.Amount
	}
	return spent, nil
}

// Send queues an own transaction to leave after a random delay. The queue is
// first-in first-out so nonces stay in order, and several transactions written
// close together leave together.
func (n *Node) Send(t *types.Tx) error {
	if err := n.prepare(t); err != nil {
		return err
	}
	// Dry-run against the pending state so the user hears about errors now.
	trial := n.trialState()
	if err := trial.ApplyTx(t, trial.NewCtx()); err != nil {
		if !(t.Kind == types.TxUpload && errors.Is(n.checkUpload(t), mempool.ErrWaitingData)) {
			return err
		}
	}
	delay := time.Duration(0)
	if n.cfg.SendDelayMax > 0 {
		delay = time.Duration(mrand.Int63n(int64(n.cfg.SendDelayMax)))
	}
	at := time.Now().Add(delay)
	n.mu.Lock()
	if at.Before(n.nextFree) {
		at = n.nextFree
	}
	n.nextFree = at
	n.queue = append(n.queue, queued{tx: t, sendAt: at})
	n.mu.Unlock()
	return nil
}

func (n *Node) sendNow(t *types.Tx) error {
	if err := n.prepare(t); err != nil {
		return err
	}
	return n.Host.SubmitOwn(t)
}

func (n *Node) sendLoop(ctx context.Context) {
	defer n.wg.Done()
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		var due []*types.Tx
		n.mu.Lock()
		for len(n.queue) > 0 && !n.queue[0].sendAt.After(now) {
			due = append(due, n.queue[0].tx)
			n.queue = n.queue[1:]
		}
		n.mu.Unlock()
		for _, tx := range due {
			if err := n.Host.SubmitOwn(tx); err != nil {
				n.cfg.Logf("node: send %s: %v", tx.Kind, err)
			}
		}
	}
}

// Queued returns own transactions still waiting to leave.
func (n *Node) Queued() []*types.Tx {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]*types.Tx, len(n.queue))
	for i, q := range n.queue {
		out[i] = q.tx
	}
	return out
}

// UploadResult says what Upload did with a file.
type UploadResult struct {
	Root   cryptox.Hash
	Action string // "upload", "extend" (stored already, retention lengthened) or "reuse"
	Until  uint64 // height the data is kept until; 0 means permanent
}

// trialState is the pending state with our still-queued transactions applied.
func (n *Node) trialState() *state.State {
	st := n.Pool.PendingState()
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, q := range n.queue {
		_ = st.ApplyTx(q.tx, st.NewCtx())
	}
	return st
}

// Upload re-encodes an image and stores it. Data already on chain is not
// uploaded again: if it is kept at least as long as the chosen tier it is
// simply reused, otherwise an extend lengthens its retention.
func (n *Node) Upload(raw []byte, tier uint16) (UploadResult, error) {
	months, perm, ok := params.TierMonths(tier)
	if !ok {
		return UploadResult{}, errors.New("bad retention tier")
	}
	data, class, err := media.Reencode(raw)
	if err != nil {
		return UploadResult{}, err
	}
	root, err := n.Store.PutData(data)
	if err != nil {
		return UploadResult{Root: root}, err
	}
	st := n.trialState()
	info, exists := st.Data[root]
	if exists {
		until := st.BucketExpiry(info.Bucket)
		if info.Tier == params.TierPermanent || (!perm && st.BucketFor(st.Height+1, months, perm) <= info.Bucket) {
			return UploadResult{Root: root, Action: "reuse", Until: until}, nil
		}
		t := &types.Tx{Kind: types.TxExtend, DataRoot: root, Tier: tier}
		if err := n.Send(t); err != nil {
			return UploadResult{Root: root}, err
		}
		return UploadResult{Root: root, Action: "extend", Until: st.BucketExpiry(st.BucketFor(st.Height+1, months, perm))}, nil
	}
	t := &types.Tx{Kind: types.TxUpload, DataRoot: root, Size: uint64(len(data)), Tier: tier, Media: class}
	if err := n.Send(t); err != nil {
		return UploadResult{Root: root}, err
	}
	return UploadResult{Root: root, Action: "upload", Until: st.BucketExpiry(st.BucketFor(st.Height+1, months, perm))}, nil
}
