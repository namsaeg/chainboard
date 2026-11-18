package types

import (
	"testing"

	"github.com/namsaeg/chainboard/internal/cryptox"
)

func TestTxRoundTripAndSignature(t *testing.T) {
	k := cryptox.KeyFromSeed(make([]byte, 32))
	tx := &Tx{Kind: TxPost, Nonce: 3, Lane: LaneFree, Board: "자유", Text: []byte("글"), Attach: []Hash{{1}}}
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
