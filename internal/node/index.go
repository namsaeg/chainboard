package node

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"

	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/params"

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
	Tips     uint64   `json:"tips"`                // coins tipped to the author (base units)
	TipTo    string   `json:"tip_to,omitempty"`    // where its likes (tips) go; empty: it takes none
	Liked    bool     `json:"liked,omitempty"`     // this node's like is counted
	Lane     string   `json:"lane"`                // "anon" or "fee"
	Pseudo   bool     `json:"pseudonym,omitempty"` // author is a pseudonym key
	Pending  bool     `json:"pending,omitempty"`
	// Retention of the text: tier code, the height it is kept until (0 is
	// permanent) and whether the body is here ("ok"), being fetched
	// ("loading") or past its retention ("expired").
	Tier      uint16 `json:"tier,omitempty"`
	Until     uint64 `json:"until,omitempty"`
	TextState string `json:"text_state"`
	// Fund is set on a funding request (its note is the text).
	Fund *FundView `json:"fund,omitempty"`
	// Kept says, per attachment, how long it is stored (same order).
	Kept []Retention `json:"kept,omitempty"`

	id       cryptox.Hash
	textHash cryptox.Hash
	textLen  uint16
}

// LikeWindowDays is how long after a post likes of it count. Likes are not
// in the chain state, so this is index policy, the same on every node.
const LikeWindowDays = 30

// Index keeps the posts of the best chain on disk (index.db), so memory
// does not grow with every post ever written. Each new block costs work in
// proportion to that block; a reorganisation undoes only the blocks it
// replaces. Likes are not in the chain state: the index resolves them,
// keeps one per account and post, and counts them, as every node does the
// same from the same blocks.
type Index struct {
	db       *bolt.DB
	sync     sync.Mutex // one Sync at a time
	mu       sync.Mutex // needText
	needText []cryptox.Hash
	// RecentBlocks: posts newer than this many blocks behind the tip are
	// queued for the background body fetcher when indexed.
	RecentBlocks uint64
	// LikeWindow: a like counts only within this many blocks of its post.
	LikeWindow uint64
}

var (
	ixPath     = []byte("path")     // height -> block hash
	ixPosts    = []byte("posts")    // post id -> record
	ixBoard    = []byte("board")    // len(board) board height id -> nil
	ixCount    = []byte("count")    // board -> number of posts
	ixComments = []byte("comments") // parent height id -> nil
	ixText     = []byte("text")     // text hash, post id -> nil
	ixData     = []byte("data")     // data root -> height first placed
	ixUndo     = []byte("undo")     // height -> what that block added
	ixAt       = []byte("at")       // height id -> nil (resolves like references)
	ixLiked    = []byte("liked")    // post id, account -> 1
	ixByAuthor = []byte("byauthor") // owner, height, post id -> 1 (see postRec.owner)
	ixByLiker  = []byte("byliker")  // account, post id -> 1 (counted likes)
	ixStats    = []byte("stats")    // post id -> likes (u64) comments (u64)
	ixMeta     = []byte("meta")     // "chain" -> format and genesis
)

// indexFormat changes whenever what the index stores changes; an index of
// another format or chain is rebuilt.
const indexFormat = "6"

// OpenIndex opens (or creates) the index database of the chain with the
// given genesis. The index is derived from the chain, so it skips fsync, and
// a file that does not open (after a crash, say), or that belongs to another
// chain or format, is deleted and rebuilt by the next Sync.
func OpenIndex(path string, genesis cryptox.Hash) (*Index, error) {
	opts := &bolt.Options{Timeout: 5 * time.Second, NoSync: true, NoFreelistSync: true}
	db, err := bolt.Open(path, 0o600, opts)
	if err != nil && !errors.Is(err, bolterrors.ErrTimeout) {
		_ = os.Remove(path)
		db, err = bolt.Open(path, 0o600, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	mark := append([]byte(indexFormat), genesis[:]...)
	var stale bool
	_ = db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket(ixMeta); b != nil && !bytes.Equal(b.Get([]byte("chain")), mark) {
			stale = true
		} else if b == nil && tx.Bucket(ixPath) != nil {
			stale = true
		}
		return nil
	})
	if stale {
		db.Close()
		_ = os.Remove(path)
		if db, err = bolt.Open(path, 0o600, opts); err != nil {
			return nil, fmt.Errorf("index: %w", err)
		}
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{ixPath, ixPosts, ixBoard, ixCount, ixComments, ixText, ixData, ixUndo, ixAt, ixLiked, ixStats, ixMeta, ixByAuthor, ixByLiker} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return tx.Bucket(ixMeta).Put([]byte("chain"), mark)
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Index{db: db}, nil
}

// Close closes the database.
func (ix *Index) Close() error { return ix.db.Close() }

func u64(x uint64) []byte { return binary.BigEndian.AppendUint64(nil, x) }

func boardKey(board string, height uint64, id cryptox.Hash) []byte {
	k := append([]byte{byte(len(board))}, board...)
	k = binary.BigEndian.AppendUint64(k, height)
	return append(k, id[:]...)
}

func commentKey(parent cryptox.Hash, height uint64, id cryptox.Hash) []byte {
	k := append(parent[:0:0], parent[:]...)
	k = binary.BigEndian.AppendUint64(k, height)
	return append(k, id[:]...)
}

// Retention is how long something is stored: the heights it was stored
// at and is kept until (0: permanent; Gone: no longer stored).
type Retention struct {
	Root  string `json:"root,omitempty"`
	Tier  uint16 `json:"tier"`
	From  uint64 `json:"from"`
	Until uint64 `json:"until"`
	Gone  bool   `json:"gone,omitempty"`
}

// postRec is what the index stores per post.
type postRec struct {
	ID       cryptox.Hash
	Board    string
	Author   cryptox.Address
	Parent   cryptox.Hash
	Height   uint64
	Time     int64
	Anon     bool
	Pseudo   bool
	Attach   []cryptox.Hash
	TextHash cryptox.Hash
	TextLen  uint16
	Tier     uint16
	Fund     bool            // a funding request; Note is its text
	Note     string          // funding request or bounty
	Bounty   bool            // a bounty; Note is its text, Attach its references
	TipTo    cryptox.Address // anonymous post: the one-time address its tips go to
}

func (r *postRec) encode() []byte {
	var w codec.Writer
	w.Bytes([]byte(r.Board))
	w.Fixed(r.Author[:])
	w.Fixed(r.Parent[:])
	w.Var(r.Height)
	w.Var(uint64(r.Time))
	w.Bool(r.Anon)
	w.Bool(r.Pseudo)
	w.Var(uint64(len(r.Attach)))
	for _, a := range r.Attach {
		w.Fixed(a[:])
	}
	w.Fixed(r.TextHash[:])
	w.U16(r.TextLen)
	w.U16(r.Tier)
	w.Bool(r.Fund)
	w.Bytes([]byte(r.Note))
	w.Bool(r.Bounty)
	w.Fixed(r.TipTo[:])
	return w.B
}

func decodeRec(id cryptox.Hash, b []byte) (*postRec, error) {
	r := &postRec{ID: id}
	rd := codec.NewReader(b)
	r.Board = string(rd.Bytes(params.MaxBoardBytes))
	rd.Fixed(r.Author[:])
	rd.Fixed(r.Parent[:])
	r.Height = rd.Var()
	r.Time = int64(rd.Var())
	r.Anon = rd.Bool()
	r.Pseudo = rd.Bool()
	n := rd.Count(params.MaxAttachments)
	for i := 0; i < n && rd.Err == nil; i++ {
		var a cryptox.Hash
		rd.Fixed(a[:])
		r.Attach = append(r.Attach, a)
	}
	rd.Fixed(r.TextHash[:])
	r.TextLen = rd.U16()
	r.Tier = rd.U16()
	r.Fund = rd.Bool()
	r.Note = string(rd.Bytes(types.MaxBountyNote))
	r.Bounty = rd.Bool()
	rd.Fixed(r.TipTo[:])
	return r, rd.Done()
}

// fundRec is how a funding request is listed: as a post of its board.
func fundRec(q *types.FundRequest, height uint64, tm int64) *postRec {
	return &postRec{ID: q.ID(), Board: q.Board, Author: q.CreatorAddr(), Height: height, Time: tm,
		Attach: []cryptox.Hash{q.DataRoot}, Fund: true, Note: q.Note}
}

func recOf(t *types.Tx, height uint64, tm int64) *postRec {
	r := &postRec{ID: t.ID(), Board: t.Board, Author: t.Author(), Parent: t.Parent, Height: height, Time: tm,
		Anon: t.Kind == types.TxAnonPost, Pseudo: len(t.PseudoPub) > 0, Attach: t.Attach,
		TextHash: t.TextHash, TextLen: t.TextLen, Tier: t.Tier, TipTo: t.TipTo}
	return r
}

// bountyRec is how a bounty is listed: as a post of its board.
func bountyRec(t *types.Tx, height uint64, tm int64) *postRec {
	return &postRec{ID: t.ID(), Board: t.Board, Author: t.Sender(), Height: height, Time: tm,
		Attach: t.Attach, Bounty: true, Note: t.Note}
}

// tipTo is where a post's tips count: its signer, its pseudonym, or the
// one-time address an anonymous post names (zero: it cannot be tipped).
func (r *postRec) tipTo() cryptox.Address {
	switch {
	case r.Fund || r.Bounty:
		return cryptox.Address{}
	case r.Anon && !r.Pseudo:
		return r.TipTo
	}
	return r.Author
}

// owner is the address a post is listed under for its writer: the signer
// or pseudonym, or for an anonymous post its one-time tip address (public
// on chain, so listing by it tells nothing new).
func (r *postRec) owner() cryptox.Address {
	if !r.Author.IsZero() {
		return r.Author
	}
	return r.TipTo
}

func (r *postRec) view() PostView {
	v := PostView{ID: r.ID.String(), Board: r.Board, Author: r.Author.String(), Height: r.Height, Time: r.Time,
		Lane: "fee", Tier: r.Tier, TextState: "ok", id: r.ID, textHash: r.TextHash, textLen: r.TextLen}
	if r.Fund {
		v.Text, v.Lane = r.Note, "fund"
	}
	if to := r.tipTo(); !to.IsZero() {
		v.TipTo = to.String()
	}
	if r.Bounty {
		v.Text, v.Lane = r.Note, "bounty"
	}
	if r.Anon {
		v.Lane, v.Author = "anon", ""
		if r.Pseudo {
			v.Author, v.Pseudo = r.Author.String(), true
		}
	}
	if !r.Parent.IsZero() {
		v.Parent = r.Parent.String()
	}
	for _, a := range r.Attach {
		v.Attach = append(v.Attach, a.String())
	}
	return v
}

// View converts a post transaction (one not indexed yet: pending).
func View(t *types.Tx, height uint64, tm int64) *PostView {
	v := recOf(t, height, tm).view()
	v.Text = string(t.Text)
	if t.TextLen > 0 && len(t.Text) == 0 {
		v.TextState = "loading"
	}
	return &v
}

// Sync catches the index up with the best chain: it walks back from its
// last indexed block to where it agrees with the chain (usually zero
// steps), undoes the blocks after that, and indexes the new ones.
func (ix *Index) Sync(c blockSource) error {
	ix.sync.Lock()
	defer ix.sync.Unlock()
	tipH := c.Height()
	var last uint64
	var fork uint64
	err := ix.db.View(func(tx *bolt.Tx) error {
		if k, _ := tx.Bucket(ixPath).Cursor().Last(); k != nil {
			last = binary.BigEndian.Uint64(k)
		}
		path := tx.Bucket(ixPath)
		for fork = min(last, tipH); fork > 0; fork-- {
			b, v := c.BlockAt(fork), path.Get(u64(fork))
			if b != nil && v != nil {
				if bh := b.Hash(); bytes.Equal(v, bh[:]) {
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if fork < last {
		if err := ix.db.Update(func(tx *bolt.Tx) error {
			for h := last; h > fork; h-- {
				if err := undoHeight(tx, h); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	// Index in transactions of about a thousand posts: bbolt splits nodes
	// only at commit, so one huge transaction slows down quadratically.
	const batchPosts = 1000
	for h := fork + 1; h <= tipH; {
		var recent []cryptox.Hash
		err := ix.db.Update(func(tx *bolt.Tx) error {
			for posts := 0; h <= tipH && posts < batchPosts; h++ {
				b := c.BlockAt(h)
				if b == nil {
					h = tipH + 1
					return nil
				}
				ids, n, err := indexBlock(tx, b, h, ix.LikeWindow)
				if err != nil {
					return err
				}
				posts += n + 1
				if h+ix.RecentBlocks >= tipH {
					recent = append(recent, ids...)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		ix.mu.Lock()
		ix.needText = append(ix.needText, recent...)
		ix.mu.Unlock()
	}
	return nil
}

// blockSource is what Sync reads: the best chain's blocks by height.
type blockSource interface {
	Height() uint64
	BlockAt(uint64) *types.Block
}

// Undo records of a block, in the order its changes were made.
const (
	undoData = 1 // data root first placed
	undoPost = 2 // post indexed
	undoLike = 3 // like counted: post id, account
	undoTip  = 5 // tip counted: post id, amount
)

// indexBlock records one block and remembers how to undo it. It returns
// the posts with text and how many posts it indexed.
func indexBlock(tx *bolt.Tx, b *types.Block, h, likeWindow uint64) ([]cryptox.Hash, int, error) {
	var undo codec.Writer
	var withText []cryptox.Hash
	boards := map[string]int64{}
	count := 0
	data := tx.Bucket(ixData)
	newData := func(root cryptox.Hash) {
		if data.Get(root[:]) == nil {
			_ = data.Put(root[:], u64(h))
			undo.U8(undoData)
			undo.Fixed(root[:])
		}
	}
	for _, tb := range b.TextBundles {
		newData(tb.Root)
	}
	for _, t := range b.Txs {
		switch {
		case t.Kind == types.TxUpload:
			newData(t.DataRoot)
		case t.Kind == types.TxLikes:
			from := t.Sender()
			for _, q := range t.Requests {
				r := fundRec(q, h, b.Time)
				if tx.Bucket(ixPosts).Get(r.ID[:]) != nil {
					continue // carried before
				}
				if err := putPost(tx, r); err != nil {
					return nil, 0, err
				}
				count++
				boards[r.Board]++
				undo.U8(undoPost)
				undo.Fixed(r.ID[:])
			}
			for _, p := range t.Pledges { // a pledge is also a like
				if tx.Bucket(ixPosts).Get(p.Request[:]) == nil {
					continue
				}
				if err := countLike(tx, &undo, undoLike, p.Request, from[:]); err != nil {
					return nil, 0, err
				}
			}
			for _, p := range t.Tips {
				if err := countTip(tx, &undo, p, from, h, likeWindow); err != nil {
					return nil, 0, err
				}
			}
		case t.Kind == types.TxBounty:
			r := bountyRec(t, h, b.Time)
			if err := putPost(tx, r); err != nil {
				return nil, 0, err
			}
			count++
			boards[r.Board]++
			undo.U8(undoPost)
			undo.Fixed(r.ID[:])
		case t.IsPost():
			count++
			r := recOf(t, h, b.Time)
			if err := putPost(tx, r); err != nil {
				return nil, 0, err
			}
			if r.Parent.IsZero() {
				boards[r.Board]++
			}
			undo.U8(undoPost)
			undo.Fixed(r.ID[:])
			if r.TextLen > 0 {
				withText = append(withText, r.ID)
			}
		}
	}
	for board, n := range boards {
		if err := addCount(tx, board, n); err != nil {
			return nil, 0, err
		}
	}
	if err := tx.Bucket(ixUndo).Put(u64(h), undo.B); err != nil {
		return nil, 0, err
	}
	bh := b.Hash()
	return withText, count, tx.Bucket(ixPath).Put(u64(h), bh[:])
}

func authorKey(a cryptox.Address, height uint64, id cryptox.Hash) []byte {
	return append(append(append(make([]byte, 0, 60), a[:]...), u64(height)...), id[:]...)
}

// ByAuthors lists the posts of some writers (signer, pseudonym or the tip
// address of an anonymous post), newest first.
func (ix *Index) ByAuthors(as []cryptox.Address, limit int) []PostView {
	var out []PostView
	_ = ix.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(ixByAuthor).Cursor()
		for _, a := range as {
			prefix := a[:]
			k, _ := c.Seek(append(append([]byte(nil), prefix...), bytes.Repeat([]byte{0xff}, 40)...))
			if k == nil {
				k, _ = c.Last()
			} else {
				k, _ = c.Prev()
			}
			for n := 0; k != nil && bytes.HasPrefix(k, prefix) && n < limit; k, _ = c.Prev() {
				var id cryptox.Hash
				copy(id[:], k[28:])
				if r, ok := ix.get(tx, id); ok {
					v := r.view()
					v.setStats(stats(tx, id))
					out = append(out, v)
					n++
				}
			}
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Height > out[j].Height })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// LikedPosts lists the posts an account's counted likes (tips and pledges)
// went to.
func (ix *Index) LikedPosts(a cryptox.Address, limit int) []PostView {
	var out []PostView
	_ = ix.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(ixByLiker).Cursor()
		for k, _ := c.Seek(a[:]); k != nil && bytes.HasPrefix(k, a[:]) && len(out) < limit; k, _ = c.Next() {
			var id cryptox.Hash
			copy(id[:], k[20:])
			if r, ok := ix.get(tx, id); ok {
				v := r.view()
				v.setStats(stats(tx, id))
				out = append(out, v)
			}
		}
		return nil
	})
	return out
}

func atKey(height uint64, id cryptox.Hash) []byte { return append(u64(height), id[:]...) }

// resolveRef finds the post a like reference names. Two posts of one block
// sharing the four-byte prefix make the reference ambiguous: it counts for
// neither (about one full block in a hundred million).
func resolveRef(tx *bolt.Tx, ref types.LikeRef) (cryptox.Hash, bool) {
	prefix := append(u64(ref.Height), ref.Prefix[:]...)
	c := tx.Bucket(ixAt).Cursor()
	k, _ := c.Seek(prefix)
	if k == nil || !bytes.HasPrefix(k, prefix) {
		return cryptox.Hash{}, false
	}
	var id cryptox.Hash
	copy(id[:], k[8:])
	if k2, _ := c.Next(); k2 != nil && bytes.HasPrefix(k2, prefix) {
		return cryptox.Hash{}, false
	}
	return id, true
}

// likedKey names one like: the post and the account that liked it.
func likedKey(id cryptox.Hash, who []byte) []byte {
	return append(append(make([]byte, 0, 64), id[:]...), who...)
}

// countLike counts one like of a post once per liker, noting how to undo it.
func countLike(tx *bolt.Tx, undo *codec.Writer, kind uint8, id cryptox.Hash, who []byte) error {
	lb := tx.Bucket(ixLiked)
	k := likedKey(id, who)
	if lb.Get(k) != nil {
		return nil
	}
	if err := lb.Put(k, []byte{1}); err != nil { // a nil value reads back as absent
		return err
	}
	if err := tx.Bucket(ixByLiker).Put(append(append([]byte(nil), who...), id[:]...), []byte{1}); err != nil {
		return err
	}
	undo.U8(kind)
	undo.Fixed(k)
	return addStat(tx, id, 1, 0, 0)
}

// countTip counts a tip for its post when it went to the post's author: the
// coins always (whenever they came), the like within the like window.
func countTip(tx *bolt.Tx, undo *codec.Writer, p types.Tip, from cryptox.Address, h, likeWindow uint64) error {
	id, ok := resolveRef(tx, p.Ref)
	if !ok {
		return nil
	}
	r, err := decodeRec(id, tx.Bucket(ixPosts).Get(id[:]))
	if err != nil || r.tipTo().IsZero() || r.tipTo() != p.To {
		return nil
	}
	if err := addStat(tx, id, 0, 0, int64(p.Amount)); err != nil {
		return err
	}
	undo.U8(undoTip)
	undo.Fixed(id[:])
	undo.Fixed(u64(p.Amount))
	if likeWindow > 0 && h > p.Ref.Height+likeWindow {
		return nil
	}
	return countLike(tx, undo, undoLike, id, from[:])
}

// addStat moves a post's like and comment counts.
// postStats are a post's counts: likes, comments and coins tipped.
type postStats struct{ likes, comments, tips uint64 }

func addStat(tx *bolt.Tx, id cryptox.Hash, likes, comments, tips int64) error {
	sb := tx.Bucket(ixStats)
	st := stats(tx, id)
	st.likes = uint64(int64(st.likes) + likes)
	st.comments = uint64(int64(st.comments) + comments)
	st.tips = uint64(int64(st.tips) + tips)
	if st == (postStats{}) {
		return sb.Delete(id[:])
	}
	return sb.Put(id[:], binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(u64(st.likes), st.comments), st.tips))
}

func stats(tx *bolt.Tx, id cryptox.Hash) postStats {
	if v := tx.Bucket(ixStats).Get(id[:]); len(v) == 24 {
		return postStats{binary.BigEndian.Uint64(v), binary.BigEndian.Uint64(v[8:]), binary.BigEndian.Uint64(v[16:])}
	}
	return postStats{}
}

func (v *PostView) setStats(st postStats) {
	v.Likes, v.Comments, v.Tips = st.likes, st.comments, st.tips
}

func putPost(tx *bolt.Tx, r *postRec) error {
	if err := tx.Bucket(ixPosts).Put(r.ID[:], r.encode()); err != nil {
		return err
	}
	if err := tx.Bucket(ixAt).Put(atKey(r.Height, r.ID), nil); err != nil {
		return err
	}
	if r.TextLen > 0 {
		if err := tx.Bucket(ixText).Put(append(r.TextHash[:], r.ID[:]...), nil); err != nil {
			return err
		}
	}
	if o := r.owner(); !o.IsZero() {
		if err := tx.Bucket(ixByAuthor).Put(authorKey(o, r.Height, r.ID), []byte{1}); err != nil {
			return err
		}
	}
	if !r.Parent.IsZero() {
		if err := addStat(tx, r.Parent, 0, 1, 0); err != nil {
			return err
		}
		return tx.Bucket(ixComments).Put(commentKey(r.Parent, r.Height, r.ID), nil)
	}
	return tx.Bucket(ixBoard).Put(boardKey(r.Board, r.Height, r.ID), nil)
}

func addCount(tx *bolt.Tx, board string, d int64) error {
	cb := tx.Bucket(ixCount)
	var n uint64
	if v := cb.Get([]byte(board)); v != nil {
		n = binary.BigEndian.Uint64(v)
	}
	n = uint64(int64(n) + d)
	if n == 0 {
		return cb.Delete([]byte(board))
	}
	return cb.Put([]byte(board), u64(n))
}

// undoHeight removes what the block at height h added, newest change first.
func undoHeight(tx *bolt.Tx, h uint64) error {
	type change struct {
		kind uint8
		id   cryptox.Hash
		who  []byte
	}
	var changes []change
	raw := tx.Bucket(ixUndo).Get(u64(h))
	rd := codec.NewReader(raw)
	for rd.Err == nil && len(raw) > 0 && !rd.AtEnd() {
		c := change{kind: rd.U8()}
		rd.Fixed(c.id[:])
		switch c.kind {
		case undoLike:
			c.who = make([]byte, len(cryptox.Address{}))
		case undoTip:
			c.who = make([]byte, 8)
		}
		rd.Fixed(c.who)
		changes = append(changes, c)
	}
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		var err error
		switch c.kind {
		case undoData:
			err = tx.Bucket(ixData).Delete(c.id[:])
		case undoLike:
			if err = tx.Bucket(ixLiked).Delete(likedKey(c.id, c.who)); err == nil {
				_ = tx.Bucket(ixByLiker).Delete(append(append([]byte(nil), c.who...), c.id[:]...))
				err = addStat(tx, c.id, -1, 0, 0)
			}
		case undoTip:
			err = addStat(tx, c.id, 0, 0, -int64(binary.BigEndian.Uint64(c.who)))
		case undoPost:
			err = removePost(tx, c.id)
		}
		if err != nil {
			return err
		}
	}
	if err := tx.Bucket(ixUndo).Delete(u64(h)); err != nil {
		return err
	}
	return tx.Bucket(ixPath).Delete(u64(h))
}

func removePost(tx *bolt.Tx, x cryptox.Hash) error {
	v := tx.Bucket(ixPosts).Get(x[:])
	if v == nil {
		return nil
	}
	r, err := decodeRec(x, v)
	if err != nil {
		return err
	}
	if err := tx.Bucket(ixPosts).Delete(x[:]); err != nil {
		return err
	}
	_ = tx.Bucket(ixAt).Delete(atKey(r.Height, x))
	if o := r.owner(); !o.IsZero() {
		_ = tx.Bucket(ixByAuthor).Delete(authorKey(o, r.Height, x))
	}
	if r.TextLen > 0 {
		_ = tx.Bucket(ixText).Delete(append(r.TextHash[:], x[:]...))
	}
	if !r.Parent.IsZero() {
		_ = tx.Bucket(ixComments).Delete(commentKey(r.Parent, r.Height, x))
		return addStat(tx, r.Parent, 0, -1, 0)
	}
	_ = tx.Bucket(ixBoard).Delete(boardKey(r.Board, r.Height, x))
	return addCount(tx, r.Board, -1)
}

func (ix *Index) get(tx *bolt.Tx, id cryptox.Hash) (*postRec, bool) {
	v := tx.Bucket(ixPosts).Get(id[:])
	if v == nil {
		return nil, false
	}
	r, err := decodeRec(id, v)
	return r, err == nil
}

// postsWithText lists the posts committing to a body.
func (ix *Index) postsWithText(x cryptox.Hash) []cryptox.Hash {
	var out []cryptox.Hash
	_ = ix.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(ixText).Cursor()
		for k, _ := c.Seek(x[:]); k != nil && bytes.HasPrefix(k, x[:]); k, _ = c.Next() {
			var id cryptox.Hash
			copy(id[:], k[32:])
			out = append(out, id)
		}
		return nil
	})
	return out
}

// dataHeight is when a data root was first placed (upload or seal).
func (ix *Index) dataHeight(root cryptox.Hash) (uint64, bool) {
	var h uint64
	var ok bool
	_ = ix.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(ixData).Get(root[:]); v != nil {
			h, ok = binary.BigEndian.Uint64(v), true
		}
		return nil
	})
	return h, ok
}

// takeNeedText returns up to n posts whose bodies may be missing.
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

// post returns an indexed post.
func (ix *Index) post(id cryptox.Hash) (PostView, bool) {
	var v PostView
	var ok bool
	_ = ix.db.View(func(tx *bolt.Tx) error {
		var r *postRec
		if r, ok = ix.get(tx, id); ok {
			v = r.view()
			v.setStats(stats(tx, id))
		}
		return nil
	})
	return v, ok
}

// Liked reports whether an account's like of a post is counted.
func (ix *Index) Liked(id cryptox.Hash, a cryptox.Address) bool { return ix.LikedBy(id, a[:]) }

// LikedBy reports whether a like of a post by an account or an anonymous
// tag is counted.
func (ix *Index) LikedBy(id cryptox.Hash, who []byte) bool {
	ok := false
	_ = ix.db.View(func(tx *bolt.Tx) error {
		ok = tx.Bucket(ixLiked).Get(likedKey(id, who)) != nil
		return nil
	})
	return ok
}

// Boards lists boards with their post counts.
func (ix *Index) Boards() map[string]int {
	out := map[string]int{}
	_ = ix.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(ixCount).ForEach(func(k, v []byte) error {
			out[string(k)] = int(binary.BigEndian.Uint64(v))
			return nil
		})
	})
	return out
}

// Board returns the newest posts of a board, newest first, with their like
// and comment counts; minLikes filters "개념글". It reads O(limit) entries
// (more when minLikes skips some).
func (ix *Index) Board(board string, minLikes uint64, limit int) []PostView {
	var out []PostView
	prefix := append([]byte{byte(len(board))}, board...)
	_ = ix.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(ixBoard).Cursor()
		k, _ := c.Seek(append(append([]byte(nil), prefix...), bytes.Repeat([]byte{0xff}, 41)...))
		if k == nil {
			k, _ = c.Last()
		} else {
			k, _ = c.Prev()
		}
		for ; k != nil && bytes.HasPrefix(k, prefix) && len(out) < limit; k, _ = c.Prev() {
			var id cryptox.Hash
			copy(id[:], k[len(prefix)+8:])
			r, ok := ix.get(tx, id)
			if !ok {
				continue
			}
			v := r.view()
			v.setStats(stats(tx, id))
			if v.Likes >= minLikes {
				out = append(out, v)
			}
		}
		return nil
	})
	return out
}

// Thread returns a post and its comments, oldest first.
func (ix *Index) Thread(id cryptox.Hash) (*PostView, []PostView) {
	var p *PostView
	var cs []PostView
	_ = ix.db.View(func(tx *bolt.Tx) error {
		r, ok := ix.get(tx, id)
		if !ok {
			return nil
		}
		v := r.view()
		v.setStats(stats(tx, id))
		p = &v
		c := tx.Bucket(ixComments).Cursor()
		for k, _ := c.Seek(id[:]); k != nil && bytes.HasPrefix(k, id[:]); k, _ = c.Next() {
			var cid cryptox.Hash
			copy(cid[:], k[40:])
			if cr, ok := ix.get(tx, cid); ok {
				cv := cr.view()
				cv.setStats(stats(tx, cid))
				cs = append(cs, cv)
			}
		}
		return nil
	})
	return p, cs
}

// PendingPosts lists posts not mined yet: ours still queued, travelling the
// stem or waiting to be proven again, and everyone's in the pool.
func (n *Node) PendingPosts() []PostView { return n.pendingPosts(true) }

// pendingPosts lists posts not mined yet; pool adds everyone's in the pool.
func (n *Node) pendingPosts(pool bool) []PostView {
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
	if pool {
		for _, t := range n.Pool.Pending() {
			add(t)
		}
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
	for _, a := range n.ownAnon(false) {
		v := PostView{Board: a.Board, Text: a.Text, Lane: "anon", Pseudo: a.Pseudo, Pending: true}
		if !a.Parent.IsZero() {
			v.Parent = a.Parent.String()
		}
		out = append(out, v)
	}
	return out
}
