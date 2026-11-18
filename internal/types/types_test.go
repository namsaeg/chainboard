package types

import (
	"testing"

	"github.com/namsaeg/chainboard/internal/cryptox"
)

func TestTxRoundTripAndSignature(t *testing.T) {
	k := cryptox.KeyFromSeed(make([]byte, 32))
	tx := &Tx{Kind: TxPost, Nonce: 3, Lane: LaneFee, Board: "자유", Text: []byte("글"), Attach: []Hash{{1}}}
	tx.Sign(k)
	got, err := DecodeTx(tx.Encode())
	if err != nil || got.ID() != tx.ID() {
		t.Fatalf("roundtrip: %v", err)
	}
	if err := got.CheckStateless(); err != nil {
		t.Fatal(err)
	}
	got.Text = []byte("변조")
	if got.CheckStateless() == nil {
		t.Fatal("tampered tx accepted")
	}
	if _, err := DecodeTx(append(tx.Encode(), 0)); err == nil {
		t.Fatal("trailing byte accepted")
	}
	b := &Block{Txs: []*Tx{tx}}
	b.Height = 7
	b.VDFOutputs = []Hash{{9}}
	b.Sol.PackedSub = make([]byte, 8192)
	b.Miner = k.Pub
	b.Sig = make([]byte, 64)
	bb, err := DecodeBlock(b.Encode())
	if err != nil || bb.Hash() != b.Hash() || len(bb.Txs) != 1 {
		t.Fatalf("block roundtrip: %v", err)
	}
}

func TestUnsignedKinds(t *testing.T) {
	pseudo := cryptox.KeyFromSeed(append(make([]byte, 31), 5))
	a := &Tx{Kind: TxAnonPost, Board: "익명", Text: []byte("안녕"), Parent: Hash{3},
		RLN: &RLNProof{Root: Hash{1}, Epoch: 9, Nullifier: Hash{2}, Y: Hash{4}, Proof: make([]byte, 164)}}
	plainSignal := a.Signal()
	a.PseudoPub = pseudo.Pub
	h := a.PseudoHash()
	a.PseudoSig = pseudo.Sign(h[:])
	if a.Signal() == plainSignal {
		t.Fatal("pseudonym key must be part of the signal")
	}
	got, err := DecodeTx(a.Encode())
	if err != nil || got.ID() != a.ID() || got.Author() != pseudo.Address() {
		t.Fatalf("anon roundtrip: %v", err)
	}
	if err := got.CheckStateless(); err != nil {
		t.Fatal(err)
	}
	got.Text = []byte("변조")
	if got.CheckStateless() == nil {
		t.Fatal("pseudonym signature survived a text change")
	}
	a.PseudoPub, a.PseudoSig = nil, nil
	got, err = DecodeTx(a.Encode())
	if err != nil || got.Author() != (Address{}) || got.Sender() != (Address{}) || got.Signal() != plainSignal {
		t.Fatalf("plain anon roundtrip: %v", err)
	}
	a.Attach = []Hash{{7}}
	if a.CheckStateless() == nil {
		t.Fatal("anonymous attachment accepted")
	}
	s := &Tx{Kind: TxRLNSlash, Secret: Hash{8}}
	if got, err := DecodeTx(s.Encode()); err != nil || got.Secret != s.Secret || len(s.Encode()) != 33 {
		t.Fatalf("slash roundtrip: %v", err)
	}
	k := cryptox.KeyFromSeed(make([]byte, 32))
	j := &Tx{Kind: TxRLNJoin, Amount: 5, Commitment: Hash{6}, Limit: 5}
	j.Sign(k)
	if got, err := DecodeTx(j.Encode()); err != nil || got.ID() != j.ID() || got.CheckStateless() != nil {
		t.Fatalf("join roundtrip: %v", err)
	}
	j.Lane = LaneAnon
	j.Sign(k)
	if j.CheckStateless() == nil {
		t.Fatal("signed tx on the anonymous lane accepted")
	}
}
