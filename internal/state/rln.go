package state

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/rln"
	"github.com/namsaeg/chainboard/internal/types"
)

// MaxSlashTxs bounds slash transactions per block.
const MaxSlashTxs = 64

var (
	// ErrDuplicateAnon is an anonymous post whose share is already known:
	// the same post again, possibly with a re-randomised proof.
	ErrDuplicateAnon = errors.New("duplicate anonymous post")
	// ErrNotMember means the secret has no live membership in this state.
	ErrNotMember = errors.New("membership not active")
)

// MemberStatus is the life cycle of an RLN membership.
type MemberStatus uint8

const (
	MemberActive  MemberStatus = iota + 1
	MemberLeaving              // still in the tree and slashable until Release
	MemberGone                 // released or slashed; the commitment can never return
)

// Member is one RLN membership, keyed by its identity commitment.
type Member struct {
	Owner   Address
	Index   uint32 // leaf in the membership tree
	Limit   uint16
	Bond    uint64
	Status  MemberStatus
	Release uint64 // height the bond returns (leaving)
}

// Share is the point an anonymous post revealed for its nullifier.
type Share struct{ X, Y Hash }

// exit is a scheduled membership release.
type exit struct {
	At  uint64
	IDC Hash
}

// DoubleSignal is returned when an anonymous post reuses a nullifier with a
// different signal and a valid proof: the two shares reveal the secret.
type DoubleSignal struct {
	Epoch     uint64
	Nullifier Hash
	First     Share
	Second    Share
}

func (d *DoubleSignal) Error() string {
	return "anonymous post exceeds its rate limit (double signal)"
}

// Secret recovers the member secret from the two shares.
func (d *DoubleSignal) Secret() (Hash, bool) {
	x1, ok1 := rln.FromBytes(d.First.X)
	y1, ok2 := rln.FromBytes(d.First.Y)
	x2, ok3 := rln.FromBytes(d.Second.X)
	y2, ok4 := rln.FromBytes(d.Second.Y)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return Hash{}, false
	}
	s, ok := rln.Recover(x1, y1, x2, y2)
	return rln.ToBytes(s), ok
}

// RLNEpoch is the rate-limit epoch of a height.
func (s *State) RLNEpoch(h uint64) uint64 { return h / s.P.RLNEpochBlocks }

// CapFor is the per-member message cap of an epoch still accepted (current
// or previous). The cap shrinks when the anonymous lane is congested, so
// heavy members are held back first.
func (s *State) CapFor(epoch uint64) (uint64, bool) {
	switch {
	case epoch == s.RLNCapEpoch:
		return s.RLNCap, true
	case epoch+1 == s.RLNCapEpoch:
		return s.RLNCapPrev, true
	}
	return 0, false
}

func (s *State) capFromCongestion() uint64 {
	c := mulDiv(s.P.RLNMaxLimit, 1000, s.Congestion)
	if c < 1 {
		c = 1
	}
	if c > s.P.RLNMaxLimit {
		c = s.P.RLNMaxLimit
	}
	return c
}

// RootEntry is a membership root and the height it was replaced at
// (0 while it is the current root).
type RootEntry struct {
	Root     Hash
	Replaced uint64
}

// RootKnown reports whether a membership root is accepted at height h: the
// current root always, a replaced one for RLNRootWindow blocks.
func (s *State) RootKnown(r Hash, h uint64) bool {
	for i := len(s.RLNRoots) - 1; i >= 0; i-- {
		e := s.RLNRoots[i]
		if e.Root == r {
			return e.Replaced == 0 || h <= e.Replaced+s.P.RLNRootWindow
		}
	}
	return false
}

// RLNRoot is the current membership root.
func (s *State) RLNRoot() Hash { return rln.ToBytes(s.RLNTree.Root()) }

// pushRoot records a tree change made at height h.
func (s *State) pushRoot(h uint64) {
	r := s.RLNRoot()
	n := len(s.RLNRoots)
	if n > 0 && s.RLNRoots[n-1].Root == r {
		return
	}
	keep := make([]RootEntry, 0, n+1)
	for _, e := range s.RLNRoots {
		if e.Replaced == 0 {
			e.Replaced = h
		}
		if h <= e.Replaced+s.P.RLNRootWindow {
			keep = append(keep, e)
		}
	}
	s.RLNRoots = append(keep, RootEntry{Root: r})
}

// MemberPath returns what a member needs to prove against the current root.
func (s *State) MemberPath(idc Hash) (m Member, path [rln.Depth]fr.Element, bits [rln.Depth]uint8, ok bool) {
	mp, found := s.Members.Get(idc)
	if !found || mp.Status == MemberGone {
		return Member{}, path, bits, false
	}
	path, bits = s.RLNTree.Path(mp.Index)
	return *mp, path, bits, true
}

// LaneSlots is the anonymous lane's capacity per epoch, in reference posts.
func (s *State) LaneSlots() uint64 {
	return max(1, s.P.TextCapFree()*s.P.RLNEpochBlocks/s.P.RLNRefPostBytes)
}

// BondFor is the bond a membership of limit messages per epoch needs now:
// the base per message follows the storage price, and grows with how much
// of the lane memberships would book after this one (see params).
func (s *State) BondFor(limit uint64) uint64 {
	base := mulDiv(s.P.RLNBondPerMsg, s.Price, s.P.InitialPrice)
	u := new(big.Int).SetUint64(s.MemberSlots + limit) // u = slots / LaneSlots
	u4 := new(big.Int).Exp(u, big.NewInt(4), nil)
	slots4 := new(big.Int).Exp(new(big.Int).SetUint64(s.LaneSlots()), big.NewInt(4), nil)
	// bond = limit × base × (1 + curve × u⁴)
	x := new(big.Int).Mul(u4, new(big.Int).SetUint64(s.P.RLNBondCurve))
	x.Add(x, slots4)
	x.Mul(x, new(big.Int).SetUint64(limit*base))
	x.Div(x, slots4)
	if !x.IsUint64() {
		return ^uint64(0)
	}
	return x.Uint64()
}

// RLNLimitFor is the largest message limit a bond buys now.
func (s *State) RLNLimitFor(bond uint64) uint64 {
	l := uint64(0)
	for l < s.P.RLNMaxLimit && s.BondFor(l+1) <= bond {
		l++
	}
	return l
}

func field(h Hash) (fr.Element, error) {
	e, ok := rln.FromBytes(h)
	if !ok {
		return e, errors.New("value is not a canonical field element")
	}
	return e, nil
}

// checkJoin validates a membership join for an account (already settled).
func (s *State) checkJoin(t *types.Tx, acc *Account) error {
	if _, err := field(t.Commitment); err != nil {
		return err
	}
	if s.Members.Has(t.Commitment) {
		return errors.New("identity commitment already used")
	}
	if uint64(t.Limit) > s.P.RLNMaxLimit {
		return fmt.Errorf("limit above %d per epoch", s.P.RLNMaxLimit)
	}
	if need := s.BondFor(uint64(t.Limit)); t.Amount < need {
		return fmt.Errorf("bond too small: %d messages need %d units now", t.Limit, need)
	}
	if acc.StakeMature < t.Amount {
		if acc.StakeMature+acc.StakePending >= t.Amount {
			return fmt.Errorf("bond must come from mature stake; pending stake matures at height %d",
				acc.PendingSince+s.P.StakeAgingBlocks)
		}
		return errors.New("bond must come from mature stake")
	}
	if s.RLNTree.Full() {
		return errors.New("membership tree full")
	}
	return nil
}

func (s *State) applyJoin(t *types.Tx, owner Address, acc *Account, h uint64) {
	acc.StakeMature -= t.Amount
	idc, _ := field(t.Commitment)
	i := s.RLNTree.Append(rln.Leaf(idc, uint64(t.Limit)))
	s.Members.Set(t.Commitment, &Member{Owner: owner, Index: i, Limit: t.Limit, Bond: t.Amount, Status: MemberActive})
	s.MemberCount++
	s.MemberSlots += uint64(t.Limit)
	s.pushRoot(h)
}

func (s *State) checkLeave(t *types.Tx, owner Address) error {
	m, ok := s.Members.Get(t.Commitment)
	if !ok || m.Owner != owner {
		return errors.New("not your membership")
	}
	if m.Status != MemberActive {
		return errors.New("membership already leaving or gone")
	}
	return nil
}

func (s *State) applyLeave(t *types.Tx, h uint64) {
	m := s.cloneMember(t.Commitment)
	m.Status = MemberLeaving
	m.Release = h + s.P.UnbondBlocks
	s.RLNExits = append(s.RLNExits, exit{At: m.Release, IDC: t.Commitment})
}

// cloneMember replaces a member with a private copy before it is modified,
// so states that share nothing stay independent.
func (s *State) cloneMember(idc Hash) *Member {
	m, _ := s.Members.Get(idc)
	c := *m
	s.Members.Set(idc, &c)
	return &c
}

// releaseExits returns the bonds of memberships whose unbonding ended.
func (s *State) releaseExits(h uint64) {
	for len(s.RLNExits) > 0 && s.RLNExits[0].At <= h {
		e := s.RLNExits[0]
		s.RLNExits = s.RLNExits[1:]
		m, ok := s.Members.Get(e.IDC)
		if !ok || m.Status != MemberLeaving {
			continue // slashed meanwhile
		}
		m = s.cloneMember(e.IDC)
		s.RLNTree.Set(m.Index, fr.Element{})
		s.pushRoot(h)
		s.Account(m.Owner).Balance += m.Bond
		m.Bond = 0
		m.Status = MemberGone
		s.MemberCount--
		s.MemberSlots -= uint64(m.Limit)
	}
}

// rolloverEpoch fixes the cap of the next block's epoch and forgets
// nullifiers that can no longer be used.
func (s *State) rolloverEpoch(nextH uint64) {
	e := s.RLNEpoch(nextH)
	if e == s.RLNCapEpoch {
		return
	}
	if e == s.RLNCapEpoch+1 {
		s.RLNCapPrev = s.RLNCap
	} else {
		s.RLNCapPrev = s.capFromCongestion()
	}
	s.RLNCap = s.capFromCongestion()
	s.RLNCapEpoch = e
	for ep := range s.RLNNulls {
		if ep+1 < e {
			delete(s.RLNNulls, ep)
		}
	}
}

func proofKey(t *types.Tx, cap uint64, scope uint64) [32]byte {
	id := t.ID()
	return cryptox.Tagged("cb/rln/verified", id[:], cryptox.U64(cap), cryptox.U64(scope))
}

// applyAnon validates and applies an anonymous post. Atomic like ApplyTx.
func (s *State) applyAnon(t *types.Tx, ctx *BlockCtx) error {
	h := ctx.H
	p := t.RLN
	e := s.RLNEpoch(h)
	if p.Epoch != e && p.Epoch+1 != e {
		return errors.New("anonymous post for a past or future epoch")
	}
	cap, ok := s.CapFor(p.Epoch)
	if !ok {
		return errors.New("no cap for that epoch")
	}
	if !s.RootKnown(p.Root, h) {
		return errors.New("unknown or stale membership root")
	}
	root, err := field(p.Root)
	if err != nil {
		return err
	}
	null, err := field(p.Nullifier)
	if err != nil {
		return err
	}
	y, err := field(p.Y)
	if err != nil {
		return err
	}
	sig := t.Signal()
	x := rln.SignalX(sig[:])
	share := Share{X: rln.ToBytes(x), Y: p.Y}
	pub := rln.Public{Root: root, X: x, Y: y, Nullifier: null, Epoch: p.Epoch, Scope: s.P.RLNScope(), Cap: cap}
	// The nullifier comes first, so a double signal is caught whatever else
	// is wrong with the second post.
	if prev, used := s.NullShare(p.Epoch, p.Nullifier); used {
		if prev.X == share.X {
			return ErrDuplicateAnon
		}
		if rln.VerifyCached(proofKey(t, cap, pub.Scope), p.Proof, pub) != nil {
			return errors.New("nullifier already used")
		}
		return &DoubleSignal{Epoch: p.Epoch, Nullifier: p.Nullifier, First: prev, Second: share}
	}
	if err := s.checkParent(t); err != nil {
		return err
	}
	dupKey, mul := s.dupMultiplier(t, h)
	if mul > 1 {
		return errors.New("repeated text must use the fee lane")
	}
	size := uint64(t.LaneSize())
	if ctx.FreeUsed+size > s.P.TextCapFree() {
		return errors.New("anonymous lane full")
	}
	if !s.replay {
		if err := rln.VerifyCached(proofKey(t, cap, pub.Scope), p.Proof, pub); err != nil {
			return err
		}
	}

	// Commit.
	fee, dest := s.textFee(uint64(t.TextLen), params.TierFree, h)
	ctx.FreeUsed += size
	ctx.FreeStorage += fee
	s.fund(dest, fee)
	nulls, ok := s.RLNNulls[p.Epoch]
	if !ok {
		nulls = newNullMap()
	}
	nulls.Set(p.Nullifier, share)
	s.RLNNulls[p.Epoch] = nulls
	s.notePost(t, h, dupKey, dest)
	s.queueText(t.ID(), t.TextHash, t.TextLen, dest)
	return nil
}

// applySlash removes a member whose secret is public and takes its bond.
func (s *State) applySlash(t *types.Tx, ctx *BlockCtx) error {
	sec, err := field(t.Secret)
	if err != nil {
		return err
	}
	idc := rln.ToBytes(rln.IdentityCommitment(sec))
	m, ok := s.Members.Get(idc)
	if !ok || m.Status == MemberGone {
		return errors.New("no slashable membership for that secret")
	}
	if ctx.Slashes >= MaxSlashTxs {
		return errors.New("too many slashes in block")
	}
	ctx.Slashes++
	m = s.cloneMember(idc)
	s.RLNTree.Set(m.Index, fr.Element{})
	// The secret is public now: proofs against older roots must stop working.
	s.RLNRoots = []RootEntry{{Root: s.RLNRoot()}}
	reward := mulDiv(m.Bond, s.P.RLNSlashPermille, 1000)
	ctx.Tips += reward
	ctx.Burn += m.Bond - reward
	m.Bond = 0
	m.Status = MemberGone
	s.MemberCount--
	s.MemberSlots -= uint64(m.Limit)
	return nil
}

// AnonMined reports whether this state already holds the share of an
// anonymous post: the same post, whatever its transaction ID.
func (s *State) AnonMined(t *types.Tx) bool {
	if t.RLN == nil {
		return false
	}
	prev, ok := s.NullShare(t.RLN.Epoch, t.RLN.Nullifier)
	if !ok {
		return false
	}
	sig := t.Signal()
	return prev.X == rln.ToBytes(rln.SignalX(sig[:]))
}

// NullShare is the share recorded under a nullifier of an epoch.
func (s *State) NullShare(epoch uint64, null Hash) (Share, bool) {
	m, ok := s.RLNNulls[epoch]
	if !ok {
		return Share{}, false
	}
	return m.Get(null)
}

// AnonEpochCap is the epoch the next block belongs to and its message cap.
func (s *State) AnonEpochCap() (epoch, cap uint64) {
	epoch = s.RLNEpoch(s.Height + 1)
	cap, _ = s.CapFor(epoch)
	return
}

// ProveAnon fills in the RLN proof of an anonymous post (content and any
// pseudonym signature already set) for the member holding secret, as
// message msgID of the next block's epoch.
func (s *State) ProveAnon(t *types.Tx, secret fr.Element, msgID uint64) error {
	idc := rln.ToBytes(rln.IdentityCommitment(secret))
	m, path, bits, ok := s.MemberPath(idc)
	if !ok {
		return ErrNotMember
	}
	epoch, cap := s.AnonEpochCap()
	signal := t.Signal()
	scope := s.P.RLNScope()
	a1 := rln.A1(secret, epoch, scope, msgID)
	x := rln.SignalX(signal[:])
	pub := rln.Public{Root: s.RLNTree.Root(), X: x, Y: rln.Share(secret, a1, x), Nullifier: rln.Nullifier(a1),
		Epoch: epoch, Scope: scope, Cap: cap}
	proof, err := rln.Prove(pub, rln.Witness{Secret: secret, Limit: uint64(m.Limit), MsgID: msgID, Path: path, Bits: bits})
	if err != nil {
		return fmt.Errorf("rln proof: %w", err)
	}
	t.RLN = &types.RLNProof{Root: rln.ToBytes(pub.Root), Epoch: epoch, Nullifier: rln.ToBytes(pub.Nullifier),
		Y: rln.ToBytes(pub.Y), Proof: proof}
	return nil
}
