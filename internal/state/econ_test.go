package state

import (
	"testing"

	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/types"
)

func TestEconomics(t *testing.T) {
	p := params.Testnet()
	// 500 MB kept 1 year at 6 replicas and 1 coin per raw GB-year = 3 coins.
	if got := StorageFee(p, params.Coin, 500_000_000, 6, 12, false); got != 3*params.Coin {
		t.Fatalf("storage fee %d", got)
	}
	// Permanent = 200 years up front.
	if got := StorageFee(p, params.Coin, 500_000_000, 6, 0, true); got != 600*params.Coin {
		t.Fatalf("permanent fee %d", got)
	}
	if BaseReward(p, 0) != 0 || BaseReward(p, p.RampBlocks/2) != p.BootstrapReward/2 {
		t.Fatal("ramp")
	}
	if BaseReward(p, p.RampBlocks) != p.BootstrapReward || BaseReward(p, p.RampBlocks+p.HalvingBlocks) != p.BootstrapReward/2 {
		t.Fatal("halving")
	}
	if FeedbackPermille(p, 0, 1) != 4000 || FeedbackPermille(p, 1<<40, 1) != 250 {
		t.Fatal("feedback clamps")
	}
	if f := FeedbackPermille(p, 1500, 1000); f != 1000 {
		t.Fatalf("at target ratio feedback should be 1.0, got %d", f)
	}
	// Base fee moves at most 12.5% per block and never below the minimum.
	if NextBaseFee(p, 800, p.TextCapFee()) != 900 || NextBaseFee(p, 800, 0) != 700 || NextBaseFee(p, 1, 0) != p.BaseFeeMin {
		t.Fatal("base fee")
	}
	// Price moves toward its average by at most 5% per day.
	ema := uint64(2 * params.Coin)
	price := NextPrice(p, params.Coin, &ema, 2*params.Coin)
	if price <= params.Coin || price > params.Coin+params.Coin/20/p.BlocksPerDay+1 {
		t.Fatalf("price step %d", price)
	}
	if TermWeight(p, true, 0, 5) != 1000 || TermWeight(p, false, 100, 200) != 0 {
		t.Fatal("term weight")
	}
	if w := TermWeight(p, false, 10*p.BlocksPerYear, 0); w != 500 {
		t.Fatalf("10-year data should weigh 0.5, got %d", w)
	}
}

// Block bytes cost at least the permanent price of a post body; the body
// itself pays only the congestion base fee besides its storage tier.
func TestBlockBytesFloor(t *testing.T) {
	s, _ := Genesis(params.Testnet())
	if got := s.PermByteFee(); got != 400 {
		t.Fatalf("permanent byte fee %d, want 400 units at 1 coin per GB-year", got)
	}
	tx := &types.Tx{Kind: types.TxLikes, From: make([]byte, 32), Sig: make([]byte, 64), MaxFee: 1 << 20, Likes: []types.LikeRef{{Height: 7}}}
	if got, want := s.BytesFee(tx), 400*uint64(tx.EncodedSize()); got != want {
		t.Fatalf("like pays %d, want %d", got, want)
	}
	post := &types.Tx{Kind: types.TxPost, From: make([]byte, 32), Sig: make([]byte, 64), Board: "b", Tier: 1}
	post.SetText(make([]byte, 300))
	if got, want := s.BytesFee(post), 400*uint64(post.EncodedSize())+s.BaseFee*300; got != want {
		t.Fatalf("post pays %d, want %d", got, want)
	}
	s.Price *= 2 // the floor follows the storage price
	if s.PermByteFee() != 800 {
		t.Fatal("floor does not follow the price")
	}
}
