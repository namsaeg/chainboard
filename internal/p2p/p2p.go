// Package p2p connects nodes over mutually authenticated TLS 1.3. It gossips
// blocks and transactions, syncs the chain, serves data chunks, and hides the
// origin of transactions with Dandelion++: a new transaction first travels a
// random single-peer "stem" and only then is broadcast ("fluff").
package p2p

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	mrand "math/rand"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/namsaeg/chainboard/internal/chain"
	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/mempool"
	"github.com/namsaeg/chainboard/internal/merkle"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/store"
	"github.com/namsaeg/chainboard/internal/types"
)

type Hash = cryptox.Hash

const (
	maxOutbound   = 8
	maxInbound    = 32
	syncBatch     = 200
	stemEpoch     = 10 * time.Minute
	fluffChance   = 10 // percent chance a stem hop turns into fluff
	embargoMin    = 30 * time.Second
	embargoJitter = 30 * time.Second
	seenLimit     = 50_000
)

// Config wires the host to the rest of the node.
type Config struct {
	P       *params.Params
	Chain   *chain.Chain
	Pool    *mempool.Pool
	Store   *store.Store
	NodeKey ed25519.PrivateKey
	Listen  string   // e.g. ":7700"; empty disables inbound
	Seeds   []string // host:port to dial
	Logf    func(string, ...any)
	// Embargo overrides the Dandelion++ embargo (tests).
	Embargo time.Duration
	// Text finds a post body to serve to a peer: from the text store, or
	// cut out of a sealed bundle. Defaults to the text store.
	Text func(Hash) ([]byte, bool)
}

// Host is the p2p node.
type Host struct {
	cfg   Config
	tls   *tls.Config
	nonce uint64

	mu       sync.Mutex
	peers    map[*peer]bool
	known    map[string]bool // addresses learned from peers
	seen     map[Hash]bool   // this generation of seen tx and block ids
	seenOld  map[Hash]bool   // the previous generation
	stem     *peer
	stemAt   time.Time
	embargo  map[Hash]*time.Timer
	fetching map[string]time.Time
	waiting  map[Hash]*types.Tx // uploads whose chunks are being fetched
	parked   map[Hash]*parkedBlock
	askedTxt map[Hash]time.Time // bodies requested, by hash

	ln   net.Listener
	quit chan struct{}
	wg   sync.WaitGroup
}

type peer struct {
	h       *Host
	conn    *tls.Conn
	out     bool
	addr    string // dialable address (outbound, or the peer's announced listen address)
	id      []byte
	height  uint64
	sendCh  chan frame    // never closed: a send may race a disconnect
	quit    chan struct{} // closed when the peer is closed
	closeMu sync.Once
}

type frame struct {
	typ uint8
	b   []byte
}

// New creates a host.
func New(cfg Config) (*Host, error) {
	tc, err := tlsConfig(cfg.NodeKey)
	if err != nil {
		return nil, err
	}
	var nb [8]byte
	rand.Read(nb[:])
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	return &Host{cfg: cfg, tls: tc, nonce: binary.BigEndian.Uint64(nb[:]),
		peers: map[*peer]bool{}, known: map[string]bool{}, seen: map[Hash]bool{}, seenOld: map[Hash]bool{},
		embargo: map[Hash]*time.Timer{}, fetching: map[string]time.Time{}, waiting: map[Hash]*types.Tx{},
		parked: map[Hash]*parkedBlock{}, askedTxt: map[Hash]time.Time{},
		quit: make(chan struct{})}, nil
}

// Start listens and dials seeds.
func (h *Host) Start() error {
	if h.cfg.Listen != "" {
		ln, err := tls.Listen("tcp", h.cfg.Listen, h.tls)
		if err != nil {
			return err
		}
		h.ln = ln
		h.wg.Add(1)
		go h.acceptLoop()
	}
	for _, s := range h.cfg.Seeds {
		h.known[s] = true
	}
	h.wg.Add(1)
	go h.maintain()
	return nil
}

// Addr is the actual listen address.
func (h *Host) Addr() string {
	if h.ln == nil {
		return ""
	}
	return h.ln.Addr().String()
}

// Stop closes everything.
func (h *Host) Stop() {
	close(h.quit)
	if h.ln != nil {
		h.ln.Close()
	}
	h.mu.Lock()
	for p := range h.peers {
		p.close()
	}
	for _, t := range h.embargo {
		t.Stop()
	}
	h.mu.Unlock()
	h.wg.Wait()
}

// PeerCount is the number of connected peers.
func (h *Host) PeerCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.peers)
}

func (h *Host) acceptLoop() {
	defer h.wg.Done()
	for {
		c, err := h.ln.Accept()
		if err != nil {
			select {
			case <-h.quit:
				return
			default:
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}
		h.mu.Lock()
		in := 0
		for p := range h.peers {
			if !p.out {
				in++
			}
		}
		h.mu.Unlock()
		if in >= maxInbound {
			c.Close()
			continue
		}
		go h.handshake(c.(*tls.Conn), false, "")
	}
}

func (h *Host) maintain() {
	defer h.wg.Done()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		h.dialMore()
		h.refetchWaiting()
		h.retryParked()
		select {
		case <-h.quit:
			return
		case <-t.C:
		}
	}
}

func (h *Host) dialMore() {
	h.mu.Lock()
	out := 0
	connected := map[string]bool{}
	for p := range h.peers {
		if p.out {
			out++
		}
		connected[p.addr] = true
	}
	var cand []string
	for a := range h.known {
		if !connected[a] && a != h.Addr() {
			if last, ok := h.fetching["dial:"+a]; ok && time.Since(last) < 10*time.Second {
				continue
			}
			cand = append(cand, a)
		}
	}
	for _, a := range cand {
		h.fetching["dial:"+a] = time.Now()
	}
	h.mu.Unlock()
	mrand.Shuffle(len(cand), func(i, j int) { cand[i], cand[j] = cand[j], cand[i] })
	for _, a := range cand {
		if out >= maxOutbound {
			break
		}
		out++
		go func(a string) {
			d := &net.Dialer{Timeout: 5 * time.Second}
			c, err := tls.DialWithDialer(d, "tcp", a, h.tls)
			if err != nil {
				return
			}
			h.handshake(c, true, a)
		}(a)
	}
}

func (h *Host) handshake(c *tls.Conn, out bool, addr string) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := c.Handshake(); err != nil {
		c.Close()
		return
	}
	tip := h.cfg.Chain.Tip()
	me := &hello{Magic: h.cfg.P.Magic, Version: params.Version, Genesis: h.cfg.Chain.Genesis(),
		Height: tip.Height, Best: tip.Tip, Listen: h.announce(), Nonce: h.nonce}
	if err := writeMsg(c, msgHello, me.encode()); err != nil {
		c.Close()
		return
	}
	typ, b, err := readMsg(c)
	if err != nil || typ != msgHello {
		c.Close()
		return
	}
	them, err := decodeHello(b)
	if err != nil || them.Magic != h.cfg.P.Magic || them.Genesis != h.cfg.Chain.Genesis() || them.Nonce == h.nonce {
		c.Close()
		return
	}
	c.SetDeadline(time.Time{})
	if addr == "" && them.Listen != "" {
		host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		_, port, err := net.SplitHostPort(them.Listen)
		if err == nil {
			addr = net.JoinHostPort(host, port)
		}
	}
	p := &peer{h: h, conn: c, out: out, addr: addr, id: peerKey(c.ConnectionState()), height: them.Height, sendCh: make(chan frame, 256), quit: make(chan struct{})}
	h.mu.Lock()
	h.peers[p] = true
	if addr != "" {
		h.known[addr] = true
	}
	h.mu.Unlock()
	h.wg.Add(2)
	go p.writeLoop()
	go p.readLoop()
	p.send(msgGetPeers, nil)
	if them.Height > tip.Height || (them.Height == tip.Height && them.Best != tip.Tip) {
		h.requestSync(p)
	}
}

func (h *Host) announce() string {
	if h.ln == nil {
		return ""
	}
	return h.ln.Addr().String()
}

func (p *peer) send(typ uint8, b []byte) {
	select {
	case <-p.quit:
	case p.sendCh <- frame{typ, b}:
	default: // slow peer: drop rather than block the node
	}
}

func (p *peer) close() {
	p.closeMu.Do(func() {
		p.conn.Close()
		close(p.quit)
	})
}

func (p *peer) writeLoop() {
	defer p.h.wg.Done()
	for {
		select {
		case <-p.quit:
			return
		case f := <-p.sendCh:
			p.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := writeMsg(p.conn, f.typ, f.b); err != nil {
				p.close()
				return
			}
		}
	}
}

func (p *peer) readLoop() {
	defer p.h.wg.Done()
	defer func() {
		p.h.mu.Lock()
		delete(p.h.peers, p)
		if p.h.stem == p {
			p.h.stem = nil
		}
		p.h.mu.Unlock()
		p.close()
	}()
	for {
		p.conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		typ, b, err := readMsg(p.conn)
		if err != nil {
			return
		}
		if err := p.h.handle(p, typ, b); err != nil {
			p.h.cfg.Logf("p2p: dropping peer %s: %v", p.addr, err)
			return
		}
	}
}

func (h *Host) markSeen(id Hash) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen[id] || h.seenOld[id] {
		return false
	}
	// Two generations: when this one is full it becomes the old one and the
	// oldest is dropped whole. O(1) per id; at least seenLimit ids are
	// always remembered.
	if len(h.seen) >= seenLimit {
		h.seenOld, h.seen = h.seen, make(map[Hash]bool, seenLimit)
	}
	h.seen[id] = true
	return true
}

func (h *Host) broadcast(typ uint8, b []byte, except *peer) {
	h.mu.Lock()
	ps := make([]*peer, 0, len(h.peers))
	for p := range h.peers {
		if p != except {
			ps = append(ps, p)
		}
	}
	h.mu.Unlock()
	for _, p := range ps {
		p.send(typ, b)
	}
}

// BroadcastBlock sends a newly mined block to every peer.
func (h *Host) BroadcastBlock(b *types.Block) {
	h.markSeen(b.Hash())
	h.broadcast(msgBlock, b.Encode(), nil)
}

func (h *Host) requestSync(p *peer) {
	var w codec.Writer
	loc := h.cfg.Chain.Locator()
	w.Var(uint64(len(loc)))
	for _, x := range loc {
		w.Fixed(x[:])
	}
	w.Var(syncBatch)
	p.send(msgGetBlocks, w.B)
}

func (h *Host) handle(p *peer, typ uint8, b []byte) error {
	switch typ {
	case msgBlock:
		blk, err := types.DecodeBlock(b)
		if err != nil {
			return err
		}
		if blk.Height > p.height {
			p.height = blk.Height
		}
		if !h.markSeen(blk.Hash()) {
			return nil
		}
		var mt *chain.MissingTextsError
		switch err := h.cfg.Chain.Add(blk); {
		case err == nil:
			h.broadcast(msgBlock, b, p)
		case errors.As(err, &mt):
			h.park(blk, b, mt.Hashes, p)
		case errors.Is(err, chain.ErrOrphan):
			h.requestSync(p)
		case errors.Is(err, chain.ErrInvalid):
			return err
		}
	case msgGetBlocks:
		r := codec.NewReader(b)
		n := r.Count(64)
		loc := make([]Hash, n)
		for i := range loc {
			r.Fixed(loc[i][:])
		}
		max := int(r.Var())
		if r.Err != nil {
			return r.Err
		}
		if max > syncBatch {
			max = syncBatch
		}
		blocks := h.cfg.Chain.After(loc, max)
		var w codec.Writer
		w.Var(uint64(len(blocks)))
		for _, blk := range blocks {
			w.Bytes(blk.Encode())
		}
		p.send(msgBlocks, w.B)
	case msgBlocks:
		r := codec.NewReader(b)
		n := r.Count(syncBatch)
		added := 0
		for i := 0; i < n && r.Err == nil; i++ {
			raw := r.Bytes(maxMessage)
			blk, err := types.DecodeBlock(raw)
			if err != nil {
				return err
			}
			h.markSeen(blk.Hash())
			err = h.cfg.Chain.Add(blk)
			if errors.Is(err, chain.ErrInvalid) {
				return err
			}
			var mt *chain.MissingTextsError
			if errors.As(err, &mt) {
				// Later blocks of the batch wait as orphans behind this one.
				h.park(blk, raw, mt.Hashes, p)
			}
			if err == nil {
				added++
			}
		}
		if n == syncBatch && added > 0 {
			h.requestSync(p)
		}
	case msgTx:
		t, err := types.DecodeWire(b)
		if err != nil {
			return err
		}
		h.receiveFluff(p, t, b)
	case msgStemTx:
		t, err := types.DecodeWire(b)
		if err != nil {
			return err
		}
		h.receiveStem(t, b)
	case msgGetTexts:
		r := codec.NewReader(b)
		n := r.Count(maxTextsPerMsg)
		var w codec.Writer
		var bodies [][]byte
		for i := 0; i < n && r.Err == nil; i++ {
			var x Hash
			r.Fixed(x[:])
			if body, ok := h.textFor(x); ok {
				bodies = append(bodies, body)
			}
		}
		if err := r.Done(); err != nil {
			return err
		}
		if len(bodies) > 0 {
			w.Var(uint64(len(bodies)))
			for _, body := range bodies {
				w.Bytes(body)
			}
			p.send(msgTexts, w.B)
		}
	case msgTexts:
		r := codec.NewReader(b)
		n := r.Count(maxTextsPerMsg)
		got := 0
		for i := 0; i < n && r.Err == nil; i++ {
			body := r.Bytes(params.MaxPostBytes)
			if r.Err != nil {
				break
			}
			x := types.TextHashOf(body)
			h.mu.Lock()
			_, asked := h.askedTxt[x]
			delete(h.askedTxt, x)
			h.mu.Unlock()
			if !asked {
				continue // unsolicited bodies are not stored
			}
			if _, err := h.cfg.Store.PutText(body); err == nil {
				got++
			}
		}
		if err := r.Done(); err != nil {
			return err
		}
		if got > 0 {
			h.retryParked()
		}
	case msgGetChunk:
		r := codec.NewReader(b)
		var root Hash
		r.Fixed(root[:])
		idx := r.Var()
		if err := r.Done(); err != nil {
			return err
		}
		c, path, err := h.cfg.Store.ChunkProof(root, idx)
		if err != nil {
			return nil // we do not have it
		}
		size, _ := h.cfg.Store.Size(root)
		var w codec.Writer
		w.Fixed(root[:])
		w.Var(idx)
		w.Var(size)
		w.Bytes(c)
		w.Var(uint64(len(path)))
		for _, x := range path {
			w.Fixed(x[:])
		}
		p.send(msgChunk, w.B)
	case msgChunk:
		r := codec.NewReader(b)
		var root Hash
		r.Fixed(root[:])
		idx := r.Var()
		size := r.Var()
		c := r.Bytes(params.ChunkSize)
		n := r.Count(64)
		path := make(merkle.Proof, n)
		for i := range path {
			r.Fixed(path[i][:])
		}
		if err := r.Done(); err != nil {
			return err
		}
		if _, ok := h.cfg.Store.Size(root); !ok {
			if !h.wanted(root, size) {
				return nil
			}
			if err := h.cfg.Store.Begin(root, size); err != nil {
				return nil
			}
		}
		if err := h.cfg.Store.PutChunk(root, idx, c, path); err != nil {
			return err
		}
		h.retryWaiting(root)
	case msgGetPeers:
		h.mu.Lock()
		var w codec.Writer
		var addrs []string
		for p2 := range h.peers {
			if p2.addr != "" && p2 != p {
				addrs = append(addrs, p2.addr)
			}
		}
		h.mu.Unlock()
		if len(addrs) > 32 {
			addrs = addrs[:32]
		}
		w.Var(uint64(len(addrs)))
		for _, a := range addrs {
			w.Bytes([]byte(a))
		}
		p.send(msgPeers, w.B)
	case msgPeers:
		r := codec.NewReader(b)
		n := r.Count(64)
		h.mu.Lock()
		for i := 0; i < n && r.Err == nil; i++ {
			a := string(r.Bytes(256))
			if _, _, err := net.SplitHostPort(a); err == nil && len(h.known) < 1000 {
				h.known[a] = true
			}
		}
		h.mu.Unlock()
	default:
		return fmt.Errorf("unknown message %d", typ)
	}
	return nil
}

// wanted reports whether chunk data for root is something we need: an upload
// in the pool or on chain with that size.
func (h *Host) wanted(root Hash, size uint64) bool {
	if info, ok := h.cfg.Chain.Tip().Data.Get(root); ok {
		return info.Size == size
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, w := range h.waiting {
		if w.DataRoot == root && w.Size == size {
			return true
		}
	}
	return false
}

// maxTextsPerMsg bounds the bodies asked for or sent in one message.
const maxTextsPerMsg = 512

// parkedBlock is a recent block waiting for post bodies.
type parkedBlock struct {
	blk  *types.Block
	raw  []byte
	from *peer
	at   time.Time
}

func (h *Host) textFor(x Hash) ([]byte, bool) {
	if h.cfg.Text != nil {
		return h.cfg.Text(x)
	}
	return h.cfg.Store.Text(x)
}

// park keeps a block until its bodies arrive and asks for them.
func (h *Host) park(blk *types.Block, raw []byte, missing []Hash, from *peer) {
	h.mu.Lock()
	if len(h.parked) < maxOrphanBlocks {
		h.parked[blk.Hash()] = &parkedBlock{blk: blk, raw: raw, from: from, at: time.Now()}
	}
	h.mu.Unlock()
	h.FetchTexts(missing, from)
}

// retryParked offers parked blocks to the chain again; ones still missing
// bodies ask again, and ones parked too long are dropped (sync refetches).
func (h *Host) retryParked() {
	h.mu.Lock()
	var ps []*parkedBlock
	for id, pb := range h.parked {
		if time.Since(pb.at) > 10*time.Minute {
			delete(h.parked, id)
			continue
		}
		ps = append(ps, pb)
	}
	h.mu.Unlock()
	sort.Slice(ps, func(i, j int) bool { return ps[i].blk.Height < ps[j].blk.Height })
	for _, pb := range ps {
		err := h.cfg.Chain.Add(pb.blk)
		var mt *chain.MissingTextsError
		if errors.As(err, &mt) {
			h.FetchTexts(mt.Hashes, nil)
			continue
		}
		h.mu.Lock()
		delete(h.parked, pb.blk.Hash())
		h.mu.Unlock()
		if err == nil {
			h.broadcast(msgBlock, pb.raw, pb.from)
		}
	}
}

// FetchTexts asks peers for post bodies: the preferred peer first, then
// everyone. A body asked for in the last few seconds is not asked again.
func (h *Host) FetchTexts(hs []Hash, prefer *peer) {
	h.mu.Lock()
	var want []Hash
	for _, x := range hs {
		if at, ok := h.askedTxt[x]; ok && time.Since(at) < refetchAfter {
			continue
		}
		h.askedTxt[x] = time.Now()
		want = append(want, x)
	}
	if len(h.askedTxt) > 100_000 {
		for k, at := range h.askedTxt {
			if time.Since(at) > time.Minute {
				delete(h.askedTxt, k)
			}
		}
	}
	h.mu.Unlock()
	for len(want) > 0 {
		n := min(len(want), maxTextsPerMsg)
		var w codec.Writer
		w.Var(uint64(n))
		for _, x := range want[:n] {
			w.Fixed(x[:])
		}
		if prefer != nil {
			prefer.send(msgGetTexts, w.B)
		}
		h.broadcast(msgGetTexts, w.B, prefer)
		want = want[n:]
	}
}

// maxOrphanBlocks bounds parked blocks.
const maxOrphanBlocks = 512
