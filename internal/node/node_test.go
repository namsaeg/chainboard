package node

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/namsaeg/chainboard/internal/anonnet"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/keystore"
	"github.com/namsaeg/chainboard/internal/nat"
	"github.com/namsaeg/chainboard/internal/p2p"
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
		// The attachment shows its own retention, kept apart from the text's.
		if len(v.Kept) != 1 || v.Kept[0].Root != root.String() || v.Kept[0].Tier != 24 || v.Kept[0].Until <= v.Until || v.Kept[0].Gone {
			t.Fatalf("attachment retention: %+v (text until %d)", v.Kept, v.Until)
		}
	}
	// A post may attach a file uploaded a moment before, still unmined:
	// the fee quote counts the node's own queued transactions.
	fresh, err := b.Upload(noisePNG(30, 20), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Send(&types.Tx{Kind: types.TxPost, Lane: types.LaneFee, Board: "테스트2", Text: []byte("바로 붙인 첨부"), Attach: []types.Hash{fresh.Root}}); err != nil {
		t.Fatalf("post right after its upload: %v", err)
	}
	if act := b.MyActivity(10); len(act.Posts) != 1 || act.Posts[0].ID != post.ID().String() || act.Posts[0].Text != "세 노드 시험 글" || len(act.Posts[0].Kept) != 1 {
		t.Fatalf("writer's activity: %+v", act.Posts)
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
	// A fetch answer still in flight may store the body once more right
	// after a sweep; the next sweep drops it again.
	waitFor(t, "body dropped past the cache", 20*time.Second, func() bool {
		e.sweep(e.Chain.Tip())
		return !e.Store.HasText(hash)
	})
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
	// The bond follows the storage price: the quote leaves room for the
	// price to move before the join is mined, and counts both ways.
	st := b.trialState() // one state: the price moves from block to block
	q := bondQuote(st, 0)
	if q.Limit != 0 || q.NextBond != q.BondFor[0] || len(q.BondFor) != int(p.RLNMaxLimit) {
		t.Fatalf("quote for nothing: %+v", q)
	}
	for i := 1; i < len(q.BondFor); i++ {
		if q.BondFor[i] <= q.BondFor[i-1] {
			t.Fatalf("bonds not rising: %v", q.BondFor)
		}
	}
	for i, bond := range q.BondFor { // each quoted bond is the least that buys its count
		if got, under := bondQuote(st, bond).Limit, bondQuote(st, bond-1).Limit; got != uint64(i+1) || under != uint64(i) {
			t.Fatalf("bond %d buys %d, one unit less %d; want %d", bond, got, under, i+1)
		}
	}
	if q4 := bondQuote(st, q.BondFor[3]); q4.Limit != 4 || q4.NextBond != q.BondFor[4] {
		t.Fatalf("quote for four: %+v", q4)
	}
	if q3 := bondQuote(st, q.BondFor[3]-1); q3.Limit != 3 {
		t.Fatalf("quote just under four: %+v", q3)
	}
	if err := b.JoinRLN(q.BondFor[3]); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "membership", 60*time.Second, func() bool { return b.RLNStatus().Status == "active" })
	if s := b.RLNStatus(); s.Limit != 4 || s.Members != 1 {
		t.Fatalf("status %+v", s)
	}
	if err := b.AnonPost(AnonRequest{Board: "익명", Text: "누가 썼는지 모르는 글"}); err != nil {
		t.Fatal(err)
	}
	if err := b.AnonPost(AnonRequest{Board: "익명", Text: "가명으로 쓴 글", Pseudo: true}); err != nil {
		t.Fatal(err)
	}
	if err := b.AnonPost(AnonRequest{Board: "익명", Text: "세 번째 익명 글"}); err != nil {
		t.Fatal(err)
	}
	// The fourth message is the last of the epoch: it needs a confirmation.
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
	waitFor(t, "anonymous posts on another node", 90*time.Second, func() bool { return len(c.Board("익명", 0, 10)) == 4 })

	// Anonymous posts take likes too: at a one-time address of their own,
	// which only the writer's node can spend from.
	var plain PostView
	for _, v := range a.Board("익명", 0, 10) {
		if !v.Pseudo && v.TipTo != "" {
			plain = v
		}
	}
	if plain.TipTo == "" || plain.TipTo == b.Wallet.Address().String() {
		t.Fatalf("anonymous post without its own tip address: %+v", plain)
	}
	tip := 2 * a.MinLikeAmount()
	pid, _ := cryptox.ParseHash(plain.ID)
	// The writer's own node knows the post as its own and will not like it
	// (that would be a transfer between its own addresses by another way).
	for _, v := range b.Board("익명", 0, 10) {
		if !v.Mine {
			t.Fatalf("own anonymous post not marked: %+v", v)
		}
	}
	if _, err := b.QueueLike(LikeRequest{ID: pid, Amount: tip}); err == nil {
		t.Fatal("liked its own anonymous post")
	}
	for _, v := range a.Board("익명", 0, 10) {
		if v.Mine {
			t.Fatal("someone else's post marked as one's own")
		}
	}
	if _, err := a.QueueLike(LikeRequest{ID: pid, Amount: tip, Now: true, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "tip to an anonymous post", 60*time.Second, func() bool {
		accts, _ := b.TipAccounts()
		for _, x := range accts {
			if x.Address == plain.TipTo && x.Balance == tip {
				return true
			}
		}
		return false
	})
	waitFor(t, "anonymous post's like counted", 30*time.Second, func() bool { v, _ := c.Index.post(pid); return v.Likes == 1 && v.Tips == tip })
	// The writer's node lists its anonymous posts (by their tip addresses)
	// among its own; the liker's lists the post as liked.
	waitFor(t, "writer's activity", 30*time.Second, func() bool { return len(b.MyActivity(10).Posts) == 4 })
	if act := a.MyActivity(10); len(act.Liked) != 1 || act.Liked[0].ID != plain.ID || len(act.Posts) != 0 {
		t.Fatalf("liker's activity: %+v", act)
	}
	from, _ := cryptox.ParseAddress(plain.TipTo)
	var link *WarningError
	if err := b.SendFromTips(from, b.Wallet.Address(), tip/4, false); !errors.As(err, &link) {
		t.Fatalf("moving anonymous tips to the wallet without a warning: %v", err)
	}

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
	var pseudo, unnamed int
	for _, v := range c.Board("익명", 0, 10) {
		if v.Lane != "anon" || v.Author == wallet || v.TipTo == wallet {
			t.Fatalf("post leaks its author: %+v", v)
		}
		if v.Pseudo && v.Author == b.RLNStatus().Pseudonym {
			pseudo++
		} else if v.Author == "" {
			unnamed++
		}
	}
	if pseudo != 1 || unnamed != 3 {
		t.Fatalf("pseudonym %d, anonymous %d", pseudo, unnamed)
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

// TestLikesAndTips: a like always carries coins; likes wait until the
// user (or the auto-send setting, off by default) sends them; the coins
// reach the post's author and the like is counted once per account; a
// waiting like can be changed or taken back.
func TestLikesAndTips(t *testing.T) {
	p := fastParams()
	a := start(t, p, true)
	b := start(t, p, false, a.Host.Addr())
	waitFor(t, "funds", 60*time.Second, func() bool { return a.Chain.Tip().Peek(a.Wallet.Address()).Balance > 30*params.Coin })
	if err := a.Send(&types.Tx{Kind: types.TxTransfer, To: b.Wallet.Address(), Amount: 20 * params.Coin}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "author funded", 60*time.Second, func() bool { return b.Chain.Tip().Peek(b.Wallet.Address()).Balance > 0 })
	var ids []cryptox.Hash
	for i := 0; i < 3; i++ {
		tx := &types.Tx{Kind: types.TxPost, Board: "좋아요", Lane: types.LaneFee}
		tx.SetText([]byte(fmt.Sprintf("좋아요 시험 글 %d", i)))
		if err := b.Send(tx); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tx.ID())
	}
	waitFor(t, "posts indexed", 60*time.Second, func() bool {
		for _, id := range ids {
			if _, ok := a.Index.post(id); !ok {
				return false
			}
		}
		return true
	})
	min := a.MinLikeAmount()
	if _, err := a.QueueLike(LikeRequest{ID: ids[0]}); err == nil {
		t.Fatal("a like without coins queued")
	}
	if _, err := a.QueueLike(LikeRequest{ID: ids[0], Amount: min - 1}); err == nil {
		t.Fatal("a like below the minimum queued")
	}
	tip := uint64(params.Coin)
	for i, id := range ids {
		amt := min
		if i == 1 {
			amt = tip
		}
		if _, err := a.QueueLike(LikeRequest{ID: id, Amount: amt}); err != nil {
			t.Fatal(err)
		}
	}
	if !a.UnqueueLike(ids[2]) {
		t.Fatal("take-back failed")
	}
	if _, err := b.QueueLike(LikeRequest{ID: ids[0], Amount: tip}); err == nil {
		t.Fatal("liked own post")
	}
	// Auto-send is off: nothing leaves by itself.
	h := a.Chain.Height()
	waitFor(t, "a few blocks", 60*time.Second, func() bool { return a.Chain.Height() >= h+3 })
	q := a.QueuedLikes()
	if len(q.Likes) != 2 || q.Coins != tip+min || q.Next != 2 || q.Fee == 0 {
		t.Fatalf("queue %+v", q)
	}
	before := a.Chain.Tip().Peek(b.Wallet.Address()).Balance
	if err := a.SetLikeAutoSend(LikeAuto{Count: 2}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "auto-sent", 30*time.Second, func() bool { return len(a.QueuedLikes().Likes) == 0 })
	waitFor(t, "likes counted", 60*time.Second, func() bool {
		v0, _ := a.Index.post(ids[0])
		v1, _ := a.Index.post(ids[1])
		v2, _ := a.Index.post(ids[2])
		return v0.Likes == 1 && v0.Tips == min && v1.Likes == 1 && v1.Tips == tip && v2.Likes == 0
	})
	if got := a.Chain.Tip().Peek(b.Wallet.Address()).Balance; got < before+tip+min {
		t.Fatalf("author balance %d, want at least %d", got, before+tip+min)
	}
	// Coins sent to someone other than the author are not the post's like.
	v2, _ := a.Index.post(ids[2])
	stray := &types.Tx{Kind: types.TxLikes, Tips: []types.Tip{{Ref: types.RefOf(ids[2], v2.Height), To: types.Address{7}, Amount: 2 * a.Chain.Tip().MinPledge()}}}
	if err := a.Send(stray); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stray tip mined", 60*time.Second, func() bool { return a.Chain.Tip().Peek(types.Address{7}).Balance > 0 })
	if v, _ := a.Index.post(ids[2]); v.Tips != 0 || v.Likes != 0 {
		t.Fatalf("a tip to another address counted: %+v", v)
	}
	// Liking again sends more coins; the like counts once.
	if _, err := a.QueueLike(LikeRequest{ID: ids[0], Amount: tip, Now: true, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second like", 60*time.Second, func() bool { v, _ := a.Index.post(ids[0]); return v.Tips == tip+min })
	if v, _ := a.Index.post(ids[0]); v.Likes != 1 || !a.Index.Liked(ids[0], a.Wallet.Address()) || v.TipTo != b.Wallet.Address().String() {
		t.Fatalf("after the second like: %+v", v)
	}
}

// TestCrowdfunding: a creator with no coins signs a request; a backer
// checks a sample of the data, pledges (carrying the request), and the
// creator's node stores the data by itself once the deadline nears. Every
// node takes the store's block only with the whole data at hand.
func TestCrowdfunding(t *testing.T) {
	p := fastParams()
	a := start(t, p, true)
	b := start(t, p, false, a.Host.Addr())
	c := start(t, p, false, a.Host.Addr())
	waitFor(t, "funds", 60*time.Second, func() bool { return a.Chain.Tip().Peek(a.Wallet.Address()).Balance > 10*params.Coin })
	if b.Chain.Tip().Peek(b.Wallet.Address()).Balance != 0 {
		t.Fatal("creator should have no coins")
	}
	q, code, err := b.CreateFund(pngBytes(), "후원", "남기고 싶은 그림", 12, 30)
	if err != nil {
		t.Fatal(err)
	}
	// The request reaches every node's list by gossip (relayed by a to c),
	// with no code passed around.
	waitFor(t, "request gossiped", 30*time.Second, func() bool {
		l := c.FundList(10)
		return len(l) == 1 && l[0].ID == q.ID().String() && l[0].Note == "남기고 싶은 그림" && l[0].Fund.Status == "new"
	})
	if l := b.FundList(10); len(l) != 1 || !l[0].Listed || !l[0].Fund.Mine {
		t.Fatalf("creator's list: %+v", l)
	}
	if _, err := a.CheckFund(code); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "spot check", 30*time.Second, func() bool {
		fc, err := a.FundStatus(q.ID())
		return err == nil && fc.Verified == fc.Asked && fc.Asked > 0 && len(fc.Preview) > 0
	})
	if _, err := a.QueueLike(LikeRequest{ID: q.ID(), Amount: 2 * params.Coin, Fund: true, Now: true, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "request on chain", 60*time.Second, func() bool { return b.Chain.Tip().Funds.Has(q.ID()) })
	waitFor(t, "listed from the chain", 30*time.Second, func() bool {
		l := c.FundList(10)
		return len(l) == 1 && l[0].OnChain && l[0].Fund.Backers == 1
	})
	waitFor(t, "data stored", 120*time.Second, func() bool { return a.Chain.Tip().Data.Has(q.DataRoot) })
	waitFor(t, "store block at a bystander", 60*time.Second, func() bool { return c.Chain.Tip().Data.Has(q.DataRoot) })
	if !c.Store.Complete(q.DataRoot) {
		t.Fatal("a node took the store's block without the whole data")
	}
	if a.Chain.Tip().Funds.Has(q.ID()) {
		t.Fatal("campaign open after its data was stored")
	}
	if l := c.FundList(10); len(l) != 0 {
		t.Fatalf("a stored request is still listed: %+v", l)
	}
	vs := a.Board("후원", 0, 10)
	if len(vs) != 1 || vs[0].Fund == nil || vs[0].Fund.Status != "stored" || vs[0].Likes != 1 {
		t.Fatalf("listing: %+v", vs)
	}
	if err := a.Chain.Tip().CheckSupply(); err != nil {
		t.Fatal(err)
	}
}

// TestLockedKeys: with a password the secret key files are locked (plain
// ones from before are locked on the next start); without it the node
// refuses to open.
func TestLockedKeys(t *testing.T) {
	p := fastParams()
	dir := t.TempDir()
	open := func(pw string) (*Node, error) {
		return New(Config{Network: "devnet", DataDir: dir, Listen: "", Params: p, Logf: t.Logf, Password: pw})
	}
	n, err := open("")
	if err != nil {
		t.Fatal(err)
	}
	addr := n.Wallet.Address()
	n.Stop()
	n, err = open("열쇠")
	if err != nil {
		t.Fatal(err)
	}
	if n.Wallet.Address() != addr {
		t.Fatal("wallet changed when it was locked")
	}
	n.Stop()
	if b, _ := os.ReadFile(filepath.Join(dir, p.Name, "wallet.key")); !keystore.Locked(b) {
		t.Fatal("wallet.key not locked")
	}
	if _, err := open(""); !errors.Is(err, keystore.ErrLocked) {
		t.Fatalf("opened without the password: %v", err)
	}
	if _, err := open("틀림"); !errors.Is(err, keystore.ErrPassword) {
		t.Fatalf("opened with a wrong password: %v", err)
	}
	n, err = open("열쇠")
	if err != nil || n.Wallet.Address() != addr {
		t.Fatalf("reopen: %v", err)
	}
	n.Stop()
}

// TestFundGossipBounds: a gossiped request needs its proof of work, a
// creator gets a few slots, and a full pool takes only stronger work.
func TestFundGossipBounds(t *testing.T) {
	p := fastParams()
	n := start(t, p, false)
	k, err := cryptox.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	h := n.Chain.Height()
	mk := func(i int) *types.FundRequest {
		q := &types.FundRequest{DataRoot: types.Hash{byte(i), 1}, Size: 1000, MinTier: 12, Deadline: h + 50, Board: "후원", Note: fmt.Sprint(i)}
		q.Sign(k)
		return q
	}
	q := mk(0)
	weak := uint64(0)
	for types.PoWBits(q.ID(), weak) >= int(p.GossipPoWBits) {
		weak++
	}
	if _, _, err := n.AddItem(p2p.ItemFund, fundWire(q, weak)); !errors.Is(err, p2p.ErrMisbehave) {
		t.Fatalf("request without enough work: %v", err)
	}
	for i := 0; i < maxCreatorFunds+1; i++ {
		q := mk(i)
		nonce, _ := types.SolvePoW(q.ID(), int(p.GossipPoWBits), nil)
		_, isNew, err := n.AddItem(p2p.ItemFund, fundWire(q, nonce))
		if i < maxCreatorFunds && (err != nil || !isNew) {
			t.Fatalf("request %d: %v", i, err)
		}
		if i == maxCreatorFunds && err == nil {
			t.Fatal("a creator went past its slots")
		}
		if i == 0 {
			if _, isNew, _ := n.AddItem(p2p.ItemFund, fundWire(q, nonce)); isNew {
				t.Fatal("the same request counted as new twice")
			}
		}
	}
	if len(n.ItemIDs(p2p.ItemFund)) != maxCreatorFunds || len(n.FundList(0)) != maxCreatorFunds {
		t.Fatal("pool contents")
	}
}

// fakePMP is a NAT-PMP router whose external address is 203.0.113.7 and
// which forwards internal port p from external port p+1.
func fakePMP(t *testing.T) string {
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 64)
		for {
			n, from, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			r := make([]byte, 16)
			switch {
			case n == 2:
				r[1] = 128
				copy(r[8:], []byte{203, 0, 113, 7})
				_, _ = c.WriteTo(r[:12], from)
			case n == 12:
				r[1] = 130
				copy(r[8:10], buf[4:6])
				binary.BigEndian.PutUint16(r[10:], binary.BigEndian.Uint16(buf[4:])+1)
				copy(r[12:], buf[8:12])
				_, _ = c.WriteTo(r, from)
			}
		}
	}()
	return c.LocalAddr().String()
}

// TestRelayBehindNAT: A and C accept no connections (as behind NAT) and
// share only B. A still gets C's data: B passes A's request on to C and the
// chunk back. B, whose router speaks NAT-PMP, announces its external port.
func TestRelayBehindNAT(t *testing.T) {
	p := fastParams()
	mk := func(listen string, cfgNAT *nat.Config, seeds ...string) *Node {
		n, err := New(Config{Network: "devnet", DataDir: t.TempDir(), Listen: listen, Seeds: seeds,
			Embargo: 300 * time.Millisecond, Params: p, Logf: t.Logf, NAT: cfgNAT})
		if err != nil {
			t.Fatal(err)
		}
		if err := n.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(n.Stop)
		return n
	}
	b := mk("127.0.0.1:0", &nat.Config{PMPGateways: []string{fakePMP(t)}, NoUPnP: true, Timeout: time.Second})
	a := mk("", nil, b.Host.Addr())
	c := mk("", nil, b.Host.Addr())
	_, port, _ := net.SplitHostPort(b.Host.Addr())
	pn, _ := strconv.Atoi(port)
	waitFor(t, "port mapped", 10*time.Second, func() bool {
		ext, _ := b.Host.NATStatus()
		return ext == fmt.Sprintf("203.0.113.7:%d", pn+1)
	})
	waitFor(t, "peers", 10*time.Second, func() bool { return b.Host.PeerCount() == 2 && a.Host.PeerCount() == 1 })
	data := make([]byte, 3*params.ChunkSize+100)
	for i := range data {
		data[i] = byte(i * 31)
	}
	root, err := c.Store.PutData(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Store.Size(root); ok {
		t.Fatal("the hub should not hold the data")
	}
	a.Host.Probe(root, uint64(len(data)), []uint64{0, 2})
	waitFor(t, "chunks relayed", 20*time.Second, func() bool { return len(a.Host.ProbeResult(root)) == 2 })
	if _, ok := b.Store.Size(root); ok {
		t.Fatal("the hub kept data it only relayed")
	}
}

// TestCoverTraffic: with cover traffic on, frames leave at a fixed rate and
// a transaction leaves only inside them, cut into frames and put back
// together by the stem peer.
func TestCoverTraffic(t *testing.T) {
	p := fastParams()
	cover := p2p.CoverConfig{Size: 300, Every: 50 * time.Millisecond}
	mk := func(mine bool, seeds ...string) *Node {
		n, err := New(Config{Network: "devnet", DataDir: t.TempDir(), Listen: "127.0.0.1:0", Seeds: seeds, Mine: mine,
			PledgeBytes: 1 << 40, Embargo: 300 * time.Millisecond, Params: p, Logf: t.Logf, Cover: cover})
		if err != nil {
			t.Fatal(err)
		}
		if err := n.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(n.Stop)
		return n
	}
	a := mk(true)
	b := mk(false, a.Host.Addr())
	waitFor(t, "funds", 60*time.Second, func() bool { return a.Chain.Tip().Peek(a.Wallet.Address()).Balance > 5*params.Coin })
	waitFor(t, "cover frames", 10*time.Second, func() bool { s, _, _ := b.Host.CoverStats(); return s > 10 })
	if err := a.Send(&types.Tx{Kind: types.TxTransfer, To: b.Wallet.Address(), Amount: params.Coin}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "transfer through cover frames", 60*time.Second, func() bool {
		return b.Chain.Tip().Peek(b.Wallet.Address()).Balance == params.Coin
	})
	tx := &types.Tx{Kind: types.TxPost, Board: "덮개", Lane: types.LaneFee}
	tx.SetText([]byte(strings.Repeat("여러 프레임으로 나뉘는 글 ", 20)))
	if err := b.Send(tx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "multi-frame post mined", 60*time.Second, func() bool { return a.Chain.Tip().Posts.Has(tx.ID()) })
	if _, filled, _ := b.Host.CoverStats(); filled < 2 {
		t.Fatalf("the post left in %d frames", filled)
	}
	if pi := b.Privacy(); !pi.Cover || pi.CoverBps <= 0 {
		t.Fatalf("privacy %+v", pi)
	}
}

// fakeSOCKS is a SOCKS5 proxy that resolves names through a table.
func fakeSOCKS(t *testing.T, names map[string]string) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 300)
				io.ReadFull(c, b[:2])
				io.ReadFull(c, b[:b[1]])
				c.Write([]byte{5, 2})
				io.ReadFull(c, b[:2])
				io.ReadFull(c, b[:b[1]])
				io.ReadFull(c, b[:1])
				io.ReadFull(c, b[:b[0]])
				c.Write([]byte{1, 0})
				io.ReadFull(c, b[:5])
				host := make([]byte, b[4])
				io.ReadFull(c, host)
				io.ReadFull(c, b[:2])
				target := names[net.JoinHostPort(string(host), strconv.Itoa(int(binary.BigEndian.Uint16(b[:2]))))]
				up, err := net.Dial("tcp", target)
				if err != nil {
					c.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				go io.Copy(up, c)
				io.Copy(c, up)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// TestOnionPeer: a Tor-only node reaches a peer by its onion address
// through the proxy, and announces nothing dialable itself.
func TestOnionPeer(t *testing.T) {
	p := fastParams()
	a := start(t, p, true)
	proxy := fakeSOCKS(t, map[string]string{"chainboardpeer.onion:7700": a.Host.Addr()})
	px, err := anonnet.ParseProxy("socks5://" + proxy)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(Config{Network: "devnet", DataDir: t.TempDir(), Listen: "", Seeds: []string{"chainboardpeer.onion:7700"},
		Embargo: 300 * time.Millisecond, Params: p, Logf: t.Logf, Proxy: px, ProxyOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Stop)
	waitFor(t, "connected through the proxy", 20*time.Second, func() bool { return b.Host.PeerCount() == 1 })
	waitFor(t, "synced", 60*time.Second, func() bool { return b.Chain.Height() > 2 && b.Chain.Height() >= a.Chain.Height()-1 })
	if pi := b.Privacy(); !pi.Proxy || !pi.ProxyOnly {
		t.Fatalf("privacy %+v", pi)
	}
}

// TestOnionService: the node publishes its listener through Tor's control
// port, keeps the service key locked, and announces the onion address.
func TestOnionService(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	adds := make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				rd := bufio.NewReader(c)
				for {
					l, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(l, "PROTOCOLINFO"):
						c.Write([]byte("250-PROTOCOLINFO 1\r\n250-AUTH METHODS=NULL\r\n250 OK\r\n"))
					case strings.HasPrefix(l, "AUTHENTICATE"):
						c.Write([]byte("250 OK\r\n"))
					case strings.HasPrefix(l, "ADD_ONION"):
						adds <- strings.TrimSpace(l)
						c.Write([]byte("250-ServiceID=chainboardtestservice\r\n250-PrivateKey=ED25519-V3:a2V5\r\n250 OK\r\n"))
					}
				}
			}(c)
		}
	}()
	p := fastParams()
	dir := t.TempDir()
	n, err := New(Config{Network: "devnet", DataDir: dir, Listen: "127.0.0.1:0", Params: p, Logf: t.Logf,
		TorControl: ln.Addr().String(), Password: "열쇠"})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	defer n.Stop()
	_, port, _ := net.SplitHostPort(n.Host.Addr())
	waitFor(t, "onion published", 10*time.Second, func() bool { return n.Host.Onion() == "chainboardtestservice.onion:"+port })
	if add := <-adds; !strings.Contains(add, "NEW:ED25519-V3 Port="+port+","+n.Host.Addr()) {
		t.Fatalf("ADD_ONION %q", add)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, p.Name, "onion.key")); !keystore.Locked(b) {
		t.Fatal("onion.key not locked")
	}
	if pi := n.Privacy(); pi.Onion != n.Host.Onion() {
		t.Fatalf("privacy %+v", pi)
	}
}

// noisePNG is a picture that does not compress: about w*h*3 bytes.
func noisePNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(7)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed = seed*1664525 + 1013904223
			img.Set(x, y, color.RGBA{byte(seed >> 24), byte(seed >> 16), byte(seed >> 8), 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

// TestBounty: a requester posts a bounty; a hunter without coins answers
// with an offer that travels by gossip; the requester sees the previews but
// cannot fetch the whole data; on acceptance the hunter's node shares the
// data and reveals it by itself, and the pot is the hunter's at once. Every
// node takes the reveal's block only with the whole data at hand.
func TestBounty(t *testing.T) {
	p := fastParams()
	p.BountyMaxBlocks, p.BountyRevealBlocks = 400, 20
	p.FileCapDivisor = 10 // room for a picture of a few chunks
	a := start(t, p, true)
	b := start(t, p, false, a.Host.Addr())
	c := start(t, p, false, a.Host.Addr())
	waitFor(t, "funds", 60*time.Second, func() bool { return a.Chain.Tip().Peek(a.Wallet.Address()).Balance > 10*params.Coin })
	id, err := a.CreateBounty("현상금", "비 오는 광화문 사진을 찾아요", nil, 3*params.Coin, 30, 12, true)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bounty listed", 60*time.Second, func() bool {
		l := b.BountyList(10)
		return len(l) == 1 && l[0].Status == "open" && l[0].Note == "비 오는 광화문 사진을 찾아요"
	})
	if _, err := a.CreateOffer(id, pngBytes(), nil, "내 것"); err == nil {
		t.Fatal("the requester answered its own bounty")
	}
	data := noisePNG(700, 700)
	o, err := b.CreateOffer(id, data, pngBytes(), "광화문에서 찍었어요")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "offer gossiped", 30*time.Second, func() bool { return len(a.BountyOffers(id)) == 1 })
	if bound := boundPreviewChunks(o.Size); bound == 0 || bound*4 > chunkCount(o.Size) {
		t.Fatalf("bound preview of %d chunks for %d", bound, chunkCount(o.Size))
	}
	waitFor(t, "previews", 30*time.Second, func() bool {
		pv, err := a.PreviewOffer(o.ID())
		return err == nil && len(pv.Free) > 0 && pv.BoundGot == pv.BoundWant && pv.BoundWant > 0
	})
	// The rest of the data is not served before acceptance.
	last := chunkCount(o.Size) - 1
	a.Host.Probe(o.DataRoot, o.Size, []uint64{last})
	time.Sleep(4 * time.Second)
	if got := a.Host.ProbeResult(o.DataRoot); len(got) != 0 {
		t.Fatal("the hunter served data beyond its preview before acceptance")
	}
	var warn *WarningError
	if err := a.AcceptOffer(o.ID(), false); !errors.As(err, &warn) || warn.Warnings[0].Code != "accept" {
		t.Fatalf("accepting without the warning: %v", err)
	}
	if err := a.AcceptOffer(o.ID(), true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "revealed and paid", 120*time.Second, func() bool {
		_, open := b.Chain.Tip().Bounties.Get(id)
		return !open && b.Chain.Tip().Peek(b.Wallet.Address()).Balance > 2*params.Coin
	})
	if !b.Chain.Tip().Data.Has(o.DataRoot) {
		t.Fatal("result not stored")
	}
	waitFor(t, "reveal block at a bystander", 60*time.Second, func() bool { return c.Chain.Tip().Data.Has(o.DataRoot) })
	if !c.Store.Complete(o.DataRoot) {
		t.Fatal("a node took the reveal's block without the whole data")
	}
	if err := a.Chain.Tip().CheckSupply(); err != nil {
		t.Fatal(err)
	}
}

func chunkCount(size uint64) uint64 { return (size + params.ChunkSize - 1) / params.ChunkSize }

// TestBadWorkBans: a peer that relays a request without its proof of work
// is dropped, and its key is refused when it dials again.
func TestBadWorkBans(t *testing.T) {
	p := fastParams()
	b := start(t, p, false)
	a := start(t, p, false, b.Host.Addr())
	waitFor(t, "connection", 10*time.Second, func() bool { return b.Host.PeerCount() == 1 })
	k, err := cryptox.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	q := &types.FundRequest{DataRoot: types.Hash{9}, Size: 1000, MinTier: 12, Deadline: b.Chain.Height() + 50, Board: "후원", Note: "약한 작업"}
	q.Sign(k)
	weak := uint64(0)
	for types.PoWBits(q.ID(), weak) >= int(p.GossipPoWBits) {
		weak++
	}
	a.Host.SubmitItem(p2p.ItemFund, fundWire(q, weak))
	waitFor(t, "the bad peer dropped", 10*time.Second, func() bool { return b.Host.PeerCount() == 0 })
	time.Sleep(3 * time.Second) // a keeps dialing its seed
	if b.Host.PeerCount() != 0 || len(b.ItemIDs(p2p.ItemFund)) != 0 {
		t.Fatal("a banned peer got back in, or its item was kept")
	}
}

// TestPasswordOnRunningNode: the key files can be locked, relocked and
// unlocked while the node runs; key files the node makes afterwards use the
// new password, and the reported state follows the files.
func TestPasswordOnRunningNode(t *testing.T) {
	p := fastParams()
	n := start(t, p, false)
	if s := n.KeyState(); s != "plain" {
		t.Fatalf("new node keys %s", s)
	}
	if err := n.ChangePassword("", "첫 비밀번호"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.newTipKey(); err != nil { // a key file made after the change
		t.Fatal(err)
	}
	if s := n.KeyState(); s != "locked" {
		t.Fatalf("after locking: %s", s)
	}
	if err := keystore.Check(n.dir, "첫 비밀번호"); err != nil {
		t.Fatalf("files not all under the new password: %v", err)
	}
	if err := n.ChangePassword("틀림", "두 번째"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("changed with a wrong current password: %v", err)
	}
	if err := n.ChangePassword("첫 비밀번호", "두 번째"); err != nil {
		t.Fatal(err)
	}
	if err := keystore.Check(n.dir, "두 번째"); err != nil || n.KeyState() != "locked" {
		t.Fatalf("relock: %v %s", err, n.KeyState())
	}
	if err := os.WriteFile(filepath.Join(n.dir, "onion.key"), []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := n.KeyState(); s != "partial" {
		t.Fatalf("with a plain file among locked ones: %s", s)
	}
	if err := n.ChangePassword("두 번째", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(n.dir, keystore.PlainMarker)); err != nil || n.KeyState() != "plain" {
		t.Fatalf("unlock: %v %s", err, n.KeyState())
	}
}
