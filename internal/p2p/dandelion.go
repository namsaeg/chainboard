package p2p

import (
	"errors"
	"fmt"
	mrand "math/rand"
	"time"

	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/mempool"
	"github.com/namsaeg/chainboard/internal/types"
)

// receiveFluff handles a broadcast transaction.
func (h *Host) receiveFluff(from *peer, t *types.Tx, raw []byte) {
	id := t.ID()
	if !h.markSeen(id) {
		h.cancelEmbargo(id)
		return
	}
	h.cancelEmbargo(id)
	if err := h.cfg.Pool.Add(t); err != nil {
		if errors.Is(err, mempool.ErrWaitingData) {
			h.wait(t, from)
		}
		return
	}
	h.broadcast(msgTx, raw, from)
}

// receiveStem forwards a stem-phase transaction to our stem peer, or with a
// small probability starts the fluff phase. An embargo timer fluffs it anyway
// if it does not come back as fluff in time, so a dropped stem cannot censor.
func (h *Host) receiveStem(t *types.Tx, raw []byte) {
	if err := t.CheckStateless(); err != nil {
		return
	}
	id := t.ID()
	h.mu.Lock()
	if _, ok := h.embargo[id]; ok {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	next := h.stemPeer()
	if next == nil || mrand.Intn(100) < fluffChance {
		h.fluff(t, raw)
		return
	}
	h.armEmbargo(t, raw)
	next.send(msgStemTx, raw)
}

// SubmitOwn sends one of our own transactions. It never enters our pool
// first: it leaves through the stem like any relayed transaction, so peers
// cannot tell it was created here.
func (h *Host) SubmitOwn(t *types.Tx) error {
	if err := t.CheckStateless(); err != nil {
		return err
	}
	raw := t.EncodeWire()
	next := h.stemPeer()
	if next == nil {
		// Alone on the network: nothing to hide from.
		return h.fluffErr(t, raw)
	}
	h.armEmbargo(t, raw)
	next.send(msgStemTx, raw)
	return nil
}

func (h *Host) fluff(t *types.Tx, raw []byte) { _ = h.fluffErr(t, raw) }

func (h *Host) fluffErr(t *types.Tx, raw []byte) error {
	id := t.ID()
	h.cancelEmbargo(id)
	h.markSeen(id)
	if err := h.cfg.Pool.Add(t); err != nil {
		if errors.Is(err, mempool.ErrWaitingData) {
			h.wait(t, nil)
			return nil
		}
		return err
	}
	h.broadcast(msgTx, raw, nil)
	return nil
}

func (h *Host) armEmbargo(t *types.Tx, raw []byte) {
	d := h.cfg.Embargo
	if d == 0 {
		d = embargoMin + time.Duration(mrand.Int63n(int64(embargoJitter)))
	}
	id := t.ID()
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.embargo[id]; ok {
		return
	}
	h.embargo[id] = time.AfterFunc(d, func() {
		h.mu.Lock()
		delete(h.embargo, id)
		h.mu.Unlock()
		if !h.cfg.Pool.Has(id) {
			h.fluff(t, raw)
		}
	})
}

func (h *Host) cancelEmbargo(id Hash) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if tm, ok := h.embargo[id]; ok {
		tm.Stop()
		delete(h.embargo, id)
	}
}

// stemPeer is one outbound peer kept for a whole epoch, as Dandelion++ asks
// (a per-epoch route resists intersection attacks better than per-transaction).
func (h *Host) stemPeer() *peer {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stem != nil && h.peers[h.stem] && time.Since(h.stemAt) < stemEpoch {
		return h.stem
	}
	var outs []*peer
	for p := range h.peers {
		if p.out {
			outs = append(outs, p)
		}
	}
	if len(outs) == 0 {
		for p := range h.peers {
			outs = append(outs, p)
		}
	}
	if len(outs) == 0 {
		h.stem = nil
		return nil
	}
	h.stem = outs[mrand.Intn(len(outs))]
	h.stemAt = time.Now()
	return h.stem
}

// maxWaiting bounds uploads parked until their data arrives.
const maxWaiting = 1024

// refetchAfter is how long a chunk request may go unanswered before the
// chunk is asked for again (from a randomly chosen peer).
const refetchAfter = 3 * time.Second

// wait parks an upload until its chunks arrive and asks peers for them.
func (h *Host) wait(t *types.Tx, from *peer) {
	h.mu.Lock()
	if len(h.waiting) >= maxWaiting {
		h.mu.Unlock()
		return
	}
	h.waiting[t.ID()] = t
	h.mu.Unlock()
	if _, ok := h.cfg.Store.Size(t.DataRoot); !ok {
		_ = h.cfg.Store.Begin(t.DataRoot, t.Size)
	}
	h.FetchData(t.DataRoot, from)
}

// FetchData asks peers for every missing chunk of a data root.
func (h *Host) FetchData(root Hash, prefer *peer) {
	missing, ok := h.cfg.Store.Missing(root)
	if !ok || len(missing) == 0 {
		return
	}
	h.mu.Lock()
	var ps []*peer
	for p := range h.peers {
		ps = append(ps, p)
	}
	h.mu.Unlock()
	if len(ps) == 0 {
		return
	}
	mrand.Shuffle(len(ps), func(i, j int) { ps[i], ps[j] = ps[j], ps[i] })
	for i, idx := range missing {
		key := fmt.Sprintf("%s:%d", root, idx)
		h.mu.Lock()
		if last, ok := h.fetching[key]; ok && time.Since(last) < refetchAfter {
			h.mu.Unlock()
			continue
		}
		h.fetching[key] = time.Now()
		h.mu.Unlock()
		p := ps[i%len(ps)]
		if prefer != nil && i%2 == 0 {
			p = prefer
		}
		var w codec.Writer
		w.Fixed(root[:])
		w.Var(idx)
		p.send(msgGetChunk, w.B)
	}
}

// FetchChunks requests specific chunks (used by mining to fill partitions).
func (h *Host) FetchChunks(want map[Hash][]uint64) {
	for root := range want {
		if _, ok := h.cfg.Store.Size(root); !ok {
			info, ok := h.cfg.Chain.Tip().Data[root]
			if !ok {
				continue
			}
			if err := h.cfg.Store.Begin(root, info.Size); err != nil {
				continue
			}
		}
		h.FetchData(root, nil)
	}
}

// refetchWaiting asks again for the data of parked uploads. A peer that
// lacks a chunk ignores the request, so unanswered requests are retried.
func (h *Host) refetchWaiting() {
	h.mu.Lock()
	roots := map[Hash]bool{}
	for _, t := range h.waiting {
		roots[t.DataRoot] = true
	}
	for k, at := range h.fetching {
		if time.Since(at) > time.Minute {
			delete(h.fetching, k)
		}
	}
	h.mu.Unlock()
	for r := range roots {
		h.FetchData(r, nil)
		h.retryWaiting(r)
	}
}

// retryWaiting re-submits uploads whose data is now complete.
func (h *Host) retryWaiting(root Hash) {
	if !h.cfg.Store.Complete(root) {
		return
	}
	h.mu.Lock()
	var ready []*types.Tx
	for id, t := range h.waiting {
		if t.DataRoot == root {
			ready = append(ready, t)
			delete(h.waiting, id)
		}
	}
	h.mu.Unlock()
	for _, t := range ready {
		if h.cfg.Pool.Add(t) == nil {
			h.broadcast(msgTx, t.EncodeWire(), nil)
		}
	}
}

// Publish broadcasts a transaction at once, skipping the stem phase. It is
// for transactions that need no sender privacy, such as slashes.
func (h *Host) Publish(t *types.Tx) error {
	if err := t.CheckStateless(); err != nil {
		return err
	}
	return h.fluffErr(t, t.EncodeWire())
}

// WaitingUploads counts upload transactions parked until their data arrives.
func (h *Host) WaitingUploads() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.waiting)
}
