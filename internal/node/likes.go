package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/rln"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

// Likes wait in likes.json until the user sends them, or until the
// auto-send setting (off unless the user turns it on) says so. There are two
// queues:
//
//   - signed: likes, tips (a like that also sends coins to the author) and
//     pledges (a like of a funding request) leave as one TxLikes: one
//     signature, header and base fee for the whole batch;
//   - anonymous: likes leave as one TxAnonLikes of up to rln.LikeBatch,
//     spending one message of the member's epoch (no coins: there is no
//     account to send them from).
//
// Sending small batches often costs more; when to send is the user's call.

// QueuedLike is a like waiting in its queue.
type QueuedLike struct {
	ID     string `json:"id"`               // post or funding request
	Height uint64 `json:"height,omitempty"` // block of the post
	Added  int64  `json:"added"`
	Amount uint64 `json:"amount,omitempty"` // tip or pledge (base units)
	To     string `json:"to,omitempty"`     // tip receiver: the post's author
	Fund   bool   `json:"fund,omitempty"`   // a pledge to a funding request
	Anon   bool   `json:"anon,omitempty"`
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
	Amount  uint64 // tip (post) or pledge (funding request); 0: a plain like
	Fund    bool
	Anon    bool
	Now     bool // send the queue it joins right away
	Confirm bool // accept the send's warnings (with Now)
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

// MinLikeAmount is the smallest tip or pledge the queue takes: the chain's
// minimum with room for the storage price to move before the batch leaves.
func (n *Node) MinLikeAmount() uint64 { return n.trialState().MinPledge() * 5 / 4 }

// likeTag is this member's anonymous tag for a post.
func (n *Node) likeTag(ref types.LikeRef) (types.Hash, error) {
	n.rlnMu.Lock()
	var sec *fr.Element
	if n.secret != nil {
		s := *n.secret
		sec = &s
	}
	n.rlnMu.Unlock()
	if sec == nil {
		return types.Hash{}, errors.New("join the anonymous lane first (bond some mature stake)")
	}
	return rln.ToBytes(rln.LikeTag(*sec, n.P.RLNScope(), ref.Post())), nil
}

// LikedAnon reports whether this member's anonymous like of a post counted.
func (n *Node) LikedAnon(id cryptox.Hash, height uint64) bool {
	tag, err := n.likeTag(types.RefOf(id, height))
	return err == nil && n.Index.LikedBy(id, tag[:])
}

// likeEntry checks a like and turns it into a queue entry.
func (n *Node) likeEntry(req LikeRequest, st *state.State) (QueuedLike, error) {
	q := QueuedLike{ID: req.ID.String(), Added: time.Now().Unix(), Amount: req.Amount, Fund: req.Fund, Anon: req.Anon}
	h := n.Chain.Height() + 1
	if req.Anon && req.Amount > 0 {
		return q, errors.New("anonymous likes carry no coins: there is no account to send them from")
	}
	if req.Fund {
		if req.Anon {
			return q, errors.New("a funding request is backed with coins, so it cannot be liked anonymously")
		}
		if min := st.MinPledge() * 5 / 4; req.Amount < min {
			return q, fmt.Errorf("a pledge is at least %s coins", coins(min))
		}
		if _, err := n.openRequest(req.ID, st, h); err != nil {
			return q, err
		}
		return q, nil
	}
	v, ok := n.Index.post(req.ID)
	if !ok {
		return q, errors.New("post not found (likes go to mined posts)")
	}
	if v.Lane == "fund" {
		return q, errors.New("a funding request is liked with a pledge")
	}
	q.Height = v.Height
	window := func() error {
		if h > v.Height+n.Index.LikeWindow {
			return fmt.Errorf("likes count only within %d days of a post", LikeWindowDays)
		}
		return nil
	}
	switch {
	case req.Anon:
		if n.LikedAnon(req.ID, v.Height) {
			return q, errors.New("already liked anonymously")
		}
		if _, err := n.likeTag(types.RefOf(req.ID, v.Height)); err != nil {
			return q, err
		}
		return q, window()
	case req.Amount == 0:
		if n.Index.Liked(req.ID, n.Wallet.Address()) {
			return q, errors.New("already liked")
		}
		return q, window()
	}
	// A tip: allowed after an earlier like, and after the like window (the
	// coins are counted for the post; the like no longer is).
	if !v.Tippable {
		return q, errors.New("a fully anonymous post has no address to tip")
	}
	if min := st.MinPledge() * 5 / 4; req.Amount < min {
		return q, fmt.Errorf("a tip is at least %s coins", coins(min))
	}
	to, err := cryptox.ParseAddress(v.Author)
	if err != nil {
		return q, err
	}
	if to == n.Wallet.Address() {
		return q, errors.New("you cannot tip your own post")
	}
	q.To = to.String()
	return q, nil
}

// QueueLike adds a like (with any tip or pledge) to its queue, replacing a
// waiting like of the same post, and sends the queue at once if asked. A
// send that fails or needs confirming leaves the queue as it was.
func (n *Node) QueueLike(req LikeRequest) (waiting int, err error) {
	st := n.trialState()
	q, err := n.likeEntry(req, st)
	if err != nil {
		return 0, err
	}
	n.likesMu.Lock()
	before := append([]QueuedLike(nil), n.likes...)
	var coinsOut uint64
	keep := n.likes[:0]
	for _, x := range n.likes {
		if x.ID == q.ID && x.Anon == q.Anon {
			continue
		}
		keep = append(keep, x)
		if !x.Anon {
			coinsOut += x.Amount
		}
	}
	if have := st.Peek(n.Wallet.Address()).Balance; q.Amount > 0 && coinsOut+q.Amount > have {
		n.likes = before
		n.likesMu.Unlock()
		return 0, fmt.Errorf("not enough coins: the waiting likes already send %s of %s", coins(coinsOut), coins(have))
	}
	n.likes = append(keep, q)
	n.saveLikesLocked()
	for _, x := range n.likes {
		if x.Anon == q.Anon {
			waiting++
		}
	}
	n.likesMu.Unlock()
	if !req.Now {
		return waiting, nil
	}
	if _, err := n.FlushLikes(q.Anon, req.Confirm); err != nil {
		n.likesMu.Lock()
		n.likes = before
		n.saveLikesLocked()
		n.likesMu.Unlock()
		return 0, err
	}
	return 0, nil
}

// UnqueueLike takes back a like that has not left yet.
func (n *Node) UnqueueLike(id cryptox.Hash, anon bool) bool {
	n.likesMu.Lock()
	defer n.likesMu.Unlock()
	for i, q := range n.likes {
		if q.ID == id.String() && q.Anon == anon {
			n.likes = append(n.likes[:i], n.likes[i+1:]...)
			n.saveLikesLocked()
			return true
		}
	}
	return false
}

// LikeQueue describes one queue for the UI.
type LikeQueue struct {
	Likes []QueuedLike `json:"likes"`
	Coins uint64       `json:"coins"`         // tips and pledges it sends
	Fee   uint64       `json:"fee"`           // estimated fee of sending it now (signed)
	Next  int          `json:"next"`          // how many the next batch takes
	Soon  int          `json:"expiring_soon"` // likes leaving their window within a day
}

// QueuedLikes lists the waiting likes: signed and anonymous.
func (n *Node) QueuedLikes() (signed, anon LikeQueue) {
	h := n.Chain.Height() + 1
	n.likesMu.Lock()
	all := append([]QueuedLike(nil), n.likes...)
	n.likesMu.Unlock()
	for _, q := range all {
		lq := &signed
		if q.Anon {
			lq = &anon
		}
		lq.Likes = append(lq.Likes, q)
		lq.Coins += q.Amount
		if !q.Fund && q.Amount == 0 && h+n.P.BlocksPerDay > q.Height+n.Index.LikeWindow {
			lq.Soon++
		}
	}
	if tx, _, _ := n.likeBatch(false); tx != nil {
		signed.Next = len(tx.Likes) + len(tx.Tips) + len(tx.Pledges)
		probe := *tx
		probe.From = append([]byte(nil), n.Wallet.Pub...)
		signed.Fee, _ = n.Quote(&probe)
	}
	if tx, _, _ := n.likeBatch(true); tx != nil {
		anon.Next = len(tx.Likes)
	}
	return signed, anon
}

// likeBatch builds the next batch of one queue: the transaction, the
// entries it sends, and the entries that can no longer be sent (window
// passed, funding closed).
func (n *Node) likeBatch(anon bool) (*types.Tx, map[QueuedLike]bool, []QueuedLike) {
	st := n.trialState()
	h := n.Chain.Height() + 1
	n.likesMu.Lock()
	all := append([]QueuedLike(nil), n.likes...)
	n.likesMu.Unlock()
	tx := &types.Tx{Kind: types.TxLikes}
	max := types.MaxLikes
	if anon {
		tx.Kind, max = types.TxAnonLikes, types.MaxAnonLikes
	}
	sent := map[QueuedLike]bool{}
	var dead []QueuedLike
	for _, q := range all {
		if q.Anon != anon {
			continue
		}
		id, err := cryptox.ParseHash(q.ID)
		if err != nil {
			dead = append(dead, q)
			continue
		}
		if len(sent) == max {
			break
		}
		ref := types.RefOf(id, q.Height)
		if q.Amount > 0 && q.Amount < st.MinPledge() {
			dead = append(dead, q) // the minimum rose past it while it waited
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
		case q.Amount > 0:
			to, err := cryptox.ParseAddress(q.To)
			if err != nil {
				dead = append(dead, q)
				continue
			}
			tx.Tips = append(tx.Tips, types.Tip{Ref: ref, To: to, Amount: q.Amount})
		case h > q.Height+n.Index.LikeWindow:
			dead = append(dead, q)
			continue
		default:
			tx.Likes = append(tx.Likes, ref)
		}
		sent[q] = true
	}
	if len(sent) == 0 {
		return nil, nil, dead
	}
	return tx, sent, dead
}

// FlushLikes sends the next batch of one queue now (at most types.MaxLikes
// signed entries or rln.LikeBatch anonymous likes; the rest wait). Entries
// that can no longer be sent are dropped. Without confirm, a batch that
// would draw a warning stays queued and the warning is returned.
func (n *Node) FlushLikes(anon, confirm bool) (int, error) {
	n.likesFlush.Lock()
	defer n.likesFlush.Unlock()
	tx, sent, dead := n.likeBatch(anon)
	if tx != nil {
		var err error
		if anon {
			err = n.AnonPost(AnonRequest{Likes: tx.Likes, Confirm: confirm})
		} else {
			err = n.SendChecked(tx, confirm)
		}
		if err != nil {
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

// tendLikes sends a queue the auto-send setting says is due.
func (n *Node) tendLikes(now time.Time) {
	n.likesMu.Lock()
	auto := n.likeAuto
	var count [2]int
	var oldest [2]int64
	for _, q := range n.likes {
		i := 0
		if q.Anon {
			i = 1
		}
		if count[i] == 0 || q.Added < oldest[i] {
			oldest[i] = q.Added
		}
		count[i]++
	}
	n.likesMu.Unlock()
	for i, anon := range []bool{false, true} {
		due := count[i] > 0 && ((auto.Count > 0 && count[i] >= auto.Count) ||
			(auto.Minutes > 0 && now.Sub(time.Unix(oldest[i], 0)) >= time.Duration(auto.Minutes)*time.Minute))
		if !due {
			continue
		}
		if _, err := n.FlushLikes(anon, false); err != nil {
			var w *WarningError
			if !errors.As(err, &w) || time.Since(n.likeWarnLogged) > time.Hour {
				n.cfg.Logf("node: like batch waits: %v", err)
				n.likeWarnLogged = time.Now()
			}
		}
	}
}
