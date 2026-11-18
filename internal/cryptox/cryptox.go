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
	"fmt"
	"os"
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

// LoadOrCreateKey reads a hex seed from path, creating it (mode 0600) if absent.
func LoadOrCreateKey(path string) (*Key, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("key file %s is corrupt", path)
		}
		return KeyFromSeed(seed), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k, err := NewKey()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(k.Priv.Seed())+"\n"), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

// Balloon is Balloon hashing (Boneh, Corrigan-Gibbs, Schechter 2016) over
// SHA-256: a sequential memory-hard function. space is the buffer size in
// 32-byte blocks, rounds the number of mixing rounds. It is used to derive
// packing keys so that a miner cannot cheaply re-create a packed replica on
// demand instead of storing it.
func Balloon(input, salt []byte, space, rounds int) Hash {
	const delta = 3
	if space < 2 {
		space = 2
	}
	buf := make([]Hash, space)
	var cnt uint64
	h := func(parts ...[]byte) Hash {
		cnt++
		hh := sha256.New()
		hh.Write(U64(cnt))
		for _, p := range parts {
			hh.Write(p)
		}
		var o Hash
		hh.Sum(o[:0])
		return o
	}
	buf[0] = h(input, salt)
	for m := 1; m < space; m++ {
		buf[m] = h(buf[m-1][:])
	}
	for t := 0; t < rounds; t++ {
		for m := 0; m < space; m++ {
			prev := buf[(m-1+space)%space]
			buf[m] = h(prev[:], buf[m][:])
			for i := 0; i < delta; i++ {
				idx := sha256.Sum256(append(append(U64(uint64(t)), U64(uint64(m))...), U64(uint64(i))...))
				o := h(salt, idx[:])
				other := binary.BigEndian.Uint64(o[:8]) % uint64(space)
				buf[m] = h(buf[m][:], buf[other][:])
			}
		}
	}
	return buf[space-1]
}
