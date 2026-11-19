package state

import (
	"math/big"

	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/types"
)

// All economics is integer arithmetic so every node computes identical values.

func mulDiv(a, b, c uint64) uint64 {
	if c == 0 {
		return 0
	}
	x := new(big.Int).Mul(new(big.Int).SetUint64(a), new(big.Int).SetUint64(b))
	x.Div(x, new(big.Int).SetUint64(c))
	if !x.IsUint64() {
		return ^uint64(0)
	}
	return x.Uint64()
}

// StorageFee is the up-front fee for keeping bytes for months (permanent when
// perm) at the current price: bytes × replicas × years × price / GB, rounded up.
func StorageFee(p *params.Params, price, bytes, replicas, months uint64, perm bool) uint64 {
	if perm {
		months = p.PermYears * 12
	}
	// bytes*replicas*months*price / (12*GB), computed in big ints.
	x := new(big.Int).SetUint64(bytes)
	x.Mul(x, new(big.Int).SetUint64(replicas))
	x.Mul(x, new(big.Int).SetUint64(months))
	x.Mul(x, new(big.Int).SetUint64(price))
	d := new(big.Int).SetUint64(12 * params.GB)
	x.Add(x, new(big.Int).Sub(d, big.NewInt(1)))
	x.Div(x, d)
	if !x.IsUint64() {
		return ^uint64(0)
	}
	return x.Uint64()
}

// BaseReward is the bootstrap schedule: a linear ramp, then halvings.
func BaseReward(p *params.Params, h uint64) uint64 {
	if h == 0 {
		return 0
	}
	if h < p.RampBlocks {
		return mulDiv(p.BootstrapReward, h, p.RampBlocks)
	}
	n := (h - p.RampBlocks) / p.HalvingBlocks
	if n >= 64 {
		return 0
	}
	return p.BootstrapReward >> n
}

// FeedbackPermille is the tail-issuance multiplier from the capacity ratio:
// (target × need / proven)², clamped to [0.25, 4]. Less proven storage than
// the target asks for more issuance; more asks for less.
func FeedbackPermille(p *params.Params, provenBytes, needBytes uint64) uint64 {
	if provenBytes == 0 {
		return 4000
	}
	r := mulDiv(p.CapacityTarget, needBytes, provenBytes) // permille
	f := mulDiv(r, r, 1000)
	if f < 250 {
		f = 250
	}
	if f > 4000 {
		f = 4000
	}
	return f
}

// TailReward is the per-block tail issuance.
func TailReward(p *params.Params, supply, provenBytes, needBytes uint64) uint64 {
	perYear := mulDiv(supply, p.TailRatePPM, 1_000_000)
	perBlock := perYear / p.BlocksPerYear
	return mulDiv(perBlock, FeedbackPermille(p, provenBytes, needBytes), 1000)
}

// PermByteFee is the floor price of one byte that stays in a block: the
// permanent storage price of a post body (TextReplicas copies for PermYears),
// since every full node keeps block bytes forever. It follows the storage
// price, so it stays the same in real terms when the coin's value moves.
func (s *State) PermByteFee() uint64 {
	return StorageFee(s.P, s.Price, 1, s.P.TextReplicas, 0, true)
}

// BytesFee is the base fee of a fee-lane transaction before surcharges: its
// block bytes at the congestion base fee but never below PermByteFee, and
// its off-chain body at the congestion base fee (the body's storage is paid
// separately, by its retention tier).
func (s *State) BytesFee(t *types.Tx) uint64 {
	onChain := uint64(t.EncodedSize())
	return satMul(max(s.BaseFee, s.PermByteFee()), onChain) + satMul(s.BaseFee, uint64(t.TextLen))
}

// NextBaseFee follows EIP-1559 on the fee lane: ±12.5% at most per block,
// targeting half of the lane's capacity.
func NextBaseFee(p *params.Params, base, used uint64) uint64 {
	target := p.TextCapFee() / 2
	if target == 0 {
		return base
	}
	var next uint64
	if used >= target {
		delta := mulDiv(base, used-target, target) / 8
		if delta == 0 {
			delta = 1
		}
		next = base + delta
	} else {
		next = base - mulDiv(base, target-used, target)/8
	}
	if next < p.BaseFeeMin {
		next = p.BaseFeeMin
	}
	return next
}

// NextCongestion moves the free-lane quota multiplier (permille).
func NextCongestion(p *params.Params, cur, used uint64) uint64 {
	if used > p.TextCapFree()/2 {
		cur = cur * 1125 / 1000
	} else {
		cur = cur * 1000 / 1125
	}
	if cur < 1000 {
		cur = 1000
	}
	if cur > 1_000_000 {
		cur = 1_000_000
	}
	return cur
}

// ProvenBytes estimates the raw storage behind a block's work. If every
// chunk of the weave were stored once, a step would unlock Σ length/span
// recall ranges of RecallChunks × sub-chunks tickets; the work per step over
// that is the number of replicas, times the weave size.
func (s *State) ProvenBytes(work *big.Int, steps uint64) uint64 {
	if steps == 0 {
		steps = 1
	}
	weave := s.WeaveChunks()
	const ppm = 1_000_000
	var ranges uint64 // per step, in ppm
	for _, p := range s.Partitions() {
		_, _, length, _ := s.PartitionRange(p)
		ranges += mulDiv(length, ppm, s.Span(length, weave))
	}
	tickets := new(big.Int).SetUint64(ranges)
	tickets.Mul(tickets, new(big.Int).SetUint64(s.P.RecallChunks*params.ChunkSize/params.SubChunkSize*steps))
	if tickets.Sign() == 0 {
		return 0
	}
	x := new(big.Int).Mul(work, new(big.Int).SetUint64(weave*params.ChunkSize))
	x.Mul(x, big.NewInt(ppm))
	x.Div(x, tickets)
	if !x.IsUint64() {
		return ^uint64(0)
	}
	return x.Uint64()
}

// ObservedPrice is what miners currently earn per raw GB-year.
func ObservedPrice(p *params.Params, incomePerBlock, provenBytes uint64) uint64 {
	if provenBytes == 0 {
		return 0
	}
	perYear := new(big.Int).Mul(new(big.Int).SetUint64(incomePerBlock), new(big.Int).SetUint64(p.BlocksPerYear))
	perYear.Mul(perYear, big.NewInt(params.GB))
	perYear.Div(perYear, new(big.Int).SetUint64(provenBytes))
	if !perYear.IsUint64() {
		return ^uint64(0)
	}
	return perYear.Uint64()
}

// NextPrice moves the price toward its 30-day moving average, by at most
// PriceMaxDayPPM per day. ema is updated in place.
func NextPrice(p *params.Params, price uint64, ema *uint64, observed uint64) uint64 {
	n := p.PriceEMABlocks
	if n == 0 {
		n = 1
	}
	if observed > 0 {
		if observed > *ema {
			*ema += (observed - *ema) / n
		} else {
			*ema -= (*ema - observed) / n
		}
	}
	maxStep := mulDiv(price, p.PriceMaxDayPPM, 1_000_000) / p.BlocksPerDay
	if maxStep == 0 {
		maxStep = 1
	}
	switch {
	case *ema > price:
		d := *ema - price
		if d > maxStep {
			d = maxStep
		}
		price += d
	case *ema < price:
		d := price - *ema
		if d > maxStep {
			d = maxStep
		}
		price -= d
	}
	if price == 0 {
		price = 1
	}
	return price
}

// DupMultiplier doubles the cost of the same text each time it is repeated
// inside the window.
func DupMultiplier(count uint64) uint64 {
	if count > 20 {
		count = 20
	}
	return 1 << count
}

// TermWeight is a chunk's mining weight in permille: permanent data counts
// fully, term data by its remaining life over 20 years. This makes permanent
// and long-lived data the most valuable to keep, and makes cheap short-lived
// uploads a poor way to buy mining power.
func TermWeight(p *params.Params, perm bool, expiry, h uint64) uint64 {
	if perm {
		return 1000
	}
	if expiry <= h {
		return 0
	}
	w := mulDiv(expiry-h, 1000, 20*p.BlocksPerYear)
	if w < 1 {
		w = 1
	}
	if w > 1000 {
		w = 1000
	}
	return w
}

// BlockReward is the issuance of the block at height h on top of this state.
func (s *State) BlockReward(h uint64) uint64 {
	return BaseReward(s.P, h) + TailReward(s.P, s.Supply, s.Proven, s.NeedBytes())
}
