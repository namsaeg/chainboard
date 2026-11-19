package node

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

// Post bodies live off-chain. This node keeps:
//   - every body it sees while the post is newer than CacheDays (they arrive
//     with the transactions, so recent text is local and instant);
//   - the text bundles of the partitions it stores as a miner, for as long
//     as their retention lasts.
//
// Anything older is fetched from peers when someone opens it. A background
// loop catches up on recent bodies missed while offline, and a periodic
// sweep forgets what is expired or past the cache.

const (
	textLoopEvery = 2 * time.Second
	sweepEvery    = 10 * time.Minute
	maxTextTries  = 15
)

func (n *Node) cacheBlocks() uint64 {
	days := n.cfg.TextCacheDays
	if days == 0 {
		days = 30
	}
	return days * n.P.BlocksPerDay
}

// textBody finds a body: in the text store, or cut out of a sealed bundle
// whose chunks are here.
func (n *Node) textBody(x cryptox.Hash) ([]byte, bool) {
	if b, ok := n.Store.Text(x); ok {
		return b, true
	}
	tip := n.Chain.Tip()
	for _, id := range n.Index.postsWithText(x) {
		pi, ok := tip.Posts.Get(id)
		if !ok || pi.TextRoot.IsZero() || tip.TextExpired(pi) {
			continue
		}
		if b, ok := n.cutBody(pi); ok {
			return b, true
		}
	}
	return nil, false
}

func (n *Node) cutBody(pi *state.PostInfo) ([]byte, bool) {
	start := uint64(pi.TextOff)
	end := start + uint64(pi.TextLen)
	out := make([]byte, 0, pi.TextLen)
	for ci := start / params.ChunkSize; ci*params.ChunkSize < end; ci++ {
		c, err := n.Store.Chunk(pi.TextRoot, ci)
		if err != nil {
			return nil, false
		}
		base := ci * params.ChunkSize
		lo, hi := max(start, base)-base, min(end, base+uint64(len(c)))-base
		if lo > hi || hi > uint64(len(c)) {
			return nil, false
		}
		out = append(out, c[lo:hi]...)
	}
	if uint64(len(out)) != uint64(pi.TextLen) || types.TextHashOf(out) != pi.TextHash {
		return nil, false
	}
	return out, true
}

// fillText completes a view with its body and retention.
func (n *Node) fillText(v *PostView, tip *state.State) {
	if v.textLen == 0 {
		v.TextState = "ok"
		return
	}
	pi, ok := tip.Posts.Get(v.id)
	if !ok && !v.Pending {
		// The chain drops a post's record when its body's retention ends.
		v.Text, v.TextState = "", "expired"
		return
	}
	if ok {
		v.Tier, v.Until = pi.TextTier, tip.TextUntil(pi)
		if tip.TextExpired(pi) {
			v.Text, v.TextState = "", "expired"
			return
		}
	}
	if v.Text != "" {
		v.TextState = "ok"
		return
	}
	if body, ok := n.textBody(v.textHash); ok {
		v.Text, v.TextState = string(body), "ok"
		return
	}
	v.TextState = "loading"
	n.Host.FetchTexts([]cryptox.Hash{v.textHash}, nil)
}

// Board returns a board's newest posts with their bodies.
func (n *Node) Board(name string, minLikes uint64, limit int) []PostView {
	tip := n.Chain.Tip()
	vs := n.Index.Board(name, minLikes, limit)
	for i := range vs {
		n.fillText(&vs[i], tip)
		n.fillFund(&vs[i], tip)
	}
	return vs
}

// Thread returns a post and its comments with their bodies.
func (n *Node) Thread(id cryptox.Hash) (*PostView, []PostView) {
	tip := n.Chain.Tip()
	p, cs := n.Index.Thread(id)
	if p == nil {
		return nil, nil
	}
	n.fillText(p, tip)
	n.fillFund(p, tip)
	for i := range cs {
		n.fillText(&cs[i], tip)
	}
	return p, cs
}

// textLoop fetches the bodies of recent posts this node does not have yet,
// in the background, so boards read instantly.
func (n *Node) textLoop(ctx context.Context) {
	defer n.wg.Done()
	t := time.NewTicker(textLoopEvery)
	defer t.Stop()
	last := time.Now()
	tries := map[cryptox.Hash]int{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		tip := n.Chain.Tip()
		ids := n.Index.takeNeedText(512)
		var want []cryptox.Hash
		var again []cryptox.Hash
		for _, id := range ids {
			v, ok := n.Index.post(id)
			pi, onChain := tip.Posts.Get(id)
			if !ok || !onChain || tip.TextExpired(pi) || v.Height+n.cacheBlocks() < tip.Height {
				delete(tries, id)
				continue // gone, expired, or old enough to fetch only when opened
			}
			if _, ok := n.textBody(v.textHash); ok {
				delete(tries, id)
				continue
			}
			if tries[id]++; tries[id] > maxTextTries {
				delete(tries, id)
				continue
			}
			want = append(want, v.textHash)
			again = append(again, id)
		}
		if len(want) > 0 {
			n.Host.FetchTexts(want, nil)
		}
		n.Index.requeueNeedText(again)
		if time.Since(last) > sweepEvery {
			n.sweep(tip)
			last = time.Now()
		}
	}
}

// sweep forgets bodies and data this node no longer needs: expired ones,
// and ones past the cache that it does not store as a miner. Anything not
// referenced by the chain is dropped on the second sweep that sees it, so an
// upload waiting for its transaction is not lost.
func (n *Node) sweep(tip *state.State) {
	cache := n.cacheBlocks()
	old := func(h uint64) bool { return h+cache < tip.Height }

	// Raw data: uploads and text bundles.
	live := map[cryptox.Hash]bool{}
	for _, b := range tip.Buckets {
		for i := 0; i < b.Records.Len(); i++ {
			live[b.Records.Ptr(i).DataRoot] = true
		}
	}
	kept := map[cryptox.Hash]bool{}
	if n.cfg.Mine {
		for _, p := range n.Miner.Assigned() {
			bucket, start, length, ok := tip.PartitionRange(p)
			if !ok {
				continue
			}
			// Records are in position order: find the first that reaches
			// the partition, then walk while they start inside it.
			recs := tip.Buckets[bucket].Records
			i := sort.Search(recs.Len(), func(i int) bool { r := recs.Ptr(i); return r.Start+r.Chunks > start })
			for ; i < recs.Len() && recs.Ptr(i).Start < start+length; i++ {
				kept[recs.Ptr(i).DataRoot] = true
			}
		}
	}
	pooled := map[cryptox.Hash]bool{}
	for _, t := range n.Pool.Pending() {
		if t.Kind == types.TxUpload {
			pooled[t.DataRoot] = true
		}
	}
	for _, t := range n.Queued() {
		if t.Kind == types.TxUpload {
			pooled[t.DataRoot] = true
		}
	}
	genesis, _ := n.genesisRoot()
	var dropData []cryptox.Hash
	unref := map[cryptox.Hash]bool{}
	for _, root := range n.Store.Roots() {
		switch {
		case root == genesis || kept[root] || pooled[root]:
		case !live[root]:
			if n.unrefData[root] {
				dropData = append(dropData, root)
			} else {
				unref[root] = true
			}
		default:
			if h, ok := n.Index.dataHeight(root); ok && old(h) {
				dropData = append(dropData, root)
			}
		}
	}
	n.unrefData = unref
	for _, r := range dropData {
		_ = n.Store.DeleteData(r)
	}

	// Bodies.
	var dropText []cryptox.Hash
	unrefT := map[cryptox.Hash]bool{}
	for _, x := range n.Store.TextHashes() {
		ids := n.Index.postsWithText(x)
		if len(ids) == 0 {
			if n.unrefText[x] {
				dropText = append(dropText, x)
			} else {
				unrefT[x] = true
			}
			continue
		}
		keep := false
		for _, id := range ids {
			pi, onChain := tip.Posts.Get(id)
			v, ok := n.Index.post(id)
			if onChain && ok && !tip.TextExpired(pi) && !old(v.Height) {
				keep = true
			}
		}
		if !keep {
			dropText = append(dropText, x)
		}
	}
	n.unrefText = unrefT
	if len(dropText) > 0 {
		_ = n.Store.DeleteTexts(dropText)
	}
	if len(dropData)+len(dropText) > 0 {
		n.cfg.Logf("node: freed %d data item(s) and %d post body(ies) past retention or cache", len(dropData), len(dropText))
	}
}

// ExtendPost lengthens how long a post's text is kept. Anyone may do it; it
// needs the body, which travels with the transaction to the next seal.
func (n *Node) ExtendPost(id cryptox.Hash, tier uint16, confirm bool) error {
	tip := n.Chain.Tip()
	pi, ok := tip.Posts.Get(id)
	if !ok {
		return errors.New("post not found")
	}
	if pi.TextLen == 0 {
		return errors.New("post has no text to keep")
	}
	body, ok := n.textBody(pi.TextHash)
	if !ok {
		n.Host.FetchTexts([]cryptox.Hash{pi.TextHash}, nil)
		return errors.New("this node does not have the text yet; try again in a moment")
	}
	t := &types.Tx{Kind: types.TxExtend, Target: id, Tier: tier, TextHash: pi.TextHash, TextLen: pi.TextLen, Text: body}
	return n.SendChecked(t, confirm)
}

func (n *Node) genesisRoot() (cryptox.Hash, error) {
	r, _ := chunk.Root(n.Chain.GenesisData)
	return r, nil
}

// keepBody stores the body of an own transaction before it leaves.
func (n *Node) keepBody(t *types.Tx) error {
	if !t.NeedsBody() {
		return nil
	}
	if err := t.CheckBody(); err != nil {
		return err
	}
	if _, err := n.Store.PutText(t.Text); err != nil {
		return fmt.Errorf("keeping post text: %w", err)
	}
	return nil
}
