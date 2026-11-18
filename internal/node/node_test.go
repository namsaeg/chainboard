package node

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math/big"
	"testing"
	"time"

	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/types"
)

func fastParams() *params.Params {
	p := params.Devnet()
	p.VDFIterations = 2000
	p.BalloonSpace = 16
	p.GenesisTarget = new(big.Int).Lsh(big.NewInt(1), 250)
	p.BlocksPerDay, p.BlocksPerMonth, p.BlocksPerYear = 10, 20, 240
	p.HalvingBlocks = 1_000_000
	return p
}

func start(t *testing.T, p *params.Params, mine bool, seeds ...string) *Node {
	t.Helper()
	n, err := New(Config{Network: "devnet", DataDir: t.TempDir(), Listen: "127.0.0.1:0", Seeds: seeds,
		Mine: mine, PledgeBytes: 1 << 40, Embargo: 300 * time.Millisecond, Params: p, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Stop)
	return n
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func pngBytes() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 40, 30))
	for x := 0; x < 40; x++ {
		img.Set(x, x%30, color.RGBA{200, 30, 30, 255})
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

// TestThreeNodes runs a miner and two users over real TLS connections.
func TestThreeNodes(t *testing.T) {
	p := fastParams()
	a := start(t, p, true)
	b := start(t, p, false, a.Host.Addr())
	c := start(t, p, false, a.Host.Addr(), b.Host.Addr())

	waitFor(t, "mining and sync", 60*time.Second, func() bool {
		return a.Chain.Height() >= 12 && b.Chain.Height() >= 12 && c.Chain.Height() >= 12
	})

	// The miner funds b, which then uploads an image and posts it.
	if err := a.Send(&types.Tx{Kind: types.TxTransfer, To: b.Wallet.Address(), Amount: 20 * params.Coin}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "transfer", 60*time.Second, func() bool {
		return b.Chain.Tip().Peek(b.Wallet.Address()).Balance > 0
	})
	root, err := b.Upload(pngBytes(), 12)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "upload mined", 60*time.Second, func() bool {
		_, ok := c.Chain.Tip().Data[root]
		return ok
	})
	post := &types.Tx{Kind: types.TxPost, Lane: types.LaneFee, Board: "테스트", Text: []byte("세 노드 시험 글"), Attach: []types.Hash{root}}
	if err := b.Send(post); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "post visible on every node", 60*time.Second, func() bool {
		for _, n := range []*Node{a, b, c} {
			if len(n.Index.Board("테스트", 0, 10)) == 0 {
				return false
			}
		}
		return true
	})
	// The miner fetched the image's chunks from b to accept the upload.
	if !a.Store.Complete(root) {
		t.Fatal("miner never fetched the uploaded data")
	}
	// All three agree on the chain.
	waitFor(t, "same tip", 30*time.Second, func() bool {
		return a.Chain.BestHash() == b.Chain.BestHash() && b.Chain.BestHash() == c.Chain.BestHash()
	})
	if err := c.Chain.Tip().CheckSupply(); err != nil {
		t.Fatal(err)
	}
}
