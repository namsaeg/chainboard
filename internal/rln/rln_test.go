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
	a1 := A1(s, 9, 1, msg)
	x := SignalX([]byte(sig))
	pub := Public{Root: tr.Root(), X: x, Y: Share(s, a1, x), Nullifier: Nullifier(a1), Epoch: 9, Scope: 1, Cap: cap}
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

	// Same msgID, different signal: secret is recoverable.
	pub2, _, err := makeProof(t, tr, s, i, 5, 2, 10, "spam")
	if err != nil {
		t.Fatal(err)
	}
	if pub2.Nullifier != pub.Nullifier {
		t.Fatal("nullifier should repeat")
	}
	got, ok := Recover(pub.X, pub.Y, pub2.X, pub2.Y)
	if !ok || got != s {
		t.Fatal("secret not recovered")
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
	if len(tr.nodes) != 0 {
		t.Fatalf("leftover nodes %d", len(tr.nodes))
	}
}
