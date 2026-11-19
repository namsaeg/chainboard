package node

import (
	"errors"
	"fmt"
	"time"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/rln"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

// Warnings are enforced here, not in the UI: whatever client talks to the
// node, a surcharged transaction or the last anonymous post of an epoch
// only leaves after the caller confirms it.

// Warning is something the user must see and accept first.
type Warning struct {
	Code    string `json:"code"` // burst, repeat, anon_last
	Message string `json:"message"`
	Factor  uint64 `json:"factor,omitempty"`
}

// WarningError stops a send until it is confirmed.
type WarningError struct {
	Warnings []Warning
	Fee      uint64 // estimated total fee, when known
}

func (e *WarningError) Error() string {
	if len(e.Warnings) == 0 {
		return "confirmation needed"
	}
	return e.Warnings[0].Message
}

// ErrForeignUse: our anonymous key spent message ids we did not record.
var ErrForeignUse = errors.New("다른 기기에서 같은 익명 키를 쓰고 있어요. 두 곳에서 쓰면 한도를 넘겨 보증금을 몰수당할 수 있어요. 한 기기에서만 쓰기로 정한 뒤 확인을 눌러 주세요")

// Warnings lists the surcharges t would pay if it left now.
func (n *Node) Warnings(t *types.Tx) ([]Warning, uint64) {
	probe := *t
	if probe.Kind == types.TxPost && len(probe.Text) > 0 {
		probe.SetText(probe.Text)
		if probe.Tier == 0 {
			probe.Tier = 1
		}
	}
	probe.From = append([]byte(nil), n.Wallet.Pub...)
	st := n.trialState()
	dup, burst := st.FeeFactors(&probe)
	var ws []Warning
	if burst > 1 {
		ws = append(ws, Warning{Code: "burst", Factor: burst,
			Message: fmt.Sprintf("짧은 시간에 트랜잭션을 많이 보내서 기본 수수료가 %d배가 돼요.", burst)})
	}
	if dup > 1 {
		ws = append(ws, Warning{Code: "repeat", Factor: dup,
			Message: fmt.Sprintf("같은 글이 최근에 이미 올라와서 기본 수수료가 %d배가 돼요.", dup)})
	}
	var fee uint64
	if len(ws) > 0 {
		fee, _ = n.Quote(&probe)
	}
	return ws, fee
}

// SendChecked sends t, unless it carries a surcharge not confirmed yet.
func (n *Node) SendChecked(t *types.Tx, confirm bool) error {
	if !confirm {
		if ws, fee := n.Warnings(t); len(ws) > 0 {
			return &WarningError{Warnings: ws, Fee: fee}
		}
	}
	return n.Send(t)
}

// BurstInfo is the wallet's burst score (points) and the factor its next
// transaction would pay.
func (n *Node) BurstInfo() (score float64, free, next uint64) {
	st := n.trialState()
	acc := st.Peek(n.Wallet.Address())
	s := st.BurstScore(&acc, st.Height+1)
	return float64(s) / 1000, st.P.BurstFree, st.BurstMultiplier(s + 1000)
}

// checkForeign looks for our nullifiers on chain that our ledger does not
// know: another device holding the same rln.key is posting. Those ids are
// marked used so this node never reuses them, and anonymous posting stops
// until the user acknowledges.
func (n *Node) checkForeign(st *state.State) {
	n.rlnMu.Lock()
	defer n.rlnMu.Unlock()
	idc, ok := n.commitment()
	if !ok {
		return
	}
	m, found := st.Members.Get(idc)
	if !found || m.Status == state.MemberGone {
		return
	}
	cur := st.RLNEpoch(st.Height + 1)
	scope := st.P.RLNScope()
	found = false
	for _, e := range []uint64{cur - 1, cur} {
		if e > cur { // cur was 0
			continue
		}
		nulls, ok := st.RLNNulls[e]
		if !ok || nulls.Len() == 0 {
			continue
		}
		for id := uint64(0); id < uint64(m.Limit); id++ {
			null := rln.ToBytes(rln.Nullifier(rln.A1(*n.secret, e, scope, id)))
			share, used := nulls.Get(null)
			if !used {
				continue
			}
			mine := n.usage.Epochs[e][uint16(id)]
			if mine != "" {
				// Ours, unless the chain holds a different post for it.
				sig, err := cryptox.ParseHash(mine)
				if err != nil || rln.ToBytes(rln.SignalX(sig[:])) == share.X {
					continue
				}
			}
			if mine == "" {
				if n.usage.Epochs[e] == nil {
					n.usage.Epochs[e] = map[uint16]string{}
				}
				n.usage.Epochs[e][uint16(id)] = "foreign"
			}
			found = true
		}
	}
	if found {
		n.saveUsageLocked()
		if !n.foreign {
			n.cfg.Logf("node: another device is posting with this node's anonymous key")
		}
		n.foreign, n.foreignAt = true, time.Now()
	}
}

// AckForeign lets anonymous posting resume after a foreign-use warning.
// What the chain shows now is taken in first, so only posts the other
// device makes after this raise the warning again.
func (n *Node) AckForeign() {
	n.checkForeign(n.Chain.Tip())
	n.rlnMu.Lock()
	n.foreign = false
	n.rlnMu.Unlock()
}
