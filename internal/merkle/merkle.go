// Package merkle builds binary Merkle trees with domain-separated leaf and
// node hashes. An odd node at any level is promoted unchanged.
package merkle

import (
	"errors"

	"github.com/namsaeg/chainboard/internal/cryptox"
)

type Hash = cryptox.Hash

// Leaf hashes raw leaf data.
func Leaf(data []byte) Hash { return cryptox.Tagged("cb/mleaf", data) }

func node(l, r Hash) Hash { return cryptox.Tagged("cb/mnode", l[:], r[:]) }

// Root computes the root over already-hashed leaves. The root of zero leaves
// is the zero hash.
func Root(leaves []Hash) Hash {
	if len(leaves) == 0 {
		return Hash{}
	}
	level := append([]Hash(nil), leaves...)
	for len(level) > 1 {
		next := make([]Hash, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 < len(level) {
				next = append(next, node(level[i], level[i+1]))
			} else {
				next = append(next, level[i])
			}
		}
		level = next
	}
	return level[0]
}

// Proof is the sibling path from a leaf to the root. Steps where the node was
// promoted carry no sibling; the leaf index and leaf count fix the shape.
type Proof []Hash

// Prove returns the path for leaf idx.
func Prove(leaves []Hash, idx int) (Proof, error) {
	if idx < 0 || idx >= len(leaves) {
		return nil, errors.New("merkle: index out of range")
	}
	var p Proof
	level := append([]Hash(nil), leaves...)
	for len(level) > 1 {
		sib := idx ^ 1
		if sib < len(level) {
			p = append(p, level[sib])
		}
		next := make([]Hash, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 < len(level) {
				next = append(next, node(level[i], level[i+1]))
			} else {
				next = append(next, level[i])
			}
		}
		level = next
		idx /= 2
	}
	return p, nil
}

// Verify checks that leaf is at idx of a tree with n leaves and the given root.
func Verify(root, leaf Hash, idx, n int, p Proof) bool {
	if idx < 0 || idx >= n || n <= 0 {
		return false
	}
	cur := leaf
	used := 0
	for n > 1 {
		sib := idx ^ 1
		if sib < n {
			if used >= len(p) {
				return false
			}
			if idx%2 == 0 {
				cur = node(cur, p[used])
			} else {
				cur = node(p[used], cur)
			}
			used++
		}
		idx /= 2
		n = (n + 1) / 2
	}
	return used == len(p) && cur == root
}
