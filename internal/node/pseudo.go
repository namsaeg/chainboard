package node

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/keystore"
	"github.com/namsaeg/chainboard/internal/types"
)

// Tips to anonymous writing. A pseudonym's posts are tipped at the
// pseudonym key's address; every other anonymous post names a one-time
// address of its own (keys in tipkeys.key, locked like the wallet). This
// node can spend from them. All of these accounts are public, so moving
// their coins to the wallet, or several of them to one place, links them
// for anyone reading the chain.

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

type tipKeysFile struct {
	Seeds []string `json:"seeds"`
}

// tipKeys gives the one-time keys of this node's anonymous posts. The file
// is read once (opening the keystore is slow on purpose) and kept here.
func (n *Node) tipKeys() ([]*cryptox.Key, error) {
	n.tipMu.Lock()
	defer n.tipMu.Unlock()
	return n.tipKeysLocked()
}

func (n *Node) tipKeysLocked() ([]*cryptox.Key, error) {
	if n.tipLoaded {
		return n.tipCache, nil
	}
	b, err := keystore.Read(n.rlnPath("tipkeys.key"), n.cfg.Password)
	if errors.Is(err, os.ErrNotExist) {
		n.tipLoaded = true
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f tipKeysFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("tipkeys.key: %w", err)
	}
	var out []*cryptox.Key
	for _, h := range f.Seeds {
		seed, err := hex.DecodeString(h)
		if err != nil || len(seed) != 32 {
			continue
		}
		out = append(out, cryptox.KeyFromSeed(seed))
	}
	n.tipCache, n.tipLoaded = out, true
	return out, nil
}

// newTipKey makes and keeps a one-time key, before its address is used.
func (n *Node) newTipKey() (types.Address, error) {
	n.tipMu.Lock()
	defer n.tipMu.Unlock()
	keys, err := n.tipKeysLocked()
	if err != nil {
		return types.Address{}, err
	}
	var f tipKeysFile
	for _, k := range keys {
		f.Seeds = append(f.Seeds, hex.EncodeToString(k.Seed()))
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return types.Address{}, err
	}
	f.Seeds = append(f.Seeds, hex.EncodeToString(seed))
	b, _ := json.Marshal(f)
	if err := keystore.Write(n.rlnPath("tipkeys.key"), b, n.cfg.Password); err != nil {
		return types.Address{}, err
	}
	k := cryptox.KeyFromSeed(seed)
	n.tipCache = append(keys[:len(keys):len(keys)], k)
	return k.Address(), nil
}

// TipAccount is an address of this node's anonymous writing and its coins.
type TipAccount struct {
	Address string `json:"address"`
	Balance uint64 `json:"balance"`
	Pseudo  bool   `json:"pseudonym,omitempty"`
}

// TipAccounts lists the pseudonym's and the anonymous posts' addresses
// that hold coins (the pseudonym's always, when there is one).
func (n *Node) TipAccounts() ([]TipAccount, error) {
	st := n.trialState()
	var out []TipAccount
	if k, ok := n.pseudoKey(); ok {
		out = append(out, TipAccount{Address: k.Address().String(), Balance: st.Peek(k.Address()).Balance, Pseudo: true})
	}
	keys, err := n.tipKeys()
	if err != nil {
		return out, err
	}
	for _, k := range keys {
		if b := st.Peek(k.Address()).Balance; b > 0 {
			out = append(out, TipAccount{Address: k.Address().String(), Balance: b})
		}
	}
	return out, nil
}

// spendKey finds the key of one of this node's tip addresses.
func (n *Node) spendKey(from cryptox.Address) (*cryptox.Key, error) {
	if k, ok := n.pseudoKey(); ok && k.Address() == from {
		return k, nil
	}
	keys, err := n.tipKeys()
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if k.Address() == from {
			return k, nil
		}
	}
	return nil, errors.New("not an address of this node's anonymous writing")
}

// SendFromTips moves coins out of one of these addresses (the fee comes out
// of it too). Sending them to this node's wallet needs confirming: it
// links the anonymous writing to the wallet for anyone reading the chain.
func (n *Node) SendFromTips(from, to cryptox.Address, amount uint64, confirm bool) error {
	k, err := n.spendKey(from)
	if err != nil {
		return err
	}
	if to.IsZero() || to == from || amount == 0 {
		return errors.New("a receiver other than the address itself and an amount are needed")
	}
	if to == n.Wallet.Address() && !confirm {
		return &WarningError{Warnings: []Warning{{Code: "tip_link",
			Message: "익명으로 받은 후원을 이 노드의 지갑으로 옮기면 체인에 기록이 남아, 누구나 그 글(또는 가명)과 지갑이 같은 사람이라는 걸 알 수 있어요."}}}
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
		if strings.Contains(err.Error(), "insufficient balance") {
			return fmt.Errorf("%w (the fee is paid from the same address)", err)
		}
		return err
	}
	return n.Host.SubmitOwn(t)
}
