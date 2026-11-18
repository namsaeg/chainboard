// Package rln implements Rate-Limiting Nullifiers (RLN v2 style) over BN254
// with a Groth16 circuit. A member registers a commitment to a secret a0 and a
// per-epoch message limit; every anonymous post carries a zero-knowledge proof
// that the author is a registered member and that this is message number
// msgID < limit of the epoch. Reusing a msgID in the same epoch for different
// content reveals a0 (two points on the line y = a0 + a1·x), which lets anyone
// slash the member's bond.
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

// A1 = H(3, secret, epoch, scope, msgID): slope of the epoch's share line.
func A1(secret fr.Element, epoch, scope, msgID uint64) fr.Element {
	return H(u64(tagShare), secret, u64(epoch), u64(scope), u64(msgID))
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

// Share returns y = secret + a1·x.
func Share(secret, a1, x fr.Element) fr.Element {
	var y fr.Element
	y.Mul(&a1, &x)
	y.Add(&y, &secret)
	return y
}

// Recover solves two shares of the same line for the secret.
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
