package state

import (
	"testing"

	"github.com/namsaeg/chainboard/internal/params"
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
