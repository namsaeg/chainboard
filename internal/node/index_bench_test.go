package node

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/types"
)

// TestIndexSyncCost reports what one new block costs the index once many
// posts are indexed. It is a measurement, not a check:
// CHAINBOARD_MEASURE=1 go test -run IndexSyncCost -v ./internal/node
func TestIndexSyncCost(t *testing.T) {
	if os.Getenv("CHAINBOARD_MEASURE") == "" {
		t.Skip("set CHAINBOARD_MEASURE=1 to measure")
	}
	const blocks, perBlock = 2000, 100
	var c fakeChain
	for h := uint64(1); h <= blocks+1; h++ {
		var txs []*types.Tx
		for i := 0; i < perBlock; i++ {
			txs = append(txs, fakePost(fmt.Sprintf("b%d", i%20), fmt.Sprintf("글 %d-%d", h, i), types.Hash{}))
		}
		c = append(c, fakeBlock(h, 0, txs...))
	}
	ix := openTestIndex(t)
	start := time.Now()
	if err := ix.Sync(c[:blocks]); err != nil {
		t.Fatal(err)
	}
	t.Logf("initial sync of %d posts: %v", blocks*perBlock, time.Since(start))
	start = time.Now()
	if err := ix.Sync(c); err != nil {
		t.Fatal(err)
	}
	t.Logf("one more block of %d posts with %d indexed: %v", perBlock, blocks*perBlock, time.Since(start))
	start = time.Now()
	vs := ix.Board(nil, "b3", 0, 50)
	t.Logf("newest 50 of a %d-post board: %v (%d)", blocks*perBlock/20, time.Since(start), len(vs))
	var h types.Header
	h.VDFOutputs = make([]cryptox.Hash, 200)
	start = time.Now()
	for i := 0; i < 10000; i++ {
		h.Hash()
	}
	t.Logf("old index: hashing one header with 200 VDF outputs: %v", time.Since(start)/10000)
}
