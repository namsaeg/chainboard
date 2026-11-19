package node

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mrand "math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/rln"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

// The anonymous lane keeps three files next to the wallet:
//
//	rln.key        the membership secret (whoever holds it can post as the member)
//	rln_used.json  message ids already spent per epoch; reusing one for other
//	               content would reveal the secret and get the bond slashed,
//	               so it is written before any proof leaves the node
//	pseudonym.key  optional Ed25519 key for signed pseudonymous posts, kept
//	               apart from the wallet so posts cannot be linked to stake

// AnonRequest is an anonymous post waiting to be proven and sent.
type AnonRequest struct {
	Board  string
	Text   string
	Parent cryptox.Hash
	Pseudo bool
	// Confirm accepts the warning shown before the epoch's last post.
	Confirm bool
}

type rlnIdentity struct {
	Secret string `json:"secret"`
}

type rlnUsage struct {
	// epoch -> message id -> signal (hex) it was spent on
	Epochs map[uint64]map[uint16]string `json:"epochs"`
}

// outboxItem is an anonymous post that left (or failed to leave) and is not
// mined yet. waitJoin marks one held back until our membership is mined.
type outboxItem struct {
	req      AnonRequest
	id       cryptox.Hash
	sentAt   uint64
	tries    int
	waitJoin bool
	since    time.Time
}

// resendAfter blocks without inclusion, an anonymous post is proven again
// (its root may have aged out or the stem may have dropped it). A post
// written right after joining waits up to joinWait for the join to be mined.
const (
	resendAfter = 5
	maxTries    = 3
	joinWait    = 30 * time.Minute
)

// errLimit means every message id of the epoch is spent.
var errLimit = errors.New("anonymous post limit reached for this epoch")

func (n *Node) rlnPath(name string) string { return filepath.Join(n.dir, name) }

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (n *Node) loadRLN() error {
	n.usage = rlnUsage{Epochs: map[uint64]map[uint16]string{}}
	if b, err := os.ReadFile(n.rlnPath("rln_used.json")); err == nil {
		if err := json.Unmarshal(b, &n.usage); err != nil {
			return fmt.Errorf("rln_used.json: %w", err)
		}
		if n.usage.Epochs == nil {
			n.usage.Epochs = map[uint64]map[uint16]string{}
		}
	}
	b, err := os.ReadFile(n.rlnPath("rln.key"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var id rlnIdentity
	if err := json.Unmarshal(b, &id); err != nil {
		return fmt.Errorf("rln.key: %w", err)
	}
	raw, err := hex.DecodeString(id.Secret)
	if err != nil || len(raw) != 32 {
		return errors.New("rln.key: bad secret")
	}
	sec, ok := rln.FromBytes(cryptox.Hash(raw))
	if !ok {
		return errors.New("rln.key: secret out of range")
	}
	n.secret = &sec
	return nil
}

func (n *Node) newIdentity() error {
	var s fr.Element
	if _, err := s.SetRandom(); err != nil {
		return err
	}
	b := rln.ToBytes(s)
	if err := writeFileAtomic(n.rlnPath("rln.key"), []byte(`{"secret":"`+hex.EncodeToString(b[:])+`"}`+"\n")); err != nil {
		return err
	}
	n.secret = &s
	return nil
}

func (n *Node) commitment() (cryptox.Hash, bool) {
	if n.secret == nil {
		return cryptox.Hash{}, false
	}
	return rln.ToBytes(rln.IdentityCommitment(*n.secret)), true
}

// RLNInfo describes this node's anonymous-lane membership.
type RLNInfo struct {
	Member     bool   `json:"member"`
	Status     string `json:"status"` // none, pending, active, leaving, gone
	Commitment string `json:"commitment,omitempty"`
	Limit      uint64 `json:"limit"`
	Bond       uint64 `json:"bond"`
	Release    uint64 `json:"release,omitempty"`
	Epoch      uint64 `json:"epoch"`
	Cap        uint64 `json:"cap"`
	Used       int    `json:"used"`
	EpochLeft  uint64 `json:"epoch_blocks_left"`
	Members    int    `json:"members"`
	Pseudonym  string `json:"pseudonym,omitempty"`
	Remaining  uint64 `json:"remaining"`        // anonymous posts left this epoch
	ForeignUse bool   `json:"foreign_use"`      // another device uses this key
	BondPerMsg uint64 `json:"bond_per_message"` // bond of one message per epoch now
	BondFull   uint64 `json:"bond_full"`        // bond of the largest limit now
}

// RLNStatus reports the membership. Active means mined: proofs are made
// against mined membership roots. A join still travelling or pooled shows
// as pending.
func (n *Node) RLNStatus() RLNInfo {
	tip := n.Chain.Tip()
	h := tip.Height + 1
	e := tip.RLNEpoch(h)
	cap, _ := tip.CapFor(e)
	info := RLNInfo{Status: "none", Epoch: e, Cap: cap, EpochLeft: (e+1)*tip.P.RLNEpochBlocks - h}
	info.Members = int(tip.MemberCount)
	info.BondPerMsg, info.BondFull = tip.BondFor(1), tip.BondFor(tip.P.RLNMaxLimit)
	joining := n.joining()
	n.rlnMu.Lock()
	defer n.rlnMu.Unlock()
	if n.pseudo != nil {
		info.Pseudonym = n.pseudo.Address().String()
	}
	info.Used = len(n.usage.Epochs[e])
	info.ForeignUse = n.foreign
	idc, ok := n.commitment()
	if !ok {
		return info
	}
	info.Commitment = idc.String()
	m, found := tip.Members.Get(idc)
	switch {
	case !found:
		if joining {
			info.Status = "pending"
		}
	case m.Status == state.MemberActive:
		info.Status, info.Member = "active", true
	case m.Status == state.MemberLeaving:
		info.Status, info.Member, info.Release = "leaving", true, m.Release
	default:
		info.Status = "gone"
	}
	if found {
		info.Limit, info.Bond = uint64(m.Limit), m.Bond
		if lim := min(uint64(m.Limit), cap); lim > uint64(info.Used) && m.Status != state.MemberGone {
			info.Remaining = lim - uint64(info.Used)
		}
	}
	return info
}

// JoinRLN bonds mature stake for an anonymous-lane membership. The message
// limit per epoch is what the bond buys.
func (n *Node) JoinRLN(bond uint64) error {
	st := n.trialState()
	// The whole offered bond is used. The limit leaves room for two days
	// of the largest price move, as the bond follows the storage price and
	// the join may be mined a little later.
	headroom := 1_000_000 + 2*st.P.PriceMaxDayPPM
	limit := st.RLNLimitFor(bond / headroom * 1_000_000)
	if limit == 0 {
		need := st.BondFor(1)
		return fmt.Errorf("bond too small: one message per epoch needs %d.%08d coin now", need/params.Coin, need%params.Coin)
	}
	n.rlnMu.Lock()
	idc, ok := n.commitment()
	if ok {
		if m, found := st.Members.Get(idc); found {
			if m.Status != state.MemberGone {
				n.rlnMu.Unlock()
				return errors.New("already a member; leave first to change the bond")
			}
			ok = false // that identity is spent
		}
	}
	if !ok {
		if err := n.newIdentity(); err != nil {
			n.rlnMu.Unlock()
			return err
		}
		idc, _ = n.commitment()
	}
	n.rlnMu.Unlock()
	return n.Send(&types.Tx{Kind: types.TxRLNJoin, Amount: bond, Commitment: idc, Limit: uint16(limit)})
}

// pendingJoin finds our membership join while it is on its way: queued
// here, travelling the stem, or pooled, but not mined.
func (n *Node) pendingJoin() (*types.Tx, bool) {
	n.rlnMu.Lock()
	idc, ok := n.commitment()
	n.rlnMu.Unlock()
	if !ok {
		return nil, false
	}
	if _, mined := n.Chain.Tip().Members.Get(idc); mined {
		return nil, false
	}
	isJoin := func(t *types.Tx) bool { return t != nil && t.Kind == types.TxRLNJoin && t.Commitment == idc }
	n.mu.Lock()
	for _, q := range n.queue {
		if isJoin(q.tx) {
			n.mu.Unlock()
			return q.tx, true
		}
	}
	for _, s := range n.sent {
		if isJoin(s.tx) {
			n.mu.Unlock()
			return s.tx, true
		}
	}
	n.mu.Unlock()
	for _, t := range n.Pool.Pending() {
		if isJoin(t) {
			return t, true
		}
	}
	return nil, false
}

func (n *Node) joining() bool {
	_, ok := n.pendingJoin()
	return ok
}

// LeaveRLN starts the exit of the membership; the bond returns after the
// unbonding period, during which the membership can still be slashed.
func (n *Node) LeaveRLN() error {
	n.rlnMu.Lock()
	idc, ok := n.commitment()
	n.rlnMu.Unlock()
	if !ok {
		return errors.New("no membership")
	}
	return n.Send(&types.Tx{Kind: types.TxRLNLeave, Commitment: idc})
}

// AnonPost queues an anonymous post. It is proven when it leaves, after the
// same random delay as other own transactions.
func (n *Node) AnonPost(req AnonRequest) error {
	if len(req.Text) == 0 {
		return errors.New("empty post")
	}
	n.rlnMu.Lock()
	idc, ok := n.commitment()
	if ok && req.Pseudo && n.pseudo == nil {
		k, err := cryptox.LoadOrCreateKey(n.rlnPath("pseudonym.key"))
		if err != nil {
			n.rlnMu.Unlock()
			return err
		}
		n.pseudo = k
	}
	n.rlnMu.Unlock()
	if !ok {
		return errors.New("join the anonymous lane first (bond some mature stake)")
	}
	tip := n.Chain.Tip()
	m, found := tip.Members.Get(idc)
	switch {
	case found && m.Status == state.MemberGone:
		return errors.New("membership ended; join again")
	case !found && !n.joining():
		return errors.New("join the anonymous lane first (bond some mature stake)")
	}
	n.checkForeign(tip) // the tip watcher may not have looked yet
	n.rlnMu.Lock()
	foreign := n.foreign
	n.rlnMu.Unlock()
	if foreign {
		return ErrForeignUse
	}
	// Build a draft to surface content errors now rather than after the delay.
	draft := newAnonTx(req)
	if req.Pseudo && n.pseudo != nil {
		draft.PseudoPub = append([]byte(nil), n.pseudo.Pub...)
	}
	draft.RLN = &types.RLNProof{Proof: []byte{0}}
	if err := checkAnonContent(draft); err != nil {
		return err
	}
	if found {
		// Reserve the message id now, so the count is exact however many
		// posts wait in the queue, and warn before the epoch's last one.
		e, cap := tip.AnonEpochCap()
		limit := min(uint64(m.Limit), cap)
		left := (e+1)*tip.P.RLNEpochBlocks - (tip.Height + 1)
		mins := (time.Duration(left)*tip.P.BlockTime + time.Minute - 1) / time.Minute
		sig := draft.Signal()
		n.rlnMu.Lock()
		used := uint64(len(n.usage.Epochs[e]))
		again := false
		for _, s := range n.usage.Epochs[e] {
			again = again || s == sig.String()
		}
		if !again {
			if used >= limit {
				n.rlnMu.Unlock()
				return fmt.Errorf("%w (%d); the next epoch starts in %d blocks", errLimit, limit, left)
			}
			if used+1 == limit && !req.Confirm {
				n.rlnMu.Unlock()
				return &WarningError{Warnings: []Warning{{Code: "anon_last", Message: fmt.Sprintf(
					"이번 에포크에 쓸 수 있는 마지막 익명 글이에요(%d/%d). 보내면 약 %d분(%d블록) 뒤 다음 에포크까지 익명 글을 더 쓸 수 없어요.",
					limit, limit, mins, left)}}}
			}
		}
		_, err := n.allocMsgID(e, limit, sig)
		n.rlnMu.Unlock()
		if err != nil {
			return err
		}
	} else if join, ok := n.pendingJoin(); ok {
		// Joining: posts wait for the join. Count them against the limit
		// the join asks for, so the warning still comes first.
		_, cap := tip.AnonEpochCap()
		limit := min(uint64(join.Limit), cap)
		waiting := uint64(len(n.ownAnon(false)))
		if waiting >= limit {
			return fmt.Errorf("%w (%d): this many posts already wait for the membership", errLimit, limit)
		}
		if waiting+1 == limit && !req.Confirm {
			return &WarningError{Warnings: []Warning{{Code: "anon_last", Message: fmt.Sprintf(
				"가입이 처리되면 이 글이 이번 에포크의 마지막 익명 글이 돼요(%d/%d). 그 뒤로는 다음 에포크까지 익명 글을 더 쓸 수 없어요.",
				limit, limit)}}}
		}
	}
	n.enqueue(queued{anon: &req})
	return nil
}

func (n *Node) enqueue(q queued) {
	delay := time.Duration(0)
	if n.cfg.SendDelayMax > 0 {
		delay = time.Duration(mrand.Int63n(int64(n.cfg.SendDelayMax)))
	}
	at := time.Now().Add(delay)
	n.mu.Lock()
	if at.Before(n.nextFree) {
		at = n.nextFree
	}
	n.nextFree = at
	q.sendAt = at
	n.queue = append(n.queue, q)
	n.mu.Unlock()
}

// allocMsgID picks the message id for a signal in an epoch and records it on
// disk before returning. The same signal always gets the same id, so a
// resend can never double-signal.
func (n *Node) allocMsgID(epoch, max uint64, signal cryptox.Hash) (uint64, error) {
	sig := signal.String()
	used := n.usage.Epochs[epoch]
	for id, s := range used {
		if s == sig {
			return uint64(id), nil
		}
	}
	for id := uint64(0); id < max; id++ {
		if _, taken := used[uint16(id)]; taken {
			continue
		}
		if used == nil {
			used = map[uint16]string{}
			n.usage.Epochs[epoch] = used
		}
		used[uint16(id)] = sig
		for e := range n.usage.Epochs {
			if e+1 < epoch {
				delete(n.usage.Epochs, e)
			}
		}
		if err := n.saveUsageLocked(); err != nil {
			delete(used, uint16(id))
			return 0, err
		}
		return id, nil
	}
	return 0, fmt.Errorf("%w (%d)", errLimit, max)
}

// saveUsageLocked writes the message-id ledger (rlnMu held).
func (n *Node) saveUsageLocked() error {
	b, err := json.Marshal(n.usage)
	if err != nil {
		return err
	}
	return writeFileAtomic(n.rlnPath("rln_used.json"), b)
}

// buildAnon proves an anonymous post against st. The message id is chosen
// and recorded under the lock; the slow proving runs outside it.
func (n *Node) buildAnon(req AnonRequest, st *state.State) (*types.Tx, error) {
	n.checkForeign(st)
	n.rlnMu.Lock()
	if n.foreign {
		n.rlnMu.Unlock()
		return nil, ErrForeignUse
	}
	idc, ok := n.commitment()
	if !ok {
		n.rlnMu.Unlock()
		return nil, errors.New("no membership")
	}
	m, found := st.Members.Get(idc)
	if !found || m.Status == state.MemberGone {
		n.rlnMu.Unlock()
		return nil, state.ErrNotMember
	}
	epoch, cap := st.AnonEpochCap()
	t := newAnonTx(req)
	if req.Pseudo && n.pseudo != nil {
		t.PseudoPub = append([]byte(nil), n.pseudo.Pub...)
		ph := t.PseudoHash()
		t.PseudoSig = n.pseudo.Sign(ph[:])
	}
	msgID, err := n.allocMsgID(epoch, min(uint64(m.Limit), cap), t.Signal())
	secret := *n.secret
	n.rlnMu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := st.ProveAnon(t, secret, msgID); err != nil {
		return nil, err
	}
	if err := n.keepBody(t); err != nil {
		return nil, err
	}
	return t, nil
}

// checkAnonContent checks a draft's content (its proof is a placeholder).
func checkAnonContent(t *types.Tx) error {
	if len(t.PseudoPub) > 0 {
		c := *t
		c.PseudoPub, c.PseudoSig = nil, nil
		return c.CheckStateless()
	}
	return t.CheckStateless()
}

// newAnonTx is an anonymous post's content: kept for one month.
func newAnonTx(req AnonRequest) *types.Tx {
	t := &types.Tx{Kind: types.TxAnonPost, Board: req.Board, Parent: req.Parent, Tier: params.TierFree}
	t.SetText([]byte(req.Text))
	return t
}

// leaveAnon sends a due anonymous post. One written while the join was
// still on its way waits for it; other failures are proven again later.
func (n *Node) leaveAnon(req AnonRequest) {
	err := n.sendAnon(req, 0)
	switch {
	case err == nil:
		return
	case errors.Is(err, state.ErrNotMember) && n.joining():
		n.rlnMu.Lock()
		n.outbox = append(n.outbox, &outboxItem{req: req, waitJoin: true, since: time.Now()})
		n.rlnMu.Unlock()
		return
	}
	n.cfg.Logf("node: anonymous post: %v", err)
	if errors.Is(err, errLimit) || errors.Is(err, state.ErrNotMember) || errors.Is(err, ErrForeignUse) {
		return
	}
	n.rlnMu.Lock()
	n.outbox = append(n.outbox, &outboxItem{req: req, sentAt: n.Chain.Height(), tries: 1, since: time.Now()})
	n.rlnMu.Unlock()
}

// sendAnon proves, checks and sends one anonymous post.
// The proof uses the chain tip, whose root every node accepts; the check
// runs on the pending state, which also knows what the pool holds.
func (n *Node) sendAnon(req AnonRequest, tries int) error {
	tip := n.Chain.Tip()
	t, err := n.buildAnon(req, tip)
	if err != nil {
		return err
	}
	if tip.AnonMined(t) {
		return nil // an earlier copy (perhaps with a re-randomised proof) is mined
	}
	n.syncPool()
	pending := n.Pool.PendingState()
	if err := pending.ApplyTx(t, pending.NewCtx()); err != nil && !errors.Is(err, state.ErrDuplicateAnon) {
		return err
	}
	if err := n.Host.SubmitOwn(t); err != nil {
		return err
	}
	n.rlnMu.Lock()
	n.outbox = append(n.outbox, &outboxItem{req: req, id: t.ID(), sentAt: tip.Height, tries: tries, since: time.Now()})
	n.rlnMu.Unlock()
	return nil
}

// tendOutbox forgets mined anonymous posts and proves again the ones that
// did not make it in time.
func (n *Node) tendOutbox(st *state.State) {
	n.rlnMu.Lock()
	var again []*outboxItem
	keep := n.outbox[:0]
	for _, it := range n.outbox {
		switch {
		case it.waitJoin:
			idc, _ := n.commitment()
			if m, ok := st.Members.Get(idc); ok && m.Status != state.MemberGone {
				it.waitJoin = false
				again = append(again, it)
			} else if time.Since(it.since) > joinWait {
				n.cfg.Logf("node: anonymous post dropped: membership join was not mined")
			} else {
				keep = append(keep, it)
			}
		case st.Posts.Has(it.id):
		case n.Pool.Has(it.id) || st.Height < it.sentAt+resendAfter || time.Since(it.since) < n.resendOwnAfter():
			keep = append(keep, it) // pooled, or may still be in the stem
		default:
			if it.tries+1 < maxTries {
				again = append(again, it)
			} else {
				n.cfg.Logf("node: anonymous post %s not included after %d tries", it.id, maxTries)
			}
		}
	}
	n.outbox = keep
	n.rlnMu.Unlock()
	for _, it := range again {
		if err := n.sendAnon(it.req, it.tries+1); err != nil {
			n.cfg.Logf("node: resend anonymous post: %v", err)
			if !errors.Is(err, errLimit) && !errors.Is(err, state.ErrNotMember) && !errors.Is(err, ErrForeignUse) &&
				it.tries+1 < maxTries {
				n.rlnMu.Lock()
				n.outbox = append(n.outbox, &outboxItem{req: it.req, sentAt: st.Height, tries: it.tries + 1, since: time.Now()})
				n.rlnMu.Unlock()
			}
		}
	}
}

// ownAnon lists our anonymous posts not mined yet: queued, waiting for the
// join, or sent. With pooled false, ones already in the pool are left out
// (they show from there).
func (n *Node) ownAnon(pooled bool) []AnonRequest {
	var out []AnonRequest
	n.mu.Lock()
	for _, q := range n.queue {
		if q.anon != nil {
			out = append(out, *q.anon)
		}
	}
	n.mu.Unlock()
	n.rlnMu.Lock()
	items := append([]*outboxItem(nil), n.outbox...)
	n.rlnMu.Unlock()
	for _, it := range items {
		if pooled || it.waitJoin || !n.Pool.Has(it.id) {
			out = append(out, it.req)
		}
	}
	return out
}

// onDoubleSignal slashes a member whose secret two posts revealed.
func (n *Node) onDoubleSignal(ds *state.DoubleSignal) {
	sec, ok := ds.Secret()
	if !ok {
		return
	}
	t := &types.Tx{Kind: types.TxRLNSlash, Secret: sec}
	if err := n.Host.Publish(t); err != nil {
		n.cfg.Logf("node: slash: %v", err)
		return
	}
	n.cfg.Logf("node: slashed a member that exceeded its rate limit (epoch %d)", ds.Epoch)
}
