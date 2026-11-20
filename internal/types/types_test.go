package types

import (
	"testing"

	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/cryptox"
)

func TestTxRoundTripAndSignature(t *testing.T) {
	k := cryptox.KeyFromSeed(make([]byte, 32))
	tx := &Tx{Kind: TxPost, Nonce: 3, Lane: LaneFee, Board: "자유", Tier: 1, Attach: []Hash{{1}}}
	tx.SetText([]byte("글"))
	tx.Sign(k)
	got, err := DecodeTx(tx.Encode())
	if err != nil || got.ID() != tx.ID() {
		t.Fatalf("roundtrip: %v", err)
	}
	if got.Text != nil {
		t.Fatal("the body must not be part of the encoding")
	}
	if err := got.CheckStateless(); err != nil {
		t.Fatal(err)
	}
	// The network form carries the body, checked against the hash.
	w, err := DecodeWire(tx.EncodeWire())
	if err != nil || string(w.Text) != "글" || w.ID() != tx.ID() {
		t.Fatalf("wire roundtrip: %v", err)
	}
	w.Text = []byte("변조")
	if w.CheckStateless() == nil {
		t.Fatal("tampered body accepted")
	}
	noBody := tx.Encode()
	var cw codec.Writer
	cw.Bytes(noBody)
	cw.Bytes(nil)
	if _, err := DecodeWire(cw.B); err == nil {
		t.Fatal("post without its body accepted from the network")
	}
	tx2 := *tx
	tx2.Tier = 0
	tx2.Sign(k)
	if tx2.CheckStateless() == nil {
		t.Fatal("text without a retention tier accepted")
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
	a := &Tx{Kind: TxAnonPost, Board: "익명", Tier: 1, Parent: Hash{3},
		RLN: &RLNProof{Root: Hash{1}, Epoch: 9, Nullifier: Hash{2}, Y: Hash{4}, Proof: make([]byte, 164)}}
	a.SetText([]byte("안녕"))
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
	got.SetText([]byte("변조"))
	if got.CheckStateless() == nil {
		t.Fatal("pseudonym signature survived a text change")
	}
	long := *a
	long.Tier = 12
	if long.CheckStateless() == nil {
		t.Fatal("anonymous post kept longer than a month")
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
	s := &Tx{Kind: TxRLNSlash, Epoch: 77, Target: Hash{9}, Secret: Hash{8}}
	if got, err := DecodeTx(s.Encode()); err != nil || got.Secret != s.Secret || got.Epoch != 77 || got.Target != s.Target || got.CheckStateless() != nil {
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

// A batch of likes (tips) costs far less than its likes sent one by one:
// one header and signature, and short references.
func TestLikeBatchSize(t *testing.T) {
	tip := func(i int) Tip {
		return Tip{Ref: LikeRef{Height: 1_500_000 - uint64(i)*997, Prefix: [4]byte{byte(i)}}, To: Address{byte(i), 1}, Amount: 20_000}
	}
	one := &Tx{Kind: TxLikes, From: make([]byte, 32), Sig: make([]byte, 64), Nonce: 1000, MaxFee: 1 << 24, Tips: []Tip{tip(0)}}
	batch := *one
	batch.Tips = nil
	for i := 0; i < 100; i++ {
		batch.Tips = append(batch.Tips, tip(i))
	}
	if b, o := batch.EncodedSize(), one.EncodedSize(); b*10 > o*100*4 {
		t.Fatalf("100 likes in a batch: %d bytes, one by one: %d", b, 100*o)
	}
	if err := checkLikes(&batch); err != nil {
		t.Fatal(err)
	}
	dup := batch
	dup.Tips = append(append([]Tip(nil), batch.Tips...), batch.Tips[0])
	if checkLikes(&dup) == nil {
		t.Fatal("duplicate like accepted")
	}
	free := batch
	free.Tips = append([]Tip(nil), batch.Tips...)
	free.Tips[3].Amount = 0
	if checkLikes(&free) == nil {
		t.Fatal("a like without coins accepted")
	}
	got, err := DecodeTx(batch.Encode())
	if err != nil || len(got.Tips) != 100 || got.Tips[7] != batch.Tips[7] {
		t.Fatalf("round trip: %v", err)
	}
}

// Bounties and offers survive encoding; an offer's signature covers it.
func TestBountyRoundTrip(t *testing.T) {
	k := cryptox.KeyFromSeed(make([]byte, 32))
	b := &Tx{Kind: TxBounty, Board: "현상금", Note: "2002년 월드컵 길거리 응원 사진", Attach: []Hash{{1}}, Amount: 5, Deadline: 900, Tier: 12, MaxFee: 1 << 30}
	b.Sign(k)
	if got, err := DecodeTx(b.Encode()); err != nil || got.ID() != b.ID() || got.CheckStateless() != nil {
		t.Fatalf("bounty roundtrip: %v", err)
	}
	o := &Offer{Bounty: b.ID(), DataRoot: Hash{2}, Size: 1000, Media: 1, PreviewRoot: Hash{3}, PreviewSize: 200, Note: "광화문"}
	o.Sign(k)
	a := &Tx{Kind: TxBountyAccept, Offer: o, MaxFee: 1 << 30}
	a.Sign(k)
	got, err := DecodeTx(a.Encode())
	if err != nil || got.ID() != a.ID() || got.CheckStateless() != nil || got.Offer.ID() != o.ID() {
		t.Fatalf("accept roundtrip: %v", err)
	}
	o.Size++
	if o.Check() == nil {
		t.Fatal("altered offer accepted")
	}
}
