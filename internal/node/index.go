package node

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
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
	ixStats    = []byte("stats")    // post id -> likes (u64) comments (u64)
	ixMeta     = []byte("meta")     // "chain" -> format and genesis
)

// indexFormat changes whenever what the index stores changes; an index of
// another format or chain is rebuilt.
const indexFormat = "4"

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
		for _, b := range [][]byte{ixPath, ixPosts, ixBoard, ixCount, ixComments, ixText, ixData, ixUndo, ixAt, ixLiked, ixStats, ixMeta} {
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
	Fund     bool   // a funding request; Note is its text
	Note     string // funding request
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
	r.Note = string(rd.Bytes(types.MaxFundNote))
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
		TextHash: t.TextHash, TextLen: t.TextLen, Tier: t.Tier}
	return r
}

func (r *postRec) view() PostView {
	v := PostView{ID: r.ID.String(), Board: r.Board, Author: r.Author.String(), Height: r.Height, Time: r.Time,
		Lane: "fee", Tier: r.Tier, TextState: "ok", id: r.ID, textHash: r.TextHash, textLen: r.TextLen}
	if r.Fund {
		v.Text, v.Lane = r.Note, "fund"
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
				added, err := addLike(tx, p.Request, from)
				if err != nil {
					return nil, 0, err
				}
				if added {
					undo.U8(undoLike)
					undo.Fixed(p.Request[:])
					undo.Fixed(from[:])
				}
			}
			for _, ref := range t.Likes {
				id, ok := resolveRef(tx, ref)
				if !ok || (likeWindow > 0 && h > ref.Height+likeWindow) {
					continue
				}
				added, err := addLike(tx, id, from)
				if err != nil {
					return nil, 0, err
				}
				if added {
					undo.U8(undoLike)
					undo.Fixed(id[:])
					undo.Fixed(from[:])
				}
			}
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

func likedKey(id cryptox.Hash, a cryptox.Address) []byte {
	return append(append(make([]byte, 0, 52), id[:]...), a[:]...)
}

// addLike counts one account's like of a post once.
func addLike(tx *bolt.Tx, id cryptox.Hash, from cryptox.Address) (bool, error) {
	lb := tx.Bucket(ixLiked)
	k := likedKey(id, from)
	if lb.Get(k) != nil {
		return false, nil
	}
	if err := lb.Put(k, []byte{1}); err != nil { // a nil value reads back as absent
		return false, err
	}
	return true, addStat(tx, id, 1, 0)
}

// addStat moves a post's like and comment counts.
func addStat(tx *bolt.Tx, id cryptox.Hash, likes, comments int64) error {
	sb := tx.Bucket(ixStats)
	var l, c uint64
	if v := sb.Get(id[:]); len(v) == 16 {
		l, c = binary.BigEndian.Uint64(v), binary.BigEndian.Uint64(v[8:])
	}
	l, c = uint64(int64(l)+likes), uint64(int64(c)+comments)
	if l == 0 && c == 0 {
		return sb.Delete(id[:])
	}
	return sb.Put(id[:], binary.BigEndian.AppendUint64(u64(l), c))
}

func stats(tx *bolt.Tx, id cryptox.Hash) (likes, comments uint64) {
	if v := tx.Bucket(ixStats).Get(id[:]); len(v) == 16 {
		return binary.BigEndian.Uint64(v), binary.BigEndian.Uint64(v[8:])
	}
	return 0, 0
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
	if !r.Parent.IsZero() {
		if err := addStat(tx, r.Parent, 0, 1); err != nil {
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
		from cryptox.Address
	}
	var changes []change
	raw := tx.Bucket(ixUndo).Get(u64(h))
	rd := codec.NewReader(raw)
	for rd.Err == nil && len(raw) > 0 && !rd.AtEnd() {
		c := change{kind: rd.U8()}
		rd.Fixed(c.id[:])
		if c.kind == undoLike {
			rd.Fixed(c.from[:])
		}
		changes = append(changes, c)
	}
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		var err error
		switch c.kind {
		case undoData:
			err = tx.Bucket(ixData).Delete(c.id[:])
		case undoLike:
			if err = tx.Bucket(ixLiked).Delete(likedKey(c.id, c.from)); err == nil {
				err = addStat(tx, c.id, -1, 0)
			}
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
	if r.TextLen > 0 {
		_ = tx.Bucket(ixText).Delete(append(r.TextHash[:], x[:]...))
	}
	if !r.Parent.IsZero() {
		_ = tx.Bucket(ixComments).Delete(commentKey(r.Parent, r.Height, x))
		return addStat(tx, r.Parent, 0, -1)
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
			v.Likes, v.Comments = stats(tx, id)
		}
		return nil
	})
	return v, ok
}

// Liked reports whether an account's like of a post is counted.
func (ix *Index) Liked(id cryptox.Hash, a cryptox.Address) bool {
	ok := false
	_ = ix.db.View(func(tx *bolt.Tx) error {
		ok = tx.Bucket(ixLiked).Get(likedKey(id, a)) != nil
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
			v.Likes, v.Comments = stats(tx, id)
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
		v.Likes, v.Comments = stats(tx, id)
		p = &v
		c := tx.Bucket(ixComments).Cursor()
		for k, _ := c.Seek(id[:]); k != nil && bytes.HasPrefix(k, id[:]); k, _ = c.Next() {
			var cid cryptox.Hash
			copy(cid[:], k[40:])
			if cr, ok := ix.get(tx, cid); ok {
				cv := cr.view()
				cv.Likes, cv.Comments = stats(tx, cid)
				cs = append(cs, cv)
			}
		}
		return nil
	})
	return p, cs
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
	for _, a := range n.ownAnon(false) {
		v := PostView{Board: a.Board, Text: a.Text, Lane: "anon", Pseudo: a.Pseudo, Pending: true}
		if !a.Parent.IsZero() {
			v.Parent = a.Parent.String()
		}
		out = append(out, v)
	}
	return out
}
