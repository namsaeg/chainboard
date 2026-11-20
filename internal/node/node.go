// Package node wires the chain, mempool, store, miner and p2p host together,
// keeps the wallet, and indexes posts for the UI.
package node

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"

	"github.com/namsaeg/chainboard/internal/anonnet"
	"github.com/namsaeg/chainboard/internal/chain"
	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/keystore"
	"github.com/namsaeg/chainboard/internal/media"
	"github.com/namsaeg/chainboard/internal/mempool"
	"github.com/namsaeg/chainboard/internal/miner"
	"github.com/namsaeg/chainboard/internal/nat"
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
	// NAT, if set, asks the home router to forward the listening port.
	NAT *nat.Config
	// Proxy carries outbound connections (Tor, Nym); with ProxyOnly the
	// node never connects directly.
	Proxy     *anonnet.Proxy
	ProxyOnly bool
	// TorControl, if set, publishes the listener as a Tor onion service
	// (its key is kept in onion.key) and announces that address.
	TorControl  string
	TorPassword string
	// Cover turns on cover traffic to the stem peer.
	Cover p2p.CoverConfig
	// TextCacheDays is how long post bodies (and data) are kept locally when
	// this node does not store them as a miner; older ones are fetched when
	// opened. 0 means 30.
	TextCacheDays uint64
	// TextWindowSec overrides how recent a block must be to need its
	// bodies before it is accepted (tests).
	TextWindowSec int64
	// Password locks the secret key files (wallet, membership, pseudonym);
	// empty keeps them plain.
	Password string
	Params   *params.Params
	Logf     func(string, ...any)
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

	dir        string
	mu         sync.Mutex
	sendMu     sync.Mutex         // held from choosing a nonce until the tx is queued or sent
	queue      []queued           // own transactions waiting for their send time
	sent       map[uint64]*sentTx // own account transactions sent but not yet mined, by nonce
	lastOutbox []byte             // outbox.json as last written
	unrefData  map[cryptox.Hash]bool
	unrefText  map[cryptox.Hash]bool
	foreign    bool // guarded by rlnMu: another device posts with our key
	foreignAt  time.Time
	nextFree   time.Time
	regFor     []uint64 // partitions we last registered
	cancel     context.CancelFunc
	ctx        context.Context // set by Start; done when the node stops
	wg         sync.WaitGroup

	fundMu sync.Mutex // guards funds
	funds  map[cryptox.Hash]*fundTend

	offerMu sync.Mutex // guards offers and shared
	offers  map[cryptox.Hash]*offerTend
	shared  map[cryptox.Hash]bool // own offered data whose offer was accepted

	likesMu        sync.Mutex // guards likes
	likes          []QueuedLike
	likeAuto       LikeAuto
	likesFlush     sync.Mutex // one batch at a time
	likeWarnLogged time.Time

	tipMu     sync.Mutex // one-time tip keys file, and the keys read from it
	tipCache  []*cryptox.Key
	tipLoaded bool
	rlnMu     sync.Mutex // guards the fields below
	secret    *fr.Element
	usage     rlnUsage
	pseudo    *cryptox.Key
	outbox    []*outboxItem
}

// sentTx is an own transaction that left through the stem. It does not enter
// our own pool until it comes back as fluff, so its nonce is tracked here.
type sentTx struct {
	tx *types.Tx
	at time.Time
}

// queued is an own transaction (tx) or anonymous post (anon) waiting to leave.
type queued struct {
	tx     *types.Tx
	anon   *AnonRequest
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
	// Secret keys follow the password: with one, plain key files left by
	// an older version are locked now; without one, locked files refuse.
	if err := keystore.Check(dir, cfg.Password); err != nil {
		return nil, err
	}
	if plain, _ := keystore.State(dir); cfg.Password != "" && len(plain) > 0 {
		if err := keystore.Rekey(dir, cfg.Password, cfg.Password); err != nil {
			return nil, err
		}
		cfg.Logf("node: locked %v with the password", plain)
	}
	wallet, err := loadKey(filepath.Join(dir, "wallet.key"), cfg.Password)
	if err != nil {
		return nil, err
	}
	nodeKey, err := loadKey(filepath.Join(dir, "node.key"), "") // never locked: see keystore.SecretFiles
	if err != nil {
		return nil, err
	}
	ch, err := chain.Open(p, dir)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(dir)
	if err != nil {
		ch.Close()
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			st.Close()
			ch.Close()
		}
	}()
	if ch.SetAside != "" {
		cfg.Logf("node: the chain database belonged to another network; moved it to %s", ch.SetAside)
		// What waited to be sent, queued likes, own funding requests and
		// spent message ids all belong to that chain.
		for _, name := range []string{"outbox.json", "likes.json", "fund.json", "offers.json", "rln_used.json"} {
			if err := os.Remove(filepath.Join(dir, name)); err == nil {
				cfg.Logf("node: dropped %s of the other network", name)
			}
		}
	}
	if dropped, err := st.UseGenesis(ch.Genesis()); err != nil {
		return nil, err
	} else if dropped {
		cfg.Logf("node: dropped packed replicas and post bodies of another network")
	}
	if groot, _ := chunk.Root(ch.GenesisData); !st.Complete(groot) {
		if _, err := st.PutData(ch.GenesisData); err != nil {
			return nil, err
		}
	}
	n := &Node{cfg: cfg, P: p, Chain: ch, Store: st, Wallet: wallet, dir: dir,
		sent: map[uint64]*sentTx{}, funds: map[cryptox.Hash]*fundTend{},
		offers: map[cryptox.Hash]*offerTend{}, shared: map[cryptox.Hash]bool{}}
	if err := n.loadRLN(); err != nil {
		return nil, err
	}
	if k, err := os.Stat(filepath.Join(dir, "pseudonym.key")); err == nil && !k.IsDir() {
		if n.pseudo, err = loadKey(filepath.Join(dir, "pseudonym.key"), cfg.Password); err != nil {
			return nil, err
		}
	}
	n.Pool = mempool.New(ch.Tip(), n.checkUpload)
	n.Pool.OnDoubleSignal = n.onDoubleSignal
	n.Pool.Bodies = st
	ch.Texts = st
	ch.TextWindowSec = cfg.TextWindowSec
	ch.KeepBundle = func(data []byte) { _, _ = st.PutData(data) }
	ch.Logf = cfg.Logf
	host, err := p2p.New(p2p.Config{P: p, Chain: ch, Pool: n.Pool, Store: st,
		NodeKey: ed25519.PrivateKey(nodeKey.Priv), Listen: cfg.Listen, Seeds: cfg.Seeds, Logf: cfg.Logf, Embargo: cfg.Embargo,
		Text: n.textBody, Gossip: n, Serve: n.serves, NAT: cfg.NAT, Proxy: cfg.Proxy, ProxyOnly: cfg.ProxyOnly, Cover: cfg.Cover})
	if err != nil {
		return nil, err
	}
	n.Host = host
	n.Miner = &miner.Miner{Chain: ch, Pool: n.Pool, Store: st, Key: wallet, PledgeBytes: cfg.PledgeBytes,
		OnBlock: host.BroadcastBlock, Logf: cfg.Logf}
	ix, err := OpenIndex(filepath.Join(dir, "index.db"), ch.Genesis())
	if err != nil {
		return nil, err
	}
	ix.RecentBlocks = n.cacheBlocks()
	ix.LikeWindow = LikeWindowDays * n.P.BlocksPerDay
	n.Index = ix
	if err := ix.Sync(ch); err != nil {
		ix.Close()
		return nil, err
	}
	n.loadOutbox()
	n.loadLikes()
	n.loadFunds()
	n.loadOffers()
	ok = true
	return n, nil
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
	n.ctx = ctx
	n.solveOwnFunds()
	n.solveOwnOffers()
	if n.cfg.TorControl != "" && n.Host.Addr() != "" {
		n.wg.Add(1)
		go n.onionLoop(ctx)
	}
	tips := n.Chain.Subscribe()
	n.wg.Add(4)
	go n.tipLoop(ctx, tips)
	go n.sendLoop(ctx)
	go n.textLoop(ctx)
	go func() { defer n.wg.Done(); n.Chain.VerifyLoop(ctx) }()
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
	n.saveOutbox()
	_ = n.Index.Close()
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
		case <-tips:
			// Work from the newest tip, not the notified one: a backlog of
			// notifications must not move the pool backwards.
			st := n.Chain.Tip()
			n.syncPool()
			if err := n.Index.Sync(n.Chain); err != nil {
				n.cfg.Logf("node: index: %v", err)
			}
			n.tendSent(st)
			n.checkForeign(st)
			n.tendOutbox(st)
			n.tendFunds(st)
			n.pruneFunds(st)
			n.tendBounties(st)
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
	if n.proofTravelling(st.Epoch(st.Height + 1)) {
		return
	}
	n.sendMu.Lock()
	defer n.sendMu.Unlock()
	if tx, ok := n.Miner.EpochProof(st, n.nextNonce()); ok {
		_ = n.submit(tx)
	}
}

// proofTravelling reports whether our storage proof for epoch is sent but not
// mined yet.
func (n *Node) proofTravelling(epoch uint64) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, s := range n.sent {
		if s.tx.Kind == types.TxProof && s.tx.Proof.Epoch == epoch {
			return true
		}
	}
	return false
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

// syncPool rebuilds the pool on the chain tip when a block arrived that the
// tip watcher has not handled yet, so own transactions see current balances.
func (n *Node) syncPool() {
	if tip := n.Chain.Tip(); n.Pool.TipHash() != tip.Tip {
		n.Pool.Reset(tip)
	}
}

// nextNonce is the nonce for a new own transaction: pending pool state plus
// what is still queued or travelling the stem.
func (n *Node) nextNonce() uint64 {
	n.syncPool()
	pooled := n.Pool.Account(n.Wallet.Address()).Nonce
	next := pooled
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, q := range n.queue {
		if q.tx != nil && q.tx.Nonce >= next {
			next = q.tx.Nonce + 1
		}
	}
	for nonce := range n.sent {
		if nonce >= next {
			next = nonce + 1
		}
	}
	return next
}

// submit sends an own account transaction through the stem and remembers it
// until it is mined.
func (n *Node) submit(t *types.Tx) error {
	if err := n.Host.SubmitOwn(t); err != nil {
		return err
	}
	n.mu.Lock()
	n.sent[t.Nonce] = &sentTx{tx: t, at: time.Now()}
	n.mu.Unlock()
	return nil
}

// resendOwnAfter is how long a sent transaction may stay out of our pool
// before it is sent again: past the longest embargo, it was dropped.
func (n *Node) resendOwnAfter() time.Duration {
	if n.cfg.Embargo > 0 {
		return 4*n.cfg.Embargo + 2*time.Second
	}
	return 2 * time.Minute
}

// tendSent forgets mined own transactions and sends again the ones the
// network dropped. One that can no longer apply blocks every later nonce, so
// it and the ones after it are given up (and logged).
func (n *Node) tendSent(tip *state.State) {
	addr := n.Wallet.Address()
	mined := tip.Peek(addr).Nonce
	n.mu.Lock()
	var stale []*types.Tx
	for nonce, s := range n.sent {
		switch {
		case nonce < mined:
			delete(n.sent, nonce)
		case time.Since(s.at) > n.resendOwnAfter() && !n.Pool.Has(s.tx.ID()):
			stale = append(stale, s.tx)
			s.at = time.Now()
		}
	}
	n.mu.Unlock()
	if len(stale) == 0 {
		return
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].Nonce < stale[j].Nonce })
	trial := n.Pool.PendingState()
	for _, t := range stale {
		err := trial.ApplyTx(t, trial.NewCtx())
		if err == nil {
			_ = n.Host.SubmitOwn(t)
			continue
		}
		if t.Nonce != trial.Peek(addr).Nonce {
			continue // waits on an earlier one still travelling
		}
		// Fill the nonce with a no-op so the later ones (unchanged, as they
		// may still be travelling) can apply. Re-signing them under new
		// nonces could let both copies be mined.
		filler := &types.Tx{Kind: types.TxTransfer, To: addr, Lane: types.LaneFee, Nonce: t.Nonce}
		if n.finish(filler) == nil && trial.ApplyTx(filler, trial.NewCtx()) == nil && n.Host.SubmitOwn(filler) == nil {
			n.mu.Lock()
			n.sent[t.Nonce] = &sentTx{tx: filler, at: time.Now()}
			n.mu.Unlock()
			n.cfg.Logf("node: %s (nonce %d) can no longer apply (%v); replaced by a no-op", t.Kind, t.Nonce, err)
			continue
		}
		n.mu.Lock()
		dropped := 0
		for nonce := range n.sent {
			if nonce >= t.Nonce {
				delete(n.sent, nonce)
				dropped++
			}
		}
		n.mu.Unlock()
		n.cfg.Logf("node: %s (nonce %d) can no longer apply: %v; gave up %d transaction(s)", t.Kind, t.Nonce, err, dropped)
		return
	}
}

// prepare fills in nonce and fee and signs.
func (n *Node) prepare(t *types.Tx) error {
	if t.Kind == types.TxPost && len(t.Text) > 0 {
		t.SetText(t.Text)
		if t.Tier == 0 {
			t.Tier = params.TierFree
		}
	}
	if err := n.keepBody(t); err != nil {
		return err
	}
	n.syncPool()
	t.Nonce = n.nextNonce()
	return n.finish(t)
}

// finish fills in the fee of a transaction whose nonce is set, and signs.
func (n *Node) finish(t *types.Tx) error {
	if t.Lane == types.LaneFee && t.MaxFee == 0 {
		st := n.Pool.PendingState()
		// Quote with headroom for base-fee moves while the transaction waits.
		t.MaxFee = 1 // placeholder so the size estimate includes the field
		t.Sign(n.Wallet)
		est, err := n.Quote(t)
		if err != nil {
			return err
		}
		t.MaxFee = est*2 + max(st.BaseFee, st.PermByteFee())*64
	}
	t.Sign(n.Wallet)
	return t.CheckStateless()
}

// Quote prices a fee-lane transaction at the current pending state: base fee,
// storage and tip, without the amount it moves.
func (n *Node) Quote(t *types.Tx) (uint64, error) { return n.quoteAs(t, n.Wallet) }

func (n *Node) quoteAs(t *types.Tx, k *cryptox.Key) (uint64, error) {
	// Own transactions still queued or travelling count: a post may attach
	// the upload sent just before it.
	st := n.trialState()
	st.Settle(k.Address(), st.Height+1) // what settles is not spent
	acc := st.Account(k.Address())
	acc.Balance = ^uint64(0) >> 2 // quote even without funds
	start := acc.Balance
	probe := *t
	probe.Nonce = acc.Nonce
	probe.MaxFee = ^uint64(0) >> 8
	probe.Sign(k)
	if err := st.ApplyTx(&probe, st.NewCtx()); err != nil {
		return 0, err
	}
	spent := start - st.Peek(k.Address()).Balance
	switch t.Kind {
	case types.TxTransfer, types.TxStake:
		spent -= t.Amount
	case types.TxLikes:
		for _, p := range t.Pledges {
			spent -= p.Amount
		}
		for _, p := range t.Tips {
			spent -= p.Amount
		}
	}
	return spent, nil
}

// Send queues an own transaction to leave after a random delay. The queue is
// first-in first-out so nonces stay in order, and several transactions written
// close together leave together.
func (n *Node) Send(t *types.Tx) error {
	n.sendMu.Lock()
	defer n.sendMu.Unlock()
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
	n.enqueue(queued{tx: t})
	return nil
}

func (n *Node) sendNow(t *types.Tx) error {
	n.sendMu.Lock()
	defer n.sendMu.Unlock()
	if err := n.prepare(t); err != nil {
		return err
	}
	return n.submit(t)
}

func (n *Node) sendLoop(ctx context.Context) {
	defer n.wg.Done()
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for tick := 0; ; tick++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if tick%5 == 0 {
			n.saveOutbox()
			n.tendLikes(time.Now())
		}
		now := time.Now()
		var due []queued
		n.mu.Lock()
		for len(n.queue) > 0 && !n.queue[0].sendAt.After(now) {
			q := n.queue[0]
			due = append(due, q)
			n.queue = n.queue[1:]
			if q.tx != nil {
				// Counted as travelling from here on, so its nonce is
				// never handed out again while it is being sent.
				n.sent[q.tx.Nonce] = &sentTx{tx: q.tx, at: now}
			}
		}
		n.mu.Unlock()
		for _, q := range due {
			if q.anon != nil {
				n.leaveAnon(*q.anon)
				continue
			}
			if err := n.Host.SubmitOwn(q.tx); err != nil {
				n.cfg.Logf("node: send %s: %v", q.tx.Kind, err)
			}
		}
	}
}

// WalletState is the pending state as our wallet will see it once our queued
// and travelling transactions apply.
func (n *Node) WalletState() *state.State { return n.trialState() }

// Outgoing counts own transactions not mined yet (queued or travelling).
func (n *Node) Outgoing() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.queue) + len(n.sent)
}

// Queued returns own transactions still waiting to leave.
func (n *Node) Queued() []*types.Tx {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []*types.Tx
	for _, q := range n.queue {
		if q.tx != nil {
			out = append(out, q.tx)
		}
	}
	return out
}

// UploadResult says what Upload did with a file.
type UploadResult struct {
	Root   cryptox.Hash
	Action string // "upload", "extend" (stored already, retention lengthened) or "reuse"
	Until  uint64 // height the data is kept until; 0 means permanent
}

// trialState is the pending state with our travelling and still-queued
// transactions applied.
func (n *Node) trialState() *state.State {
	n.syncPool()
	st := n.Pool.PendingState()
	n.mu.Lock()
	defer n.mu.Unlock()
	var travelling []*types.Tx
	for _, s := range n.sent {
		travelling = append(travelling, s.tx)
	}
	sort.Slice(travelling, func(i, j int) bool { return travelling[i].Nonce < travelling[j].Nonce })
	for _, t := range travelling {
		_ = st.ApplyTx(t, st.NewCtx()) // fails harmlessly if already pooled
	}
	for _, q := range n.queue {
		if q.tx != nil {
			_ = st.ApplyTx(q.tx, st.NewCtx())
		}
	}
	return st
}

// Upload re-encodes an image and stores it. Data already on chain is not
// uploaded again: if it is kept at least as long as the chosen tier it is
// simply reused, otherwise an extend lengthens its retention.
func (n *Node) Upload(raw []byte, tier uint16, confirm bool) (UploadResult, error) {
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
	info, exists := st.Data.Get(root)
	if exists {
		until := st.BucketExpiry(info.Bucket)
		if info.Tier == params.TierPermanent || (!perm && st.BucketFor(st.Height+1, months, perm) <= info.Bucket) {
			return UploadResult{Root: root, Action: "reuse", Until: until}, nil
		}
		t := &types.Tx{Kind: types.TxExtend, DataRoot: root, Tier: tier}
		if err := n.SendChecked(t, confirm); err != nil {
			return UploadResult{Root: root}, err
		}
		return UploadResult{Root: root, Action: "extend", Until: st.BucketExpiry(st.BucketFor(st.Height+1, months, perm))}, nil
	}
	t := &types.Tx{Kind: types.TxUpload, DataRoot: root, Size: uint64(len(data)), Tier: tier, Media: class}
	if err := n.SendChecked(t, confirm); err != nil {
		return UploadResult{Root: root}, err
	}
	return UploadResult{Root: root, Action: "upload", Until: st.BucketExpiry(st.BucketFor(st.Height+1, months, perm))}, nil
}

// loadKey reads an Ed25519 key (hex seed) from a key file, unlocking it
// with password, and creates it (locked with password, if set) when absent.
func loadKey(path, password string) (*cryptox.Key, error) {
	b, err := keystore.Read(path, password)
	if errors.Is(err, os.ErrNotExist) {
		k, err := cryptox.NewKey()
		if err != nil {
			return nil, err
		}
		return k, keystore.Write(path, []byte(hex.EncodeToString(k.Priv.Seed())+"\n"), password)
	}
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != 32 {
		return nil, fmt.Errorf("key file %s is corrupt", filepath.Base(path))
	}
	return cryptox.KeyFromSeed(seed), nil
}
