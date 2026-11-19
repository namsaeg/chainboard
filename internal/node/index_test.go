package node

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/types"
)

// fakeChain is a best chain of unvalidated blocks: the index only reads.
type fakeChain []*types.Block

func (f fakeChain) Height() uint64 { return uint64(len(f)) }
func (f fakeChain) BlockAt(h uint64) *types.Block {
	if h == 0 || h > uint64(len(f)) {
		return nil
	}
	return f[h-1]
}

func fakeBlock(h uint64, salt byte, txs ...*types.Tx) *types.Block {
	b := &types.Block{Txs: txs}
	b.Height, b.Time = h, int64(h)
	b.Prev = cryptox.Hash{salt, byte(h)} // distinct hashes per branch
	return b
}

func fakePost(board, text string, parent types.Hash) *types.Tx {
	t := &types.Tx{Kind: types.TxPost, Board: board, Parent: parent, Tier: 1, From: make([]byte, 32)}
	t.SetText([]byte(text))
	return t
}

func openTestIndex(t *testing.T) *Index {
	ix, err := OpenIndex(filepath.Join(t.TempDir(), "index.db"), cryptox.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func snapshot(ix *Index) (map[string]int, []string, []string) {
	ids := func(vs []PostView) (out []string) {
		for _, v := range vs {
			out = append(out, v.Board+":"+v.ID)
		}
		return
	}
	return ix.Boards(), ids(ix.Board("a", 0, 100)), ids(ix.Board("b", 0, 100))
}

// TestIndexReorg: a reorganisation undoes only the replaced blocks, and the
// result equals an index built directly on the new chain.
func TestIndexReorg(t *testing.T) {
	p1 := fakePost("a", "첫 글", types.Hash{})
	c1 := fakePost("a", "댓글", p1.ID())
	up := &types.Tx{Kind: types.TxUpload, DataRoot: cryptox.Hash{9}, Size: 1}
	common := fakeChain{fakeBlock(1, 0, p1), fakeBlock(2, 0, c1, up)}
	lost := append(append(fakeChain(nil), common...),
		fakeBlock(3, 1, fakePost("a", "사라질 글", types.Hash{})),
		fakeBlock(4, 1, fakePost("b", "사라질 글 2", types.Hash{}), &types.Tx{Kind: types.TxUpload, DataRoot: cryptox.Hash{8}}))
	won := append(append(fakeChain(nil), common...),
		fakeBlock(3, 2, fakePost("b", "남을 글", types.Hash{})),
		fakeBlock(4, 2),
		fakeBlock(5, 2, fakePost("a", "새 글", types.Hash{})))

	ix := openTestIndex(t)
	if err := ix.Sync(lost); err != nil {
		t.Fatal(err)
	}
	if got := ix.Boards(); got["a"] != 2 || got["b"] != 1 {
		t.Fatalf("boards %v", got)
	}
	if vs := ix.Board("a", 0, 1); len(vs) != 1 || vs[0].Height != 3 {
		t.Fatalf("newest first, limited: %+v", vs)
	}
	if p, cs := ix.Thread(p1.ID()); p == nil || len(cs) != 1 || cs[0].ID != c1.ID().String() {
		t.Fatal("thread")
	}
	if ids := ix.postsWithText(p1.TextHash); len(ids) != 1 || ids[0] != p1.ID() {
		t.Fatal("text lookup")
	}
	if h, ok := ix.dataHeight(cryptox.Hash{8}); !ok || h != 4 {
		t.Fatal("data height")
	}

	if err := ix.Sync(won); err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.dataHeight(cryptox.Hash{8}); ok {
		t.Fatal("data of a replaced block kept")
	}
	if h, ok := ix.dataHeight(cryptox.Hash{9}); !ok || h != 2 {
		t.Fatal("data of a common block lost")
	}
	direct := openTestIndex(t)
	if err := direct.Sync(won); err != nil {
		t.Fatal(err)
	}
	b1, a1, bb1 := snapshot(ix)
	b2, a2, bb2 := snapshot(direct)
	if !reflect.DeepEqual(b1, b2) || !reflect.DeepEqual(a1, a2) || !reflect.DeepEqual(bb1, bb2) {
		t.Fatalf("after reorg %v %v %v, direct %v %v %v", b1, a1, bb1, b2, a2, bb2)
	}
}

func fakeLikes(from byte, refs ...types.LikeRef) *types.Tx {
	pub := make([]byte, 32)
	pub[0] = from
	return &types.Tx{Kind: types.TxLikes, From: pub, Likes: refs}
}

// Likes are resolved from short references, counted once per account and
// post within the window, and undone with the blocks that carried them.
func TestIndexLikes(t *testing.T) {
	p1 := fakePost("a", "좋아요 받을 글", types.Hash{})
	c1 := fakePost("a", "댓글", p1.ID())
	ref := types.RefOf(p1.ID(), 1)
	wrong := ref
	wrong.Prefix[0]++
	common := fakeChain{
		fakeBlock(1, 0, p1),
		fakeBlock(2, 0, c1, fakeLikes(1, ref, wrong)),
	}
	lost := append(append(fakeChain(nil), common...),
		fakeBlock(3, 1, fakeLikes(2, ref)),
		fakeBlock(4, 1, fakeLikes(1, ref))) // the same account again
	won := append(append(fakeChain(nil), common...),
		fakeBlock(3, 2), fakeBlock(4, 2), fakeBlock(5, 2), fakeBlock(6, 2),
		fakeBlock(7, 2, fakeLikes(3, ref))) // past the window
	ix := openTestIndex(t)
	ix.LikeWindow = 5
	if err := ix.Sync(lost); err != nil {
		t.Fatal(err)
	}
	v, _ := ix.post(p1.ID())
	if v.Likes != 2 || v.Comments != 1 {
		t.Fatalf("likes %d comments %d, want 2 and 1", v.Likes, v.Comments)
	}
	if err := ix.Sync(won); err != nil {
		t.Fatal(err)
	}
	v, _ = ix.post(p1.ID())
	var b, c cryptox.Address
	b = cryptox.AddressOf(fakeLikes(2).From)
	c = cryptox.AddressOf(fakeLikes(3).From)
	if v.Likes != 1 || ix.Liked(p1.ID(), b) || ix.Liked(p1.ID(), c) {
		t.Fatalf("after reorg: likes %d", v.Likes)
	}
	if vs := ix.Board("a", 1, 10); len(vs) != 1 {
		t.Fatal("best-of filter")
	}
}
