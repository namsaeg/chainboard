package state

import (
	"testing"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"

	"github.com/namsaeg/chainboard/internal/params"
)

func TestRLNCapFollowsCongestion(t *testing.T) {
	p := params.Devnet()
	s, _ := Genesis(p)
	if c, ok := s.CapFor(s.RLNEpoch(1)); !ok || c != p.RLNMaxLimit {
		t.Fatalf("genesis cap %d", c)
	}
	s.Congestion = 8000
	e := s.RLNEpoch(1)
	s.rolloverEpoch((e + 1) * p.RLNEpochBlocks)
	cur, _ := s.CapFor(e + 1)
	prev, _ := s.CapFor(e)
	if cur != p.RLNMaxLimit/8 || prev != p.RLNMaxLimit {
		t.Fatalf("cap %d prev %d", cur, prev)
	}
	if _, ok := s.CapFor(e - 1); ok && e > 0 {
		t.Fatal("old epoch still accepted")
	}
	s.Congestion = 1_000_000
	s.rolloverEpoch((e + 5) * p.RLNEpochBlocks)
	if c, _ := s.CapFor(e + 5); c != 1 {
		t.Fatalf("cap under full congestion %d", c)
	}
	if _, ok := s.CapFor(e + 1); ok {
		t.Fatal("skipped epochs left a stale cap")
	}
}

func TestReplacedRootsExpire(t *testing.T) {
	p := params.Devnet()
	s, _ := Genesis(p)
	r0 := s.RLNRoot()
	var leaf fr.Element
	leaf.SetUint64(5)
	s.RLNTree.Append(leaf)
	s.pushRoot(10)
	r1 := s.RLNRoot()
	if !s.RootKnown(r0, 10+p.RLNRootWindow) || s.RootKnown(r0, 11+p.RLNRootWindow) {
		t.Fatal("replaced root window wrong")
	}
	if !s.RootKnown(r1, 1_000_000) {
		t.Fatal("current root must never expire")
	}
	s.RLNTree.Append(leaf)
	s.pushRoot(100) // r0 is long gone, r1 is replaced now
	if len(s.RLNRoots) != 2 || !s.RootKnown(r1, 100+p.RLNRootWindow) {
		t.Fatalf("history %v", s.RLNRoots)
	}
}

// The bond follows the storage price and stays near its base while the
// lane is lightly booked; booking half the lane costs an order of magnitude
// more than at a flat rate.
func TestBondCurve(t *testing.T) {
	s, _ := Genesis(params.Testnet())
	base := s.P.RLNBondPerMsg
	if got := s.BondFor(30); got < 30*base || got > 30*base*101/100 {
		t.Fatalf("30 slots on an empty lane bond %d", got)
	}
	s.MemberSlots = s.LaneSlots() / 10 // a tenth of the lane booked
	if got := s.BondFor(30); got > 30*base*103/100 {
		t.Fatalf("30 slots at 10%% booking bond %d", got)
	}
	s.Price *= 2
	if got := s.BondFor(30); got < 60*base {
		t.Fatal("bond does not follow the price")
	}
	s.Price /= 2
	// A spammer booking 6,370 slots (half the lane with minimal posts) in
	// memberships of 30, each priced at the booking it brings the lane to.
	s.MemberSlots = 0
	var total uint64
	for booked := uint64(0); booked < 6370; booked += 30 {
		total += s.BondFor(30)
		s.MemberSlots += 30
	}
	if total < 10*6370*base {
		t.Fatalf("half-lane booking bonds %d coin, want at least ten times %d", total/params.Coin, 6370)
	}
}
