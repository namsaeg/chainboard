// Package chunk splits uploaded data into 256 KiB chunks of 8 KiB sub-chunks
// and builds the data root that goes on chain.
//
//	sub-chunk leaf = merkle.Leaf(sub-chunk bytes)
//	chunk leaf     = Tagged("cb/chunk", chunk length, root of its sub-chunk leaves)
//	data root      = merkle root of the chunk leaves
//
// A proof that one sub-chunk belongs to a data root carries the sub-chunk
// (8 KiB), its path inside the chunk and the chunk's path inside the data.
package chunk

import (
	"errors"

	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/merkle"
	"github.com/namsaeg/chainboard/internal/params"
)

type Hash = cryptox.Hash

// Count returns the number of chunks for size bytes.
func Count(size uint64) uint64 { return (size + params.ChunkSize - 1) / params.ChunkSize }

// Len returns the length of chunk i of data of size bytes.
func Len(size, i uint64) uint64 {
	start := i * params.ChunkSize
	if start >= size {
		return 0
	}
	if size-start < params.ChunkSize {
		return size - start
	}
	return params.ChunkSize
}

func subCount(chunkLen uint64) int {
	return int((chunkLen + params.SubChunkSize - 1) / params.SubChunkSize)
}

func subLeaves(c []byte) []Hash {
	n := subCount(uint64(len(c)))
	out := make([]Hash, n)
	for i := 0; i < n; i++ {
		end := (i + 1) * params.SubChunkSize
		if end > len(c) {
			end = len(c)
		}
		out[i] = merkle.Leaf(c[i*params.SubChunkSize : end])
	}
	return out
}

// ChunkLeaf is the leaf of one chunk in the data tree.
func ChunkLeaf(c []byte) Hash {
	return leafFrom(uint64(len(c)), merkle.Root(subLeaves(c)))
}

func leafFrom(length uint64, subRoot Hash) Hash {
	return cryptox.Tagged("cb/chunk", cryptox.U64(length), subRoot[:])
}

// Split cuts data into chunks.
func Split(data []byte) [][]byte {
	var out [][]byte
	for i := 0; i < len(data); i += params.ChunkSize {
		end := i + params.ChunkSize
		if end > len(data) {
			end = len(data)
		}
		out = append(out, data[i:end])
	}
	return out
}

// Root returns the data root of data and its chunk leaves.
func Root(data []byte) (Hash, []Hash) {
	chunks := Split(data)
	leaves := make([]Hash, len(chunks))
	for i, c := range chunks {
		leaves[i] = ChunkLeaf(c)
	}
	return merkle.Root(leaves), leaves
}

// Proof proves one sub-chunk of one chunk against a data root.
type Proof struct {
	SubPath   merkle.Proof
	ChunkPath merkle.Proof
}

// VerifyChunk checks a whole chunk against a data root.
func VerifyChunk(root Hash, size uint64, i uint64, c []byte, path merkle.Proof) bool {
	if uint64(len(c)) != Len(size, i) || len(c) == 0 {
		return false
	}
	return merkle.Verify(root, ChunkLeaf(c), int(i), int(Count(size)), path)
}

// ProveSub builds the proof for sub-chunk s of chunk c (chunk index i).
func ProveSub(chunkLeaves []Hash, i int, c []byte, s int) (Proof, error) {
	sl := subLeaves(c)
	sp, err := merkle.Prove(sl, s)
	if err != nil {
		return Proof{}, err
	}
	cp, err := merkle.Prove(chunkLeaves, i)
	if err != nil {
		return Proof{}, err
	}
	return Proof{SubPath: sp, ChunkPath: cp}, nil
}

// VerifySub checks that sub is sub-chunk s of chunk i of the data (root, size).
func VerifySub(root Hash, size, i uint64, s int, sub []byte, p Proof) bool {
	cl := Len(size, i)
	if cl == 0 {
		return false
	}
	n := subCount(cl)
	if s < 0 || s >= n {
		return false
	}
	want := params.SubChunkSize
	if s == n-1 {
		want = int(cl) - s*params.SubChunkSize
	}
	if len(sub) != want {
		return false
	}
	// Recompute the sub-chunk root from the path, then the chunk leaf.
	subRoot, ok := rootFromPath(merkle.Leaf(sub), s, n, p.SubPath)
	if !ok {
		return false
	}
	return merkle.Verify(root, leafFrom(cl, subRoot), int(i), int(Count(size)), p.ChunkPath)
}

// rootFromPath folds a path into a root (same shape rules as merkle.Verify).
func rootFromPath(leaf Hash, idx, n int, p merkle.Proof) (Hash, bool) {
	cur := leaf
	used := 0
	for n > 1 {
		sib := idx ^ 1
		if sib < n {
			if used >= len(p) {
				return Hash{}, false
			}
			if idx%2 == 0 {
				cur = cryptox.Tagged("cb/mnode", cur[:], p[used][:])
			} else {
				cur = cryptox.Tagged("cb/mnode", p[used][:], cur[:])
			}
			used++
		}
		idx /= 2
		n = (n + 1) / 2
	}
	return cur, used == len(p)
}

// SubLen is the length of sub-chunk s of chunk i.
func SubLen(size, i uint64, s int) int {
	cl := Len(size, i)
	n := subCount(cl)
	if s < 0 || s >= n {
		return 0
	}
	if s == n-1 {
		return int(cl) - s*params.SubChunkSize
	}
	return params.SubChunkSize
}

// SubCount is the number of sub-chunks in chunk i.
func SubCount(size, i uint64) int { return subCount(Len(size, i)) }

// EncodeProof / DecodeProof serialise a Proof.
func EncodeProof(w *codec.Writer, p Proof) {
	w.Var(uint64(len(p.SubPath)))
	for _, h := range p.SubPath {
		w.Fixed(h[:])
	}
	w.Var(uint64(len(p.ChunkPath)))
	for _, h := range p.ChunkPath {
		w.Fixed(h[:])
	}
}

func DecodeProof(r *codec.Reader) Proof {
	var p Proof
	n := r.Count(64)
	for i := 0; i < n; i++ {
		var h Hash
		r.Fixed(h[:])
		p.SubPath = append(p.SubPath, h)
	}
	n = r.Count(64)
	for i := 0; i < n; i++ {
		var h Hash
		r.Fixed(h[:])
		p.ChunkPath = append(p.ChunkPath, h)
	}
	return p
}

var ErrEmpty = errors.New("chunk: empty data")
