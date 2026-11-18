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
	Lane     string   `json:"lane"`
	Pending  bool     `json:"pending,omitempty"`
}

// Index keeps the posts of the best chain, rebuilt when the chain reorganises.
type Index struct {
	mu       sync.RWMutex
	path     []cryptox.Hash // block hash by height that was indexed
	posts    map[cryptox.Hash]*PostView
	byBoard  map[string][]cryptox.Hash
	comments map[cryptox.Hash][]cryptox.Hash
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
}

// View converts a post transaction.
func View(t *types.Tx, height uint64, tm int64) *PostView {
	v := &PostView{ID: t.ID().String(), Board: t.Board, Text: string(t.Text), Author: t.Sender().String(),
		Height: height, Time: tm, Lane: "fee"}
	if t.Lane == types.LaneFree {
		v.Lane = "free"
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
		for _, t := range b.Txs {
			if t.Kind != types.TxPost {
				continue
			}
			id := t.ID()
			ix.posts[id] = View(t, height, b.Time)
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
