package node

import (
	"github.com/namsaeg/chainboard/internal/cryptox"
)

// Activity is this node's own part in the board: what it wrote (signed,
// under the pseudonym or anonymously), what it liked or pledged to, and
// its crowdfunding, bounties and bounty offers. It is put together here
// from local keys; nothing of it is sent anywhere.
type Activity struct {
	Posts    []PostView   `json:"posts"`
	Pending  []PostView   `json:"pending"`
	Liked    []PostView   `json:"liked"`
	Funds    []FundItem   `json:"funds"`
	Bounties []BountyItem `json:"bounties"`
	Offers   []OfferView  `json:"offers"`
}

// MyActivity gathers this node's activity, newest first, at most limit of
// each kind.
func (n *Node) MyActivity(limit int) Activity {
	me := n.Wallet.Address()
	writers := []cryptox.Address{me}
	if k, ok := n.pseudoKey(); ok {
		writers = append(writers, k.Address())
	}
	if keys, err := n.tipKeys(); err == nil {
		for _, k := range keys {
			writers = append(writers, k.Address())
		}
	}
	a := Activity{Posts: n.Index.ByAuthors(writers, limit), Pending: n.pendingPosts(false), Liked: n.Index.LikedPosts(me, limit),
		Funds: []FundItem{}, Bounties: []BountyItem{}, Offers: []OfferView{}}
	tip := n.Chain.Tip()
	for i := range a.Posts {
		n.fillView(&a.Posts[i], tip)
	}
	for i := range a.Liked {
		n.fillView(&a.Liked[i], tip)
	}
	for _, f := range n.FundList(0) {
		if f.Creator == me.String() && len(a.Funds) < limit {
			a.Funds = append(a.Funds, f)
		}
	}
	for _, b := range n.BountyList(0) {
		if (b.Mine || b.MyOffer) && len(a.Bounties) < limit {
			a.Bounties = append(a.Bounties, b)
		}
	}
	n.offerMu.Lock()
	for id, ot := range n.offers {
		if ot.own {
			a.Offers = append(a.Offers, ot.view(id))
		}
	}
	n.offerMu.Unlock()
	if a.Pending == nil {
		a.Pending = []PostView{}
	}
	if a.Posts == nil {
		a.Posts = []PostView{}
	}
	if a.Liked == nil {
		a.Liked = []PostView{}
	}
	return a
}
