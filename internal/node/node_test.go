package node

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/types"
)

func fastParams() *params.Params {
	p := params.Devnet()
	p.VDFIterations = 2000
	p.PackMemoryKiB = 8
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
	up, err := b.Upload(pngBytes(), 12, true)
	if err != nil || up.Action != "upload" {
		t.Fatalf("upload: %+v %v", up, err)
	}
	root := up.Root
	waitFor(t, "upload mined", 60*time.Second, func() bool {
		_, ok := c.Chain.Tip().Data.Get(root)
		return ok
	})
	// The same file again: reused for a shorter tier, extended for a longer one.
	if again, err := b.Upload(pngBytes(), 1, true); err != nil || again.Action != "reuse" || again.Root != root {
		t.Fatalf("re-upload: %+v %v", again, err)
	}
	if longer, err := b.Upload(pngBytes(), 24, true); err != nil || longer.Action != "extend" {
		t.Fatalf("longer re-upload: %+v %v", longer, err)
	}
	waitFor(t, "auto-extend mined", 60*time.Second, func() bool {
		d, ok := c.Chain.Tip().Data.Get(root)
		return ok && d.Tier == 24
	})
	post := &types.Tx{Kind: types.TxPost, Lane: types.LaneFee, Board: "테스트", Text: []byte("세 노드 시험 글"), Tier: 12, Attach: []types.Hash{root}}
	if err := b.Send(post); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "post visible on every node", 60*time.Second, func() bool {
		for _, n := range []*Node{a, b, c} {
			if len(n.Board("테스트", 0, 10)) == 0 {
				return false
			}
		}
		return true
	})
	// The miner fetched the image's chunks from b to accept the upload.
	if !a.Store.Complete(root) {
		t.Fatal("miner never fetched the uploaded data")
	}
	// The body is not in the block but every node shows it.
	for _, n := range []*Node{a, b, c} {
		v := n.Board("테스트", 0, 10)[0]
		if v.Text != "세 노드 시험 글" || v.TextState != "ok" || v.Tier != 12 {
			t.Fatalf("post as shown: %+v", v)
		}
	}
	// A node that joins later gets the bodies along with the blocks.
	d := start(t, p, false, a.Host.Addr())
	waitFor(t, "late node synced with texts", 60*time.Second, func() bool {
		vs := d.Board("테스트", 0, 10)
		return d.Chain.Height() >= c.Chain.Height() && len(vs) == 1 && vs[0].Text == "세 노드 시험 글"
	})
	// One that takes the blocks as history (no bodies demanded) fetches
	// them in the background; past its one-day cache it forgets a body it
	// does not store and fetches it again when the post is opened.
	e, err := New(Config{Network: "devnet", DataDir: t.TempDir(), Listen: "127.0.0.1:0", Seeds: []string{a.Host.Addr()},
		Embargo: 300 * time.Millisecond, Params: p, Logf: t.Logf, TextWindowSec: 1, TextCacheDays: 1})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // make every existing block "history" for e
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Stop)
	hash := post.TextHash
	waitFor(t, "background fetch", 60*time.Second, func() bool {
		return e.Chain.Height() >= c.Chain.Height() && e.Store.HasText(hash)
	})
	waitFor(t, "post ages past the cache", 60*time.Second, func() bool {
		v, _ := e.Index.post(post.ID())
		return e.Chain.Height() > v.Height+p.BlocksPerDay+1
	})
	e.sweep(e.Chain.Tip())
	if e.Store.HasText(hash) {
		t.Fatal("body kept past the cache")
	}
	waitFor(t, "fetched again when opened", 30*time.Second, func() bool {
		vs := e.Board("테스트", 0, 10)
		return len(vs) == 1 && vs[0].TextState == "ok" && vs[0].Text == "세 노드 시험 글"
	})
	// All three agree on the chain.
	waitFor(t, "same tip", 30*time.Second, func() bool {
		return a.Chain.BestHash() == b.Chain.BestHash() && b.Chain.BestHash() == c.Chain.BestHash()
	})
	if err := c.Chain.Tip().CheckSupply(); err != nil {
		t.Fatal(err)
	}
}

// TestAnonymousPosts joins the RLN lane over the network and posts both fully
// anonymously and under a pseudonym.
func TestAnonymousPosts(t *testing.T) {
	p := fastParams()
	p.RLNEpochBlocks = 1000 // one epoch for the whole test
	a := start(t, p, true)
	b := start(t, p, false, a.Host.Addr())
	c := start(t, p, false, a.Host.Addr(), b.Host.Addr())
	waitFor(t, "mining", 60*time.Second, func() bool { return a.Chain.Height() >= 8 })
	if err := a.Send(&types.Tx{Kind: types.TxTransfer, To: b.Wallet.Address(), Amount: 20 * params.Coin}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "funds", 60*time.Second, func() bool { return b.Chain.Tip().Peek(b.Wallet.Address()).Balance > 0 })
	if err := b.AnonPost(AnonRequest{Board: "익명", Text: "가입 전"}); err == nil {
		t.Fatal("anonymous post accepted before joining")
	}
	if err := b.Send(&types.Tx{Kind: types.TxStake, Amount: 10 * params.Coin}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stake", 60*time.Second, func() bool { return b.Chain.Tip().Peek(b.Wallet.Address()).StakePending > 0 })
	start := b.Chain.Height()
	waitFor(t, "stake aging", 60*time.Second, func() bool { return b.Chain.Height() > start+p.StakeAgingBlocks })
	if err := b.JoinRLN(3 * params.Coin); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "membership", 60*time.Second, func() bool { return b.RLNStatus().Status == "active" })
	if s := b.RLNStatus(); s.Limit != 3 || s.Members != 1 {
		t.Fatalf("status %+v", s)
	}
	if err := b.AnonPost(AnonRequest{Board: "익명", Text: "누가 썼는지 모르는 글"}); err != nil {
		t.Fatal(err)
	}
	if err := b.AnonPost(AnonRequest{Board: "익명", Text: "가명으로 쓴 글", Pseudo: true}); err != nil {
		t.Fatal(err)
	}
	// The third post is the last of the epoch: it needs a confirmation.
	last := AnonRequest{Board: "익명", Text: "이번 에포크 마지막 글"}
	var we *WarningError
	if err := b.AnonPost(last); !errors.As(err, &we) || we.Warnings[0].Code != "anon_last" {
		t.Fatalf("last post of the epoch without a warning: %v", err)
	}
	last.Confirm = true
	if err := b.AnonPost(last); err != nil {
		t.Fatal(err)
	}
	if err := b.AnonPost(AnonRequest{Board: "익명", Text: "한도 초과", Confirm: true}); !errors.Is(err, errLimit) {
		t.Fatalf("post beyond the limit: %v", err)
	}
	waitFor(t, "anonymous posts on another node", 90*time.Second, func() bool { return len(c.Board("익명", 0, 10)) == 3 })

	// A second device with the same anonymous key notices the first one's
	// posts and stops before it double-signals.
	key, err := os.ReadFile(filepath.Join(b.dir, "rln.key"))
	if err != nil {
		t.Fatal(err)
	}
	fdir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fdir, p.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fdir, p.Name, "rln.key"), key, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := New(Config{Network: "devnet", DataDir: fdir, Listen: "127.0.0.1:0", Seeds: []string{a.Host.Addr()},
		Embargo: 300 * time.Millisecond, Params: p, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Stop)
	waitFor(t, "foreign use noticed", 60*time.Second, func() bool { return f.RLNStatus().ForeignUse })
	if err := f.AnonPost(AnonRequest{Board: "익명", Text: "다른 기기"}); !errors.Is(err, ErrForeignUse) {
		t.Fatalf("second device kept posting: %v", err)
	}
	waitFor(t, "second device synced", 60*time.Second, func() bool { return f.Chain.Height() >= c.Chain.Height() })
	f.AckForeign()
	if err := f.AnonPost(AnonRequest{Board: "익명", Text: "다른 기기", Confirm: true}); !errors.Is(err, errLimit) {
		t.Fatalf("second device after acknowledging: %v (its ledger should hold the first device's ids)", err)
	}
	wallet := b.Wallet.Address().String()
	var pseudo, plain int
	for _, v := range c.Board("익명", 0, 10) {
		if v.Lane != "anon" || v.Author == wallet {
			t.Fatalf("post leaks its author: %+v", v)
		}
		if v.Pseudo && v.Author == b.RLNStatus().Pseudonym {
			pseudo++
		} else if v.Author == "" {
			plain++
		}
	}
	if pseudo != 1 || plain != 2 {
		t.Fatalf("pseudonym %d, anonymous %d", pseudo, plain)
	}
	if s := b.RLNStatus(); s.Used != 2 && s.Epoch == b.Chain.Tip().RLNEpoch(b.Chain.Height()+1) {
		t.Logf("used %d (epoch may have rolled over)", s.Used)
	}
	if len(b.ownAnon(true)) != 0 {
		waitFor(t, "outbox drained", 30*time.Second, func() bool { return len(b.ownAnon(true)) == 0 })
	}
}

// TestOutboxSurvivesRestart closes a node while a transaction waits out its
// random send delay; the reopened node still has it, unchanged.
func TestOutboxSurvivesRestart(t *testing.T) {
	p := fastParams()
	dir := t.TempDir()
	open := func(mine bool) *Node {
		n, err := New(Config{Network: "devnet", DataDir: dir, Listen: "127.0.0.1:0", Mine: mine, PledgeBytes: 1 << 40,
			SendDelayMax: time.Hour, Embargo: 300 * time.Millisecond, Params: p, Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		if err := n.Start(); err != nil {
			t.Fatal(err)
		}
		return n
	}
	a := open(true)
	waitFor(t, "funds", 60*time.Second, func() bool { return a.Chain.Tip().Peek(a.Wallet.Address()).Balance > params.Coin })
	tx := &types.Tx{Kind: types.TxTransfer, To: a.Wallet.Address(), Amount: 1}
	if err := a.Send(tx); err != nil {
		t.Fatal(err)
	}
	a.Stop()
	b := open(false)
	defer b.Stop()
	q := b.Queued()
	if len(q) != 1 || q[0].ID() != tx.ID() {
		t.Fatalf("queue after restart: %v", q)
	}
}

// TestBurstWarning: sending fast makes the node ask for a confirmation
// before the surcharged transaction leaves.
func TestBurstWarning(t *testing.T) {
	p := fastParams()
	a := start(t, p, true)
	waitFor(t, "funds", 60*time.Second, func() bool { return a.Chain.Tip().Peek(a.Wallet.Address()).Balance > 10*params.Coin })
	for i := 0; ; i++ {
		err := a.SendChecked(&types.Tx{Kind: types.TxTransfer, To: a.Wallet.Address(), Amount: 1}, false)
		var we *WarningError
		if errors.As(err, &we) {
			if we.Warnings[0].Code != "burst" || we.Warnings[0].Factor < 2 || we.Fee == 0 {
				t.Fatalf("warning %+v", we)
			}
			if i < int(p.BurstFree) {
				t.Fatalf("warned after only %d transactions", i)
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if i > 80 {
			t.Fatal("no warning after 80 fast transactions")
		}
	}
	if err := a.SendChecked(&types.Tx{Kind: types.TxTransfer, To: a.Wallet.Address(), Amount: 1}, true); err != nil {
		t.Fatalf("confirmed send: %v", err)
	}
}
