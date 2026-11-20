// Package rln implements Rate-Limiting Nullifiers (RLN v2 style) over BN254
// with a Groth16 circuit. A member registers a commitment to a secret a0 and a
// per-epoch message limit; every anonymous post carries a zero-knowledge proof
// that the author is a registered member and that this is message number
// msgID < limit of the epoch.
//
// The share line of an epoch is y = k + a1·x with the epoch key
// k = H(6, a0, epoch, scope), a1 = H(3, k, msgID). Reusing a msgID in the
// same epoch for different content reveals k, not a0: only that epoch's
// messages become linkable. Each message also carries a tag
// T = idc + H(7, a1), the identity commitment masked per message; with k
// anyone can unmask it and slash the member, while tags of other epochs
// stay unreadable.
package rln

import (
	"crypto/sha256"
	"math/big"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr/mimc"
)

// Domain tags prepended to the MiMC inputs so that no two hash uses can
// collide structurally (Merkle nodes are the only untagged 2-input use).
const (
	tagIdentity  = 1
	tagLeaf      = 2
	tagShare     = 3
	tagNullifier = 4
	tagEpoch     = 6
	tagMask      = 7
)

// Element is a BN254 scalar field element in canonical big-endian form.
type Element = [32]byte

// H hashes field elements with MiMC-BN254 (Miyaguchi-Preneel).
func H(xs ...fr.Element) fr.Element {
	h := mimc.NewMiMC()
	for i := range xs {
		b := xs[i].Bytes()
		h.Write(b[:])
	}
	var out fr.Element
	out.SetBytes(h.Sum(nil))
	return out
}

func u64(v uint64) fr.Element { var e fr.Element; e.SetUint64(v); return e }

// IdentityCommitment = H(1, secret).
func IdentityCommitment(secret fr.Element) fr.Element { return H(u64(tagIdentity), secret) }

// Leaf = H(2, identityCommitment, limit) — what goes into the membership tree.
func Leaf(idc fr.Element, limit uint64) fr.Element { return H(u64(tagLeaf), idc, u64(limit)) }

// EpochKey = H(6, secret, epoch, scope): the intercept of the epoch's share
// lines, which a double signal reveals.
func EpochKey(secret fr.Element, epoch, scope uint64) fr.Element {
	return H(u64(tagEpoch), secret, u64(epoch), u64(scope))
}

// A1 = H(3, epochKey, msgID): slope of a message's share line.
func A1(key fr.Element, msgID uint64) fr.Element { return H(u64(tagShare), key, u64(msgID)) }

// MessageA1 is A1 straight from the member secret.
func MessageA1(secret fr.Element, epoch, scope, msgID uint64) fr.Element {
	return A1(EpochKey(secret, epoch, scope), msgID)
}

// Tag = idc + H(7, a1): the identity commitment masked for one message.
func Tag(idc, a1 fr.Element) fr.Element {
	m := H(u64(tagMask), a1)
	m.Add(&m, &idc)
	return m
}

// Unmask recovers the identity commitment from a tag and its message's a1.
func Unmask(tag, a1 fr.Element) fr.Element {
	m := H(u64(tagMask), a1)
	var idc fr.Element
	idc.Sub(&tag, &m)
	return idc
}

// SlopeOf checks an epoch key against a recorded share and its nullifier
// and returns the message's a1: a1 = (y − k)/x, and H(4, a1) must be the
// nullifier.
func SlopeOf(key, x, y, nullifier fr.Element) (fr.Element, bool) {
	var a1 fr.Element
	if x.IsZero() {
		return a1, false
	}
	a1.Sub(&y, &key)
	var inv fr.Element
	inv.Inverse(&x)
	a1.Mul(&a1, &inv)
	n := Nullifier(a1)
	return a1, n.Equal(&nullifier)
}

// Nullifier = H(4, a1). Equal for equal (secret, epoch, msgID).
func Nullifier(a1 fr.Element) fr.Element { return H(u64(tagNullifier), a1) }

// Node hashes two Merkle children.
func Node(l, r fr.Element) fr.Element { return H(l, r) }

// SignalX maps arbitrary signal bytes to the x coordinate of the share.
func SignalX(signal []byte) fr.Element {
	d := sha256.Sum256(append([]byte("cb/rln/x"), signal...))
	var e fr.Element
	e.SetBigInt(new(big.Int).SetBytes(d[:]))
	return e
}

// Share returns y = key + a1·x.
func Share(key, a1, x fr.Element) fr.Element {
	var y fr.Element
	y.Mul(&a1, &x)
	y.Add(&y, &key)
	return y
}

// Recover solves two shares of the same line for its intercept: the epoch
// key.
func Recover(x1, y1, x2, y2 fr.Element) (fr.Element, bool) {
	var dx, dy, a1, s fr.Element
	dx.Sub(&x2, &x1)
	if dx.IsZero() {
		return s, false
	}
	dy.Sub(&y2, &y1)
	dx.Inverse(&dx)
	a1.Mul(&dy, &dx)
	s.Mul(&a1, &x1)
	s.Sub(&y1, &s)
	return s, true
}

// FromBytes decodes a canonical element; ok=false if b >= modulus.
func FromBytes(b Element) (fr.Element, bool) {
	var e fr.Element
	if err := e.SetBytesCanonical(b[:]); err != nil {
		return e, false
	}
	return e, true
}

// ToBytes encodes an element canonically.
func ToBytes(e fr.Element) Element { return e.Bytes() }
