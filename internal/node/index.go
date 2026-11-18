package node

import (
	"sort"
	"sync"

	"github.com/namsaeg/chainboard/internal/chain"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/types"
)

// PostView is a post as the UI shows it.
type PostView struct {
	ID       string   `json:"id"`
	Board    string   `json:"board"`
	Text     string   `json:"text"`
	Author   string   `json:"author"`
	Parent   string   `json:"parent,omitempty"`
	Attach   []string `json:"attach,omitempty"`
	Height   uint64   `json:"height"`
	Time     int64    `json:"time"`
	Likes    uint64   `json:"likes"`
	Comments uint64   `json:"comments"`
	Lane     string   `json:"lane"`                // "anon" or "fee"
	Pseudo   bool     `json:"pseudonym,omitempty"` // author is a pseudonym key
	Pending  bool     `json:"pending,omitempty"`
	// Retention of the text: tier code, the height it is kept until (0 is
	// permanent) and whether the body is here ("ok"), being fetched
	// ("loading") or past its retention ("expired").
	Tier      uint16 `json:"tier,omitempty"`
	Until     uint64 `json:"until,omitempty"`
	TextState string `json:"text_state"`

	id       cryptox.Hash
	textHash cryptox.Hash
	textLen  uint16
}

// Index keeps the posts of the best chain, rebuilt when the chain reorganises.
type Index struct {
	mu       sync.RWMutex
	path     []cryptox.Hash // block hash by height that was indexed
	posts    map[cryptox.Hash]*PostView
	byBoard  map[string][]cryptox.Hash
	comments map[cryptox.Hash][]cryptox.Hash
	byText   map[cryptox.Hash][]cryptox.Hash // text hash -> posts
	dataAt   map[cryptox.Hash]uint64         // upload and bundle roots -> height first placed
	needText []cryptox.Hash                  // posts whose bodies may still be missing
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	ix := &Index{}
	ix.reset()
	return ix
}

func (ix *Index) reset() {
	ix.path = nil
	ix.posts = map[cryptox.Hash]*PostView{}
	ix.byBoard = map[string][]cryptox.Hash{}
	ix.comments = map[cryptox.Hash][]cryptox.Hash{}
	ix.byText = map[cryptox.Hash][]cryptox.Hash{}
	ix.dataAt = map[cryptox.Hash]uint64{}
	ix.needText = nil
}

// View converts a post transaction.
func View(t *types.Tx, height uint64, tm int64) *PostView {
	v := &PostView{ID: t.ID().String(), Board: t.Board, Text: string(t.Text), Author: t.Author().String(),
		Height: height, Time: tm, Lane: "fee", Tier: t.Tier, TextState: "ok", id: t.ID(), textHash: t.TextHash, textLen: t.TextLen}
	if t.TextLen > 0 && len(t.Text) == 0 {
		v.TextState = "loading"
	}
	if t.Kind == types.TxAnonPost {
		v.Lane = "anon"
		v.Author = ""
		if len(t.PseudoPub) > 0 {
			v.Author, v.Pseudo = t.Author().String(), true
		}
	}
	if !t.Parent.IsZero() {
		v.Parent = t.Parent.String()
	}
	for _, a := range t.Attach {
		v.Attach = append(v.Attach, a.String())
	}
	return v
}

// Sync catches the index up with the best chain.
func (ix *Index) Sync(c *chain.Chain) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	h := c.Height()
	// Find the first height where our path disagrees with the chain.
	for i := range ix.path {
		b := c.BlockAt(uint64(i) + 1)
		if b == nil || b.Hash() != ix.path[i] {
			ix.reset()
			break
		}
	}
	for height := uint64(len(ix.path)) + 1; height <= h; height++ {
		b := c.BlockAt(height)
		if b == nil {
			break
		}
		for _, tb := range b.TextBundles {
			if _, ok := ix.dataAt[tb.Root]; !ok {
				ix.dataAt[tb.Root] = height
			}
		}
		for _, t := range b.Txs {
			if t.Kind == types.TxUpload {
				if _, ok := ix.dataAt[t.DataRoot]; !ok {
					ix.dataAt[t.DataRoot] = height
				}
			}
			if !t.IsPost() {
				continue
			}
			id := t.ID()
			ix.posts[id] = View(t, height, b.Time)
			if t.TextLen > 0 {
				ix.byText[t.TextHash] = append(ix.byText[t.TextHash], id)
				ix.needText = append(ix.needText, id)
			}
			if t.Parent.IsZero() {
				ix.byBoard[t.Board] = append(ix.byBoard[t.Board], id)
			} else {
				ix.comments[t.Parent] = append(ix.comments[t.Parent], id)
			}
		}
		ix.path = append(ix.path, b.Hash())
	}
	tip := c.Tip()
	for id, v := range ix.posts {
		if info, ok := tip.Posts[id]; ok {
			v.Likes, v.Comments = info.Likes, info.Comments
		}
	}
}

// postsWithText lists the posts committing to a body.
func (ix *Index) postsWithText(x cryptox.Hash) []cryptox.Hash {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return append([]cryptox.Hash(nil), ix.byText[x]...)
}

// dataHeight is when a data root was first placed (upload or seal).
func (ix *Index) dataHeight(root cryptox.Hash) (uint64, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	h, ok := ix.dataAt[root]
	return h, ok
}

// takeNeedText returns up to n posts whose bodies may be missing, oldest
// queued first; keep says whether to look at a post again later.
func (ix *Index) takeNeedText(n int) []cryptox.Hash {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	n = min(n, len(ix.needText))
	out := append([]cryptox.Hash(nil), ix.needText[:n]...)
	ix.needText = ix.needText[n:]
	return out
}

func (ix *Index) requeueNeedText(ids []cryptox.Hash) {
	ix.mu.Lock()
	ix.needText = append(ix.needText, ids...)
	ix.mu.Unlock()
}

// post returns a copy of an indexed post.
func (ix *Index) post(id cryptox.Hash) (PostView, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	v, ok := ix.posts[id]
	if !ok {
		return PostView{}, false
	}
	return *v, true
}

// Boards lists boards with their post counts.
func (ix *Index) Boards() map[string]int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := map[string]int{}
	for b, ids := range ix.byBoard {
		out[b] = len(ids)
	}
	return out
}

// Board returns the newest posts of a board; minLikes filters "개념글".
func (ix *Index) Board(board string, minLikes uint64, limit int) []PostView {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	ids := ix.byBoard[board]
	var out []PostView
	for i := len(ids) - 1; i >= 0 && len(out) < limit; i-- {
		v := ix.posts[ids[i]]
		if v.Likes >= minLikes {
			out = append(out, *v)
		}
	}
	return out
}

// Thread returns a post and its comments, oldest first.
func (ix *Index) Thread(id cryptox.Hash) (*PostView, []PostView) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	p, ok := ix.posts[id]
	if !ok {
		return nil, nil
	}
	var cs []PostView
	for _, c := range ix.comments[id] {
		cs = append(cs, *ix.posts[c])
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Height < cs[j].Height })
	cp := *p
	return &cp, cs
}

// PendingPosts lists posts not mined yet: ours still queued, travelling the
// stem or waiting to be proven again, and everyone's in the pool.
func (n *Node) PendingPosts() []PostView {
	var out []PostView
	seen := map[cryptox.Hash]bool{}
	add := func(t *types.Tx) {
		if (t.Kind != types.TxPost && t.Kind != types.TxAnonPost) || seen[t.ID()] {
			return
		}
		seen[t.ID()] = true
		v := View(t, 0, 0)
		v.Pending = true
		out = append(out, *v)
	}
	for _, t := range n.Pool.Pending() {
		add(t)
	}
	n.mu.Lock()
	var own []*types.Tx
	for _, s := range n.sent {
		own = append(own, s.tx)
	}
	for _, q := range n.queue {
		if q.tx != nil {
			own = append(own, q.tx)
		}
	}
	n.mu.Unlock()
	for _, t := range own {
		add(t)
	}
	for _, a := range n.unsentAnon() {
		v := PostView{Board: a.Board, Text: a.Text, Lane: "anon", Pseudo: a.Pseudo, Pending: true}
		if !a.Parent.IsZero() {
			v.Parent = a.Parent.String()
		}
		out = append(out, v)
	}
	return out
}
