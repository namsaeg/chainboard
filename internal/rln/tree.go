package rln

import (
	"sync/atomic"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
)

var zeros [Depth + 1]fr.Element

func init() {
	for i := 1; i <= Depth; i++ {
		zeros[i] = Node(zeros[i-1], zeros[i-1])
	}
}

const pageBits = 8 // 256 nodes per page

type page struct {
	owner   uint64
	present [1 << pageBits / 64]uint64
	v       [1 << pageBits]fr.Element
}

var treeIDs atomic.Uint64

// Tree is a sparse Merkle tree of member leaves. Removed members are
// overwritten with the zero leaf; indices are never reused.
//
// Clones share storage copy-on-write in pages of 256 nodes, so cloning a
// state with many members is cheap.
type Tree struct {
	Next  uint32
	id    atomic.Uint64
	pages map[uint64]*page
}

// NewTree returns an empty tree.
func NewTree() *Tree {
	t := &Tree{pages: map[uint64]*page{}}
	t.id.Store(treeIDs.Add(1))
	return t
}

func pageKey(level int, idx uint64) (uint64, int) {
	return uint64(level)<<40 | idx>>pageBits, int(idx & (1<<pageBits - 1))
}

func (t *Tree) get(level int, idx uint64) fr.Element {
	k, slot := pageKey(level, idx)
	if p, ok := t.pages[k]; ok && p.present[slot/64]&(1<<(slot%64)) != 0 {
		return p.v[slot]
	}
	return zeros[level]
}

func (t *Tree) put(level int, idx uint64, v fr.Element) {
	k, slot := pageKey(level, idx)
	p, ok := t.pages[k]
	id := t.id.Load()
	if !ok {
		p = &page{owner: id}
		t.pages[k] = p
	} else if p.owner != id {
		c := *p
		c.owner = id
		p = &c
		t.pages[k] = p
	}
	if v == zeros[level] {
		p.present[slot/64] &^= 1 << (slot % 64)
	} else {
		p.present[slot/64] |= 1 << (slot % 64)
		p.v[slot] = v
	}
}

// Clone returns an independent tree. Both trees stop owning their pages, so
// the first write to a shared page copies it.
func (t *Tree) Clone() *Tree {
	c := &Tree{Next: t.Next, pages: make(map[uint64]*page, len(t.pages))}
	for k, v := range t.pages {
		c.pages[k] = v
	}
	c.id.Store(treeIDs.Add(1))
	t.id.Store(treeIDs.Add(1))
	return c
}

// Full reports whether no index is left.
func (t *Tree) Full() bool { return uint64(t.Next) >= 1<<Depth }

// Append adds a leaf and returns its index.
func (t *Tree) Append(leaf fr.Element) uint32 {
	i := t.Next
	t.Next++
	t.Set(i, leaf)
	return i
}

// Set writes leaf i and recomputes its path.
func (t *Tree) Set(i uint32, leaf fr.Element) {
	idx := uint64(i)
	cur := leaf
	for level := 0; level <= Depth; level++ {
		t.put(level, idx, cur)
		if level == Depth {
			break
		}
		sib := t.get(level, idx^1)
		if idx&1 == 0 {
			cur = Node(cur, sib)
		} else {
			cur = Node(sib, cur)
		}
		idx >>= 1
	}
}

// Leaf returns leaf i.
func (t *Tree) Leaf(i uint32) fr.Element { return t.get(0, uint64(i)) }

// Root returns the tree root.
func (t *Tree) Root() fr.Element { return t.get(Depth, 0) }

// Path returns the sibling path and direction bits (1 = node is right child).
func (t *Tree) Path(i uint32) (path [Depth]fr.Element, bits [Depth]uint8) {
	idx := uint64(i)
	for level := 0; level < Depth; level++ {
		path[level] = t.get(level, idx^1)
		bits[level] = uint8(idx & 1)
		idx >>= 1
	}
	return
}

// nodes counts stored (non-zero) nodes; for tests.
func (t *Tree) nodes() int {
	n := 0
	for _, p := range t.pages {
		for _, w := range p.present {
			for ; w != 0; w &= w - 1 {
				n++
			}
		}
	}
	return n
}
