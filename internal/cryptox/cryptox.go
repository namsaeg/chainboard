// Package cryptox holds hashing, keys, addresses and the memory-hard function
// used for packing. Only the Go standard library is used.
//
// Signatures are Ed25519 (RFC 8032): deterministic nonces (no nonce-reuse
// failure mode), constant-time implementation in the standard library, small
// keys and signatures, fast verification.
package cryptox

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
)

// Hash is a SHA-256 digest.
type Hash [32]byte

func (h Hash) String() string { return hex.EncodeToString(h[:]) }
func (h Hash) IsZero() bool   { return h == Hash{} }

// ParseHash parses a hex hash.
func ParseHash(s string) (Hash, error) {
	var h Hash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return h, errors.New("bad hash")
	}
	copy(h[:], b)
	return h, nil
}

// Tagged hashes every part under a domain tag so that hashes made for
// different purposes can never collide.
func Tagged(tag string, parts ...[]byte) Hash {
	h := sha256.New()
	var l [8]byte
	binary.BigEndian.PutUint64(l[:], uint64(len(tag)))
	h.Write(l[:])
	h.Write([]byte(tag))
	for _, p := range parts {
		binary.BigEndian.PutUint64(l[:], uint64(len(p)))
		h.Write(l[:])
		h.Write(p)
	}
	var out Hash
	h.Sum(out[:0])
	return out
}

// U64 encodes v big-endian, for hashing.
func U64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

// Address identifies an account: the first 20 bytes of a tagged hash of the
// Ed25519 public key.
type Address [20]byte

const addrPrefix = "cb1"

func (a Address) String() string { return addrPrefix + hex.EncodeToString(a[:]) }
func (a Address) IsZero() bool   { return a == Address{} }

// ParseAddress parses "cb1" + 40 hex characters.
func ParseAddress(s string) (Address, error) {
	var a Address
	if !strings.HasPrefix(s, addrPrefix) {
		return a, errors.New("address must start with cb1")
	}
	b, err := hex.DecodeString(s[len(addrPrefix):])
	if err != nil || len(b) != 20 {
		return a, errors.New("bad address")
	}
	copy(a[:], b)
	return a, nil
}

// AddressOf derives the address of a public key.
func AddressOf(pub ed25519.PublicKey) Address {
	h := Tagged("cb/addr", pub)
	var a Address
	copy(a[:], h[:20])
	return a
}

// Key is an Ed25519 key pair.
type Key struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// NewKey generates a key.
func NewKey() (*Key, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Key{Priv: priv, Pub: pub}, nil
}

// KeyFromSeed builds a key from a 32-byte seed (tests, deterministic devnets).
func KeyFromSeed(seed []byte) *Key {
	priv := ed25519.NewKeyFromSeed(seed)
	return &Key{Priv: priv, Pub: priv.Public().(ed25519.PublicKey)}
}

func (k *Key) Address() Address       { return AddressOf(k.Pub) }
func (k *Key) Sign(msg []byte) []byte { return ed25519.Sign(k.Priv, msg) }

// Verify checks an Ed25519 signature.
func Verify(pub, msg, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), msg, sig)
}
