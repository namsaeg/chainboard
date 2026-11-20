package rln

import (
	"testing"
	"time"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
)

func setupMember(t *testing.T) (*Tree, fr.Element, uint32) {
	tr := NewTree()
	var other fr.Element
	other.SetUint64(77)
	tr.Append(Leaf(IdentityCommitment(other), 3))
	var s fr.Element
	s.SetRandom()
	i := tr.Append(Leaf(IdentityCommitment(s), 5))
	return tr, s, i
}

func makeProof(t *testing.T, tr *Tree, s fr.Element, i uint32, limit, msg, cap uint64, sig string) (Public, []byte, error) {
	return makeProofAt(t, tr, s, i, 9, limit, msg, cap, sig)
}

func makeProofAt(t *testing.T, tr *Tree, s fr.Element, i uint32, epoch, limit, msg, cap uint64, sig string) (Public, []byte, error) {
	key := EpochKey(s, epoch, 1)
	a1 := A1(key, msg)
	x := SignalX([]byte(sig))
	pub := Public{Root: tr.Root(), X: x, Y: Share(key, a1, x), Nullifier: Nullifier(a1), Tag: Tag(IdentityCommitment(s), a1),
		Epoch: epoch, Scope: 1, Cap: cap}
	path, bits := tr.Path(i)
	pr, err := Prove(pub, Witness{Secret: s, Limit: limit, MsgID: msg, Path: path, Bits: bits})
	return pub, pr, err
}

func TestProveVerifyRecover(t *testing.T) {
	tr, s, i := setupMember(t)
	t0 := time.Now()
	pub, pr, err := makeProof(t, tr, s, i, 5, 2, 10, "hello")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("prove %v, %d bytes", time.Since(t0), len(pr))
	t0 = time.Now()
	if err := Verify(pr, pub); err != nil {
		t.Fatal(err)
	}
	t.Logf("verify %v", time.Since(t0))

	// Tampering with any public input fails.
	bad := pub
	bad.X = SignalX([]byte("other"))
	if Verify(pr, bad) == nil {
		t.Fatal("tampered signal accepted")
	}
	bad = pub
	bad.Cap = 11
	if Verify(pr, bad) == nil {
		t.Fatal("tampered cap accepted")
	}

	// Same msgID, different signal: the epoch key is recoverable, and with
	// it the member (its tag unmasks to the identity commitment).
	pub2, _, err := makeProof(t, tr, s, i, 5, 2, 10, "spam")
	if err != nil {
		t.Fatal(err)
	}
	if pub2.Nullifier != pub.Nullifier || pub2.Tag != pub.Tag {
		t.Fatal("nullifier and tag should repeat for a reused message id")
	}
	key, ok := Recover(pub.X, pub.Y, pub2.X, pub2.Y)
	if !ok || key != EpochKey(s, 9, 1) || key == s {
		t.Fatal("epoch key not recovered")
	}
	a1, ok := SlopeOf(key, pub.X, pub.Y, pub.Nullifier)
	if !ok || Unmask(pub.Tag, a1) != IdentityCommitment(s) {
		t.Fatal("tag does not unmask to the member")
	}
	var wrong fr.Element
	wrong.SetUint64(3)
	if _, ok := SlopeOf(wrong, pub.X, pub.Y, pub.Nullifier); ok {
		t.Fatal("a wrong key passed")
	}
	// The key opens nothing of another epoch: its tags and nullifiers do
	// not follow from it.
	other, _, err := makeProofAt(t, tr, s, i, 10, 5, 2, 10, "next hour")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := SlopeOf(key, other.X, other.Y, other.Nullifier); ok {
		t.Fatal("one epoch's key opens another epoch")
	}
	// Two messages of one epoch have unrelated tags.
	third, _, err := makeProof(t, tr, s, i, 5, 3, 10, "another")
	if err != nil || third.Tag == pub.Tag || third.Nullifier == pub.Nullifier {
		t.Fatalf("tags of two messages: %v", err)
	}

	// msgID beyond limit or cap cannot be proven.
	if _, _, err := makeProof(t, tr, s, i, 5, 5, 10, "x"); err == nil {
		t.Fatal("msgID >= limit proven")
	}
	if _, _, err := makeProof(t, tr, s, i, 5, 3, 3, "x"); err == nil {
		t.Fatal("msgID >= cap proven")
	}
	// Wrong limit (not the registered one) fails membership.
	if _, _, err := makeProof(t, tr, s, i, 9, 6, 10, "x"); err == nil {
		t.Fatal("inflated limit proven")
	}
}

func TestTreeRemove(t *testing.T) {
	tr, _, i := setupMember(t)
	empty := NewTree()
	r1 := tr.Root()
	tr.Set(i, fr.Element{})
	tr.Set(0, fr.Element{})
	if tr.Root() != empty.Root() || r1 == empty.Root() {
		t.Fatal("zeroed tree root mismatch")
	}
	if tr.nodes() != 0 {
		t.Fatalf("leftover nodes %d", tr.nodes())
	}
}

func TestTreeCloneIsolation(t *testing.T) {
	a := NewTree()
	for i := 0; i < 600; i++ {
		var e fr.Element
		e.SetUint64(uint64(i + 1))
		a.Append(e)
	}
	ra := a.Root()
	b := a.Clone()
	var z fr.Element
	z.SetUint64(999)
	b.Set(5, z)
	b.Append(z)
	if a.Root() != ra || b.Root() == ra {
		t.Fatal("clone write leaked into the original")
	}
	a.Set(7, z)
	if b.Leaf(7) == z {
		t.Fatal("original write leaked into the clone")
	}
	// Rebuilding b from scratch gives the same root: paths are consistent.
	c := NewTree()
	for i := uint32(0); i < b.Next; i++ {
		c.Append(b.Leaf(i))
	}
	if c.Root() != b.Root() {
		t.Fatal("copy-on-write tree root differs from a fresh build")
	}
}

// nodes counts stored (non-zero) nodes.
func (t *Tree) nodes() int {
	n := 0
	t.pages.Range(func(_ uint64, p *page) bool {
		for _, w := range p.present {
			for ; w != 0; w &= w - 1 {
				n++
			}
		}
		return true
	})
	return n
}
