package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

// A like always carries coins: a tip to a post's author, a pledge to a
// funding request, or a boost of a bounty's pot. Likes wait in likes.json
// until the user sends them (or until the auto-send setting, off unless
// turned on, says so) and leave together as one transaction: one
// signature, header and base fee for the whole batch. Sending small batches
// often costs more; when to send is the user's call.

// QueuedLike is a like waiting in the queue.
type QueuedLike struct {
	ID     string `json:"id"`               // post, funding request or bounty
	Height uint64 `json:"height,omitempty"` // block of the post
	Added  int64  `json:"added"`
	Amount uint64 `json:"amount"`         // base units
	To     string `json:"to,omitempty"`   // tip receiver: where the post's tips go
	Fund   bool   `json:"fund,omitempty"` // a pledge to a funding request
	Bounty bool   `json:"bounty,omitempty"`
}

// LikeAuto is when waiting likes leave by themselves; zero fields are off.
type LikeAuto struct {
	Minutes int `json:"minutes,omitempty"` // once the oldest has waited this long
	Count   int `json:"count,omitempty"`   // as soon as this many wait
}

type likesFile struct {
	Likes []QueuedLike `json:"likes"`
	Auto  LikeAuto     `json:"auto"`
}

// LikeRequest is a like the user gives.
type LikeRequest struct {
	ID      cryptox.Hash
	Amount  uint64 // at least MinLikeAmount
	Fund    bool   // ID is a funding request
	Bounty  bool   // ID is a bounty
	Now     bool   // send the queue right away
	Confirm bool   // accept the send's warnings (with Now)
}

func (n *Node) loadLikes() {
	b, err := os.ReadFile(n.rlnPath("likes.json"))
	if err != nil {
		return
	}
	var f likesFile
	if err := json.Unmarshal(b, &f); err != nil {
		n.cfg.Logf("node: likes.json unreadable, ignored: %v", err)
		return
	}
	n.likesMu.Lock()
	n.likes, n.likeAuto = f.Likes, f.Auto
	n.likesMu.Unlock()
}

// saveLikesLocked writes likes.json; likesMu is held.
func (n *Node) saveLikesLocked() {
	b, _ := json.Marshal(likesFile{Likes: n.likes, Auto: n.likeAuto})
	if err := writeFileAtomic(n.rlnPath("likes.json"), b); err != nil {
		n.cfg.Logf("node: saving likes: %v", err)
	}
}

// LikeAutoSend reads the auto-send setting.
func (n *Node) LikeAutoSend() LikeAuto {
	n.likesMu.Lock()
	defer n.likesMu.Unlock()
	return n.likeAuto
}

// SetLikeAutoSend changes the auto-send setting (zero fields turn it off).
func (n *Node) SetLikeAutoSend(a LikeAuto) error {
	if a.Minutes < 0 || a.Count < 0 || a.Count > types.MaxLikes {
		return fmt.Errorf("auto-send: minutes ≥ 0, count 0 to %d", types.MaxLikes)
	}
	n.likesMu.Lock()
	n.likeAuto = a
	n.saveLikesLocked()
	n.likesMu.Unlock()
	return nil
}

// MinLikeAmount is the smallest like the queue takes: the chain's minimum
// with room for the storage price to move before the batch leaves.
func (n *Node) MinLikeAmount() uint64 { return n.trialState().MinPledge() * 5 / 4 }

// likeEntry checks a like and turns it into a queue entry.
func (n *Node) likeEntry(req LikeRequest, st *state.State) (QueuedLike, error) {
	q := QueuedLike{ID: req.ID.String(), Added: time.Now().Unix(), Amount: req.Amount, Fund: req.Fund, Bounty: req.Bounty}
	h := n.Chain.Height() + 1
	if min := st.MinPledge() * 5 / 4; req.Amount < min {
		return q, fmt.Errorf("a like carries at least %s coins", coins(min))
	}
	switch {
	case req.Fund:
		_, err := n.openRequest(req.ID, st, h)
		return q, err
	case req.Bounty:
		b, ok := st.Bounties.Get(req.ID)
		if !ok || (b.Status != state.BountyOpen && b.Status != state.BountyAccepted) || h >= b.Deadline {
			return q, errors.New("that bounty takes no likes now")
		}
		return q, nil
	}
	v, ok := n.Index.post(req.ID)
	if !ok {
		return q, errors.New("post not found (likes go to mined posts)")
	}
	if v.TipTo == "" {
		return q, errors.New("this post takes no likes (it names no address to receive them)")
	}
	to, err := cryptox.ParseAddress(v.TipTo)
	if err != nil {
		return q, err
	}
	if to == n.Wallet.Address() {
		return q, errors.New("you cannot like your own post")
	}
	q.Height, q.To = v.Height, to.String()
	return q, nil
}

// QueueLike adds a like to the queue, replacing a waiting like of the same
// post, and sends the queue at once if asked. A send that fails or needs
// confirming leaves the queue as it was.
func (n *Node) QueueLike(req LikeRequest) (waiting int, err error) {
	st := n.trialState()
	q, err := n.likeEntry(req, st)
	if err != nil {
		return 0, err
	}
	n.likesMu.Lock()
	before := append([]QueuedLike(nil), n.likes...)
	var out uint64
	keep := n.likes[:0]
	for _, x := range n.likes {
		if x.ID == q.ID {
			continue
		}
		keep = append(keep, x)
		out += x.Amount
	}
	if have := st.Peek(n.Wallet.Address()).Balance; out+q.Amount > have {
		n.likes = before
		n.likesMu.Unlock()
		return 0, fmt.Errorf("not enough coins: the waiting likes already send %s of %s", coins(out), coins(have))
	}
	n.likes = append(keep, q)
	n.saveLikesLocked()
	waiting = len(n.likes)
	n.likesMu.Unlock()
	if !req.Now {
		return waiting, nil
	}
	if _, err := n.FlushLikes(req.Confirm); err != nil {
		n.likesMu.Lock()
		n.likes = before
		n.saveLikesLocked()
		n.likesMu.Unlock()
		return 0, err
	}
	return 0, nil
}

// UnqueueLike takes back a like that has not left yet.
func (n *Node) UnqueueLike(id cryptox.Hash) bool {
	n.likesMu.Lock()
	defer n.likesMu.Unlock()
	for i, q := range n.likes {
		if q.ID == id.String() {
			n.likes = append(n.likes[:i], n.likes[i+1:]...)
			n.saveLikesLocked()
			return true
		}
	}
	return false
}

// LikeQueue describes the queue for the UI.
type LikeQueue struct {
	Likes []QueuedLike `json:"likes"`
	Coins uint64       `json:"coins"`         // what it sends
	Fee   uint64       `json:"fee"`           // estimated fee of sending it now
	Next  int          `json:"next"`          // how many the next batch takes
	Soon  int          `json:"expiring_soon"` // likes leaving their window within a day
}

// QueuedLikes lists the waiting likes.
func (n *Node) QueuedLikes() LikeQueue {
	h := n.Chain.Height() + 1
	n.likesMu.Lock()
	lq := LikeQueue{Likes: append([]QueuedLike(nil), n.likes...)}
	n.likesMu.Unlock()
	for _, q := range lq.Likes {
		lq.Coins += q.Amount
		if q.Height > 0 && h+n.P.BlocksPerDay > q.Height+n.Index.LikeWindow {
			lq.Soon++
		}
	}
	if tx, _, _ := n.likeBatch(); tx != nil {
		lq.Next = len(tx.Tips) + len(tx.Pledges)
		probe := *tx
		probe.From = append([]byte(nil), n.Wallet.Pub...)
		lq.Fee, _ = n.Quote(&probe)
	}
	return lq
}

// likeBatch builds the next batch: the transaction, the entries it sends,
// and the entries that can no longer be sent (funding or bounty closed,
// the minimum risen past them).
func (n *Node) likeBatch() (*types.Tx, map[QueuedLike]bool, []QueuedLike) {
	st := n.trialState()
	h := n.Chain.Height() + 1
	n.likesMu.Lock()
	all := append([]QueuedLike(nil), n.likes...)
	n.likesMu.Unlock()
	tx := &types.Tx{Kind: types.TxLikes}
	sent := map[QueuedLike]bool{}
	var dead []QueuedLike
	for _, q := range all {
		if len(sent) == types.MaxLikes {
			break
		}
		id, err := cryptox.ParseHash(q.ID)
		if err != nil || q.Amount < st.MinPledge() {
			dead = append(dead, q)
			continue
		}
		switch {
		case q.Fund:
			req, err := n.openRequest(id, st, h)
			if err != nil {
				dead = append(dead, q)
				continue
			}
			tx.Pledges = append(tx.Pledges, types.Pledge{Request: id, Amount: q.Amount})
			if req != nil {
				tx.Requests = append(tx.Requests, req)
			}
		case q.Bounty:
			if b, ok := st.Bounties.Get(id); !ok || (b.Status != state.BountyOpen && b.Status != state.BountyAccepted) || h >= b.Deadline {
				dead = append(dead, q)
				continue
			}
			tx.Pledges = append(tx.Pledges, types.Pledge{Request: id, Amount: q.Amount})
		default:
			to, err := cryptox.ParseAddress(q.To)
			if err != nil {
				dead = append(dead, q)
				continue
			}
			tx.Tips = append(tx.Tips, types.Tip{Ref: types.RefOf(id, q.Height), To: to, Amount: q.Amount})
		}
		sent[q] = true
	}
	if len(sent) == 0 {
		return nil, nil, dead
	}
	return tx, sent, dead
}

// FlushLikes sends the next batch now (at most types.MaxLikes; the rest
// wait). Entries that can no longer be sent are dropped. Without confirm,
// a batch that would draw a warning stays queued and the warning is
// returned.
func (n *Node) FlushLikes(confirm bool) (int, error) {
	n.likesFlush.Lock()
	defer n.likesFlush.Unlock()
	tx, sent, dead := n.likeBatch()
	if tx != nil {
		if err := n.SendChecked(tx, confirm); err != nil {
			return 0, err
		}
	}
	drop := map[QueuedLike]bool{}
	for _, q := range dead {
		drop[q] = true
		n.cfg.Logf("node: dropped a like that can no longer be sent (%s)", q.ID)
	}
	n.likesMu.Lock()
	keep := n.likes[:0]
	for _, q := range n.likes {
		if !sent[q] && !drop[q] {
			keep = append(keep, q)
		}
	}
	n.likes = keep
	n.saveLikesLocked()
	n.likesMu.Unlock()
	return len(sent), nil
}

// tendLikes sends the queue when the auto-send setting says it is due.
func (n *Node) tendLikes(now time.Time) {
	n.likesMu.Lock()
	auto := n.likeAuto
	count := len(n.likes)
	var oldest int64
	for i, q := range n.likes {
		if i == 0 || q.Added < oldest {
			oldest = q.Added
		}
	}
	n.likesMu.Unlock()
	due := count > 0 && ((auto.Count > 0 && count >= auto.Count) ||
		(auto.Minutes > 0 && now.Sub(time.Unix(oldest, 0)) >= time.Duration(auto.Minutes)*time.Minute))
	if !due {
		return
	}
	if _, err := n.FlushLikes(false); err != nil {
		var w *WarningError
		if !errors.As(err, &w) || time.Since(n.likeWarnLogged) > time.Hour {
			n.cfg.Logf("node: like batch waits: %v", err)
			n.likeWarnLogged = time.Now()
		}
	}
}
