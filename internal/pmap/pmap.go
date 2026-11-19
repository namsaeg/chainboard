// Package pmap provides persistent collections: copying one is O(1), and a
// write copies only the O(log n) path it changes, so many versions share
// nearly all their storage. The chain keeps a state per recent block and the
// mempool, miner and API each take their own copy; with plain maps every
// copy cost O(size of the state).
//
// Values are stored as given: callers that store pointers must not change
// what they point to after storing (replace the entry instead).
package pmap

import (
	"encoding/binary"
	"math/bits"
)

const (
	fanBits = 5
	fan     = 1 << fanBits
	mask    = fan - 1
)

type leaf[K comparable, V any] struct {
	h uint64
	k K
	v V
}

// node is a bitmap-compressed trie node. Below the last hash level it
// holds colliding leaves in a plain list (bitmap unused).
type node[K comparable, V any] struct {
	bitmap uint32
	kids   []*node[K, V] // for set bits: child node, or nil when leafs[i] is used
	leafs  []*leaf[K, V]
}

// Map is a persistent hash map. The zero value is not usable; call New.
type Map[K comparable, V any] struct {
	root *node[K, V]
	n    int
	hash func(K) uint64
}

// New makes an empty map with the given hash. It must be deterministic:
// iteration order follows it.
func New[K comparable, V any](hash func(K) uint64) Map[K, V] {
	return Map[K, V]{root: &node[K, V]{}, hash: hash}
}

// Len is the number of entries.
func (m Map[K, V]) Len() int { return m.n }

// Get looks a key up.
func (m Map[K, V]) Get(k K) (V, bool) {
	h := m.hash(k)
	n := m.root
	for shift := uint(0); n != nil; shift += fanBits {
		if shift >= 64 {
			for _, l := range n.leafs {
				if l.k == k {
					return l.v, true
				}
			}
			break
		}
		bit := uint32(1) << ((h >> shift) & mask)
		if n.bitmap&bit == 0 {
			break
		}
		i := bits.OnesCount32(n.bitmap & (bit - 1))
		if c := n.kids[i]; c != nil {
			n = c
			continue
		}
		if l := n.leafs[i]; l.k == k {
			return l.v, true
		}
		break
	}
	var zero V
	return zero, false
}

// Has reports whether a key is present.
func (m Map[K, V]) Has(k K) bool {
	_, ok := m.Get(k)
	return ok
}

// Set stores a value, copying the path to it. Versions copied before are
// not affected.
func (m *Map[K, V]) Set(k K, v V) {
	root, added := m.root.set(&leaf[K, V]{h: m.hash(k), k: k, v: v}, 0)
	m.root = root
	if added {
		m.n++
	}
}

// Delete removes a key, copying the path to it.
func (m *Map[K, V]) Delete(k K) {
	root, removed := m.root.del(m.hash(k), k, 0)
	if !removed {
		return
	}
	if root == nil {
		root = &node[K, V]{}
	}
	m.root = root
	m.n--
}

// Range calls f for every entry, in hash order, until f returns false.
func (m Map[K, V]) Range(f func(K, V) bool) {
	m.root.each(f)
}

func (n *node[K, V]) each(f func(K, V) bool) bool {
	for i, l := range n.leafs {
		if c := n.kids[i]; c != nil {
			if !c.each(f) {
				return false
			}
		} else if !f(l.k, l.v) {
			return false
		}
	}
	return true
}

func (n *node[K, V]) clone(extra int) *node[K, V] {
	c := &node[K, V]{bitmap: n.bitmap,
		kids: make([]*node[K, V], len(n.kids), len(n.kids)+extra), leafs: make([]*leaf[K, V], len(n.leafs), len(n.leafs)+extra)}
	copy(c.kids, n.kids)
	copy(c.leafs, n.leafs)
	return c
}

func (n *node[K, V]) set(l *leaf[K, V], shift uint) (*node[K, V], bool) {
	if shift >= 64 { // collision list
		for i, x := range n.leafs {
			if x.k == l.k {
				c := n.clone(0)
				c.leafs[i] = l
				return c, false
			}
		}
		c := n.clone(1)
		c.kids = append(c.kids, nil)
		c.leafs = append(c.leafs, l)
		return c, true
	}
	bit := uint32(1) << ((l.h >> shift) & mask)
	i := bits.OnesCount32(n.bitmap & (bit - 1))
	if n.bitmap&bit == 0 {
		c := n.clone(1)
		c.bitmap |= bit
		c.kids = append(c.kids[:i], append([]*node[K, V]{nil}, c.kids[i:]...)...)
		c.leafs = append(c.leafs[:i], append([]*leaf[K, V]{l}, c.leafs[i:]...)...)
		return c, true
	}
	c := n.clone(0)
	if kid := n.kids[i]; kid != nil {
		nk, added := kid.set(l, shift+fanBits)
		c.kids[i] = nk
		return c, added
	}
	old := n.leafs[i]
	if old.k == l.k {
		c.leafs[i] = l
		return c, false
	}
	// Two leaves share this slot: push both one level down.
	sub := &node[K, V]{}
	sub, _ = sub.set(old, shift+fanBits)
	sub, _ = sub.set(l, shift+fanBits)
	c.kids[i], c.leafs[i] = sub, nil
	return c, true
}

func (n *node[K, V]) del(h uint64, k K, shift uint) (*node[K, V], bool) {
	if shift >= 64 {
		for i, x := range n.leafs {
			if x.k == k {
				if len(n.leafs) == 1 {
					return nil, true
				}
				c := n.clone(0)
				c.kids = append(c.kids[:i], c.kids[i+1:]...)
				c.leafs = append(c.leafs[:i], c.leafs[i+1:]...)
				return c, true
			}
		}
		return n, false
	}
	bit := uint32(1) << ((h >> shift) & mask)
	if n.bitmap&bit == 0 {
		return n, false
	}
	i := bits.OnesCount32(n.bitmap & (bit - 1))
	if kid := n.kids[i]; kid != nil {
		nk, removed := kid.del(h, k, shift+fanBits)
		if !removed {
			return n, false
		}
		c := n.clone(0)
		switch {
		case nk == nil:
			return c.drop(i, bit), true
		case len(nk.leafs) == 1 && nk.kids[0] == nil:
			c.kids[i], c.leafs[i] = nil, nk.leafs[0] // pull a lone leaf up
		default:
			c.kids[i] = nk
		}
		return c, true
	}
	if n.leafs[i].k != k {
		return n, false
	}
	return n.clone(0).drop(i, bit), true
}

// drop removes slot i of a node this call owns; nil if it empties.
func (c *node[K, V]) drop(i int, bit uint32) *node[K, V] {
	c.bitmap &^= bit
	c.kids = append(c.kids[:i], c.kids[i+1:]...)
	c.leafs = append(c.leafs[:i], c.leafs[i+1:]...)
	if len(c.leafs) == 0 {
		return nil
	}
	return c
}

// Hash functions for the key types the state uses. Hashes and addresses are
// already uniform, so their first bytes serve; integers are mixed.

// Hash32 hashes a 32-byte hash key.
func Hash32[K ~[32]byte](k K) uint64 { return binary.LittleEndian.Uint64(k[:8]) }

// Hash20 hashes an address key.
func Hash20[K ~[20]byte](k K) uint64 { return binary.LittleEndian.Uint64(k[:8]) }

// Hash52 hashes an address||hash key by its hash part, so one address's
// keys spread out.
func Hash52[K ~[52]byte](k K) uint64 { return binary.LittleEndian.Uint64(k[20:28]) }

// Uint64 hashes an integer key (splitmix64 finaliser).
func Uint64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ x>>31
}

// Vec is a persistent append-mostly vector: copying is O(1), Append and Set
// copy one chunk of chunkLen elements plus the chunk index.
type Vec[T any] struct {
	chunks [][]T
	n      int
}

const chunkLen = 256

// Len is the number of elements.
func (v Vec[T]) Len() int { return v.n }

// At returns element i.
func (v Vec[T]) At(i int) T { return v.chunks[i/chunkLen][i%chunkLen] }

// Ptr returns a pointer to element i for reading. Writing through it would
// change every version that shares the chunk; use Set.
func (v Vec[T]) Ptr(i int) *T { return &v.chunks[i/chunkLen][i%chunkLen] }

// Append adds an element.
func (v *Vec[T]) Append(x T) {
	if v.n%chunkLen == 0 {
		v.chunks = append(v.chunks[:len(v.chunks):len(v.chunks)], make([]T, 0, chunkLen))
	} else {
		last := len(v.chunks) - 1
		c := make([]T, len(v.chunks[last]), chunkLen)
		copy(c, v.chunks[last])
		chunks := make([][]T, len(v.chunks))
		copy(chunks, v.chunks)
		chunks[last] = c
		v.chunks = chunks
	}
	last := len(v.chunks) - 1
	v.chunks[last] = append(v.chunks[last], x)
	v.n++
}

// Set replaces element i.
func (v *Vec[T]) Set(i int, x T) {
	ci := i / chunkLen
	c := make([]T, len(v.chunks[ci]), chunkLen)
	copy(c, v.chunks[ci])
	c[i%chunkLen] = x
	chunks := make([][]T, len(v.chunks))
	copy(chunks, v.chunks)
	chunks[ci] = c
	v.chunks = chunks
}
