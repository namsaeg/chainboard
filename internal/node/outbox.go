package node

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"time"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/types"
)

// outbox.json keeps what this node still has to send, so closing the app
// during the random send delay (up to five minutes on testnet) loses nothing:
//
//	txs   signed own transactions, queued or travelling (nonces kept)
//	anon  anonymous posts not mined yet; they are proven again after a
//	      restart (the message-id ledger gives the same signal the same id,
//	      and the anonymous lane refuses repeated text, so no double post)
type outboxFile struct {
	Txs  []outboxTx   `json:"txs"`
	Anon []outboxAnon `json:"anon"`
}

type outboxTx struct {
	Raw    string `json:"raw"`
	SendAt int64  `json:"send_at"`
	Sent   bool   `json:"sent"`
}

type outboxAnon struct {
	Board  string `json:"board"`
	Text   string `json:"text"`
	Parent string `json:"parent,omitempty"`
	Pseudo bool   `json:"pseudo,omitempty"`
}

func (n *Node) outboxSnapshot() []byte {
	var f outboxFile
	n.mu.Lock()
	for _, q := range n.queue {
		if q.tx != nil {
			f.Txs = append(f.Txs, outboxTx{Raw: hex.EncodeToString(q.tx.Encode()), SendAt: q.sendAt.Unix()})
		}
	}
	var sent []*types.Tx
	for _, s := range n.sent {
		sent = append(sent, s.tx)
	}
	n.mu.Unlock()
	sort.Slice(sent, func(i, j int) bool { return sent[i].Nonce < sent[j].Nonce })
	for _, t := range sent {
		f.Txs = append(f.Txs, outboxTx{Raw: hex.EncodeToString(t.Encode()), Sent: true})
	}
	for _, r := range n.QueuedAnon() {
		a := outboxAnon{Board: r.Board, Text: r.Text, Pseudo: r.Pseudo}
		if !r.Parent.IsZero() {
			a.Parent = r.Parent.String()
		}
		f.Anon = append(f.Anon, a)
	}
	b, _ := json.Marshal(f)
	return b
}

// saveOutbox writes outbox.json when it changed.
func (n *Node) saveOutbox() {
	b := n.outboxSnapshot()
	if bytes.Equal(b, n.lastOutbox) {
		return
	}
	if err := writeFileAtomic(n.rlnPath("outbox.json"), b); err != nil {
		n.cfg.Logf("node: saving outbox: %v", err)
		return
	}
	n.lastOutbox = b
}

// loadOutbox restores what an earlier run left unsent. Travelling
// transactions are sent again soon; queued ones keep their send time.
func (n *Node) loadOutbox() {
	b, err := os.ReadFile(n.rlnPath("outbox.json"))
	if err != nil {
		return
	}
	var f outboxFile
	if err := json.Unmarshal(b, &f); err != nil {
		n.cfg.Logf("node: outbox.json unreadable, ignored: %v", err)
		return
	}
	mined := n.Chain.Tip().Peek(n.Wallet.Address()).Nonce
	for _, ot := range f.Txs {
		raw, err := hex.DecodeString(ot.Raw)
		if err != nil {
			continue
		}
		t, err := types.DecodeTx(raw)
		if err != nil || t.Nonce < mined {
			continue
		}
		if ot.Sent {
			// Due for a resend at the first tip after start-up.
			n.sent[t.Nonce] = &sentTx{tx: t, at: time.Time{}}
		} else {
			n.queue = append(n.queue, queued{tx: t, sendAt: time.Unix(ot.SendAt, 0)})
		}
	}
	for _, a := range f.Anon {
		req := AnonRequest{Board: a.Board, Text: a.Text, Pseudo: a.Pseudo}
		if a.Parent != "" {
			h, err := cryptox.ParseHash(a.Parent)
			if err != nil {
				continue
			}
			req.Parent = h
		}
		n.queue = append(n.queue, queued{anon: &req, sendAt: time.Now()})
	}
	sort.SliceStable(n.queue, func(i, j int) bool { return n.queue[i].sendAt.Before(n.queue[j].sendAt) })
	if k := len(n.queue); k > 0 {
		n.nextFree = n.queue[k-1].sendAt
	}
	n.lastOutbox = b
	if c := len(n.queue) + len(n.sent); c > 0 {
		n.cfg.Logf("node: restored %d unsent item(s) from the last run", c)
	}
}
