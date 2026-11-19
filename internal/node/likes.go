package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/types"
)

// Likes wait in likes.json and leave together as one batch transaction:
// ten minutes after the oldest one, as soon as LikeBatchFull are waiting,
// or when the user sends them. One signature and one transaction header
// cover the whole batch, and each like is an eight-byte-or-so reference
// instead of a 138-byte transaction. A like can be taken back while it waits.
const (
	LikeFlushAfter = 10 * time.Minute
	LikeBatchFull  = 100
)

// QueuedLike is a like waiting for its batch.
type QueuedLike struct {
	ID     string `json:"id"`
	Height uint64 `json:"height"`
	Added  int64  `json:"added"`
}

type likesFile struct {
	Likes []QueuedLike `json:"likes"`
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
	n.likes = f.Likes
	n.likesMu.Unlock()
}

// saveLikesLocked writes likes.json; likesMu is held.
func (n *Node) saveLikesLocked() {
	b, _ := json.Marshal(likesFile{Likes: n.likes})
	if err := writeFileAtomic(n.rlnPath("likes.json"), b); err != nil {
		n.cfg.Logf("node: saving likes: %v", err)
	}
}

// QueueLike adds a like of a mined post to the next batch.
func (n *Node) QueueLike(id cryptox.Hash) (waiting int, err error) {
	v, ok := n.Index.post(id)
	if !ok {
		return 0, errors.New("post not found (likes go to mined posts)")
	}
	if n.Index.Liked(id, n.Wallet.Address()) {
		return 0, errors.New("already liked")
	}
	if h := n.Chain.Height() + 1; h > v.Height+n.Index.LikeWindow {
		return 0, fmt.Errorf("likes count only within %d days of a post", LikeWindowDays)
	}
	n.likesMu.Lock()
	for _, q := range n.likes {
		if q.ID == v.ID {
			n.likesMu.Unlock()
			return 0, errors.New("already waiting to be sent")
		}
	}
	n.likes = append(n.likes, QueuedLike{ID: v.ID, Height: v.Height, Added: time.Now().Unix()})
	n.saveLikesLocked()
	waiting = len(n.likes)
	n.likesMu.Unlock()
	if waiting >= LikeBatchFull {
		if _, err := n.FlushLikes(false); err != nil {
			n.cfg.Logf("node: like batch: %v", err)
		}
	}
	return waiting, nil
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

// QueuedLikes lists the likes waiting for their batch.
func (n *Node) QueuedLikes() []QueuedLike {
	n.likesMu.Lock()
	defer n.likesMu.Unlock()
	return append([]QueuedLike(nil), n.likes...)
}

// FlushLikes sends the waiting likes as one batch (at most types.MaxLikes;
// the rest wait for the next). Likes whose post has left the like window
// are dropped. Without confirm, a batch that would draw a warning stays
// queued and the warning is returned.
func (n *Node) FlushLikes(confirm bool) (int, error) {
	n.likesFlush.Lock()
	defer n.likesFlush.Unlock()
	h := n.Chain.Height() + 1
	n.likesMu.Lock()
	var refs []types.LikeRef
	for _, q := range n.likes {
		id, err := cryptox.ParseHash(q.ID)
		if err == nil && h <= q.Height+n.Index.LikeWindow && len(refs) < types.MaxLikes {
			refs = append(refs, types.RefOf(id, q.Height))
		}
	}
	n.likesMu.Unlock()
	if len(refs) == 0 {
		return 0, nil
	}
	if err := n.SendChecked(&types.Tx{Kind: types.TxLikes, Likes: refs}, confirm); err != nil {
		return 0, err
	}
	sent := map[types.LikeRef]bool{}
	for _, r := range refs {
		sent[r] = true
	}
	n.likesMu.Lock()
	var keep []QueuedLike
	for _, q := range n.likes {
		id, err := cryptox.ParseHash(q.ID)
		if err == nil && !sent[types.RefOf(id, q.Height)] && h <= q.Height+n.Index.LikeWindow {
			keep = append(keep, q)
		}
	}
	n.likes = keep
	n.saveLikesLocked()
	n.likesMu.Unlock()
	return len(refs), nil
}

// tendLikes sends the batch once its oldest like has waited long enough.
func (n *Node) tendLikes(now time.Time) {
	n.likesMu.Lock()
	due := len(n.likes) > 0 && now.Sub(time.Unix(n.likes[0].Added, 0)) >= n.likeFlushAfter()
	n.likesMu.Unlock()
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

func (n *Node) likeFlushAfter() time.Duration {
	if n.cfg.LikeFlushAfter > 0 {
		return n.cfg.LikeFlushAfter
	}
	return LikeFlushAfter
}
