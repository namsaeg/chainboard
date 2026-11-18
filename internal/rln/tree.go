package rln

import "github.com/consensys/gnark-crypto/ecc/bn254/fr"

var zeros [Depth + 1]fr.Element

func init() {
	for i := 1; i <= Depth; i++ {
		zeros[i] = Node(zeros[i-1], zeros[i-1])
	}
}

// Tree is a sparse, append-mostly Merkle tree of member leaves. Removed
// members are overwritten with the zero leaf; indices are never reused.
type Tree struct {
	Next  uint32
	nodes map[uint64]fr.Element
}

// NewTree returns an empty tree.
func NewTree() *Tree { return &Tree{nodes: map[uint64]fr.Element{}} }

func key(level int, idx uint64) uint64 { return uint64(level)<<32 | idx }

func (t *Tree) get(level int, idx uint64) fr.Element {
	if v, ok := t.nodes[key(level, idx)]; ok {
		return v
	}
	return zeros[level]
}

// Clone deep-copies the tree.
func (t *Tree) Clone() *Tree {
	c := &Tree{Next: t.Next, nodes: make(map[uint64]fr.Element, len(t.nodes))}
	for k, v := range t.nodes {
		c.nodes[k] = v
	}
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
		if cur == zeros[level] {
			delete(t.nodes, key(level, idx))
		} else {
			t.nodes[key(level, idx)] = cur
		}
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
