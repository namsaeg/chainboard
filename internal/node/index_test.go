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
	ix, err := OpenIndex(filepath.Join(t.TempDir(), "index.db"))
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
	return ix.Boards(), ids(ix.Board(nil, "a", 0, 100)), ids(ix.Board(nil, "b", 0, 100))
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
	if vs := ix.Board(nil, "a", 0, 1); len(vs) != 1 || vs[0].Height != 3 {
		t.Fatalf("newest first, limited: %+v", vs)
	}
	if p, cs := ix.Thread(nil, p1.ID()); p == nil || len(cs) != 1 || cs[0].ID != c1.ID().String() {
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
