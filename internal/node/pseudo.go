package node

import (
	"errors"
	"os"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/types"
)

// A pseudonym's posts can be tipped: the tips go to the pseudonym key's
// address, which this node can spend from. The pseudonym's account is
// public like any other, so moving its coins to the wallet (or anywhere the
// wallet also pays) links the two on chain.

// pseudoKey loads the pseudonym key if one was ever made.
func (n *Node) pseudoKey() (*cryptox.Key, bool) {
	n.rlnMu.Lock()
	defer n.rlnMu.Unlock()
	if n.pseudo != nil {
		return n.pseudo, true
	}
	path := n.rlnPath("pseudonym.key")
	if _, err := os.Stat(path); err != nil {
		return nil, false
	}
	k, err := loadKey(path, n.cfg.Password)
	if err != nil {
		return nil, false
	}
	n.pseudo = k
	return k, true
}

// PseudoBalance is what the pseudonym's address holds.
func (n *Node) PseudoBalance() (cryptox.Address, uint64, bool) {
	k, ok := n.pseudoKey()
	if !ok {
		return cryptox.Address{}, 0, false
	}
	return k.Address(), n.trialState().Peek(k.Address()).Balance, true
}

// SendPseudo moves coins out of the pseudonym's address (the fee comes out
// of it too). Sending them to this node's wallet needs confirming: it links
// the pseudonym to the wallet for anyone reading the chain.
func (n *Node) SendPseudo(to cryptox.Address, amount uint64, confirm bool) error {
	k, ok := n.pseudoKey()
	if !ok {
		return errors.New("no pseudonym yet")
	}
	if to.IsZero() || to == k.Address() || amount == 0 {
		return errors.New("a receiver other than the pseudonym and an amount are needed")
	}
	if to == n.Wallet.Address() && !confirm {
		return &WarningError{Warnings: []Warning{{Code: "pseudo_link",
			Message: "가명이 받은 코인을 이 노드의 지갑으로 옮기면 체인에 기록이 남아, 누구나 가명과 지갑이 같은 사람이라는 걸 알 수 있어요."}}}
	}
	n.sendMu.Lock()
	defer n.sendMu.Unlock()
	t := &types.Tx{Kind: types.TxTransfer, To: to, Amount: amount, Lane: types.LaneFee, MaxFee: 1}
	t.Sign(k)
	est, err := n.quoteAs(t, k)
	if err != nil {
		return err
	}
	st := n.Pool.PendingState()
	t.Nonce = st.Peek(k.Address()).Nonce
	t.MaxFee = est*2 + max(st.BaseFee, st.PermByteFee())*64
	t.Sign(k)
	trial := n.trialState()
	if err := trial.ApplyTx(t, trial.NewCtx()); err != nil {
		return err
	}
	return n.Host.SubmitOwn(t)
}
