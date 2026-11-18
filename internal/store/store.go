// Package store keeps data on disk: raw chunks of uploaded data (verified
// against their data root) and this node's packed replicas.
package store

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/merkle"
	"github.com/namsaeg/chainboard/internal/params"
)

type Hash = cryptox.Hash

// Store is safe for concurrent use.
type Store struct {
	dir string
	mu  sync.RWMutex
	// leaves caches the chunk leaves of complete data, to build proofs.
	leaves map[Hash][]Hash
	sizes  map[Hash]uint64
}

type meta struct {
	Size uint64 `json:"size"`
}

// Open creates the directory layout.
func Open(dir string) (*Store, error) {
	for _, d := range []string{"data", "packed"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir, leaves: map[Hash][]Hash{}, sizes: map[Hash]uint64{}}, nil
}

func (s *Store) dataDir(root Hash) string { return filepath.Join(s.dir, "data", root.String()) }

// Begin declares data of a given size; chunks can then arrive in any order.
func (s *Store) Begin(root Hash, size uint64) error {
	if size == 0 {
		return chunk.ErrEmpty
	}
	d := s.dataDir(root)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(meta{Size: size})
	s.mu.Lock()
	s.sizes[root] = size
	s.mu.Unlock()
	return os.WriteFile(filepath.Join(d, "meta.json"), b, 0o644)
}

// Size returns the declared size of data, if known.
func (s *Store) Size(root Hash) (uint64, bool) {
	s.mu.RLock()
	sz, ok := s.sizes[root]
	s.mu.RUnlock()
	if ok {
		return sz, true
	}
	b, err := os.ReadFile(filepath.Join(s.dataDir(root), "meta.json"))
	if err != nil {
		return 0, false
	}
	var m meta
	if json.Unmarshal(b, &m) != nil {
		return 0, false
	}
	s.mu.Lock()
	s.sizes[root] = m.Size
	s.mu.Unlock()
	return m.Size, true
}

// PutChunk stores chunk i after checking it against the data root.
func (s *Store) PutChunk(root Hash, i uint64, c []byte, path merkle.Proof) error {
	size, ok := s.Size(root)
	if !ok {
		return errors.New("store: unknown data root")
	}
	if !chunk.VerifyChunk(root, size, i, c, path) {
		return errors.New("store: chunk does not match data root")
	}
	return os.WriteFile(filepath.Join(s.dataDir(root), fmt.Sprintf("%d.chunk", i)), c, 0o644)
}

// PutData stores complete data (its root is computed here).
func (s *Store) PutData(data []byte) (Hash, error) {
	root, leaves := chunk.Root(data)
	if err := s.Begin(root, uint64(len(data))); err != nil {
		return root, err
	}
	for i, c := range chunk.Split(data) {
		if err := os.WriteFile(filepath.Join(s.dataDir(root), fmt.Sprintf("%d.chunk", i)), c, 0o644); err != nil {
			return root, err
		}
	}
	s.mu.Lock()
	s.leaves[root] = leaves
	s.mu.Unlock()
	return root, nil
}

// Chunk reads raw chunk i.
func (s *Store) Chunk(root Hash, i uint64) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.dataDir(root), fmt.Sprintf("%d.chunk", i)))
}

// Missing lists chunk indexes not yet stored.
func (s *Store) Missing(root Hash) ([]uint64, bool) {
	size, ok := s.Size(root)
	if !ok {
		return nil, false
	}
	var out []uint64
	for i := uint64(0); i < chunk.Count(size); i++ {
		if _, err := os.Stat(filepath.Join(s.dataDir(root), fmt.Sprintf("%d.chunk", i))); err != nil {
			out = append(out, i)
		}
	}
	return out, true
}

// Complete reports whether all chunks of the data are present.
func (s *Store) Complete(root Hash) bool {
	m, ok := s.Missing(root)
	return ok && len(m) == 0
}

// Leaves returns the chunk leaves of complete data.
func (s *Store) Leaves(root Hash) ([]Hash, error) {
	s.mu.RLock()
	l, ok := s.leaves[root]
	s.mu.RUnlock()
	if ok {
		return l, nil
	}
	size, ok := s.Size(root)
	if !ok || !s.Complete(root) {
		return nil, errors.New("store: data incomplete")
	}
	n := chunk.Count(size)
	l = make([]Hash, n)
	for i := uint64(0); i < n; i++ {
		c, err := s.Chunk(root, i)
		if err != nil {
			return nil, err
		}
		l[i] = chunk.ChunkLeaf(c)
	}
	s.mu.Lock()
	s.leaves[root] = l
	s.mu.Unlock()
	return l, nil
}

// Data reads complete data.
func (s *Store) Data(root Hash) ([]byte, error) {
	size, ok := s.Size(root)
	if !ok {
		return nil, errors.New("store: unknown data")
	}
	out := make([]byte, 0, size)
	for i := uint64(0); i < chunk.Count(size); i++ {
		c, err := s.Chunk(root, i)
		if err != nil {
			return nil, err
		}
		out = append(out, c...)
	}
	return out, nil
}

// ChunkProof returns chunk i and its path.
func (s *Store) ChunkProof(root Hash, i uint64) ([]byte, merkle.Proof, error) {
	l, err := s.Leaves(root)
	if err != nil {
		return nil, nil, err
	}
	c, err := s.Chunk(root, i)
	if err != nil {
		return nil, nil, err
	}
	p, err := merkle.Prove(l, int(i))
	return c, p, err
}

// SubProof builds the proof of sub-chunk sub of chunk i.
func (s *Store) SubProof(root Hash, i uint64, sub int) (chunk.Proof, error) {
	l, err := s.Leaves(root)
	if err != nil {
		return chunk.Proof{}, err
	}
	c, err := s.Chunk(root, i)
	if err != nil {
		return chunk.Proof{}, err
	}
	return chunk.ProveSub(l, int(i), c, sub)
}

// Packed replicas are unique to a miner address, so they are kept per address.
func (s *Store) packedPath(miner cryptox.Address, bucket uint32, pos uint64) string {
	return filepath.Join(s.dir, "packed", hex.EncodeToString(miner[:]), fmt.Sprint(bucket), fmt.Sprintf("%d.pk", pos))
}

// PutPacked stores a packed replica.
func (s *Store) PutPacked(miner cryptox.Address, bucket uint32, pos uint64, packed []byte) error {
	p := s.packedPath(miner, bucket, pos)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, packed, 0o644)
}

// HasPacked reports whether a packed replica exists.
func (s *Store) HasPacked(miner cryptox.Address, bucket uint32, pos uint64) bool {
	_, err := os.Stat(s.packedPath(miner, bucket, pos))
	return err == nil
}

// PackedSub reads one packed sub-chunk.
func (s *Store) PackedSub(miner cryptox.Address, bucket uint32, pos uint64, sub int) ([]byte, error) {
	f, err := os.Open(s.packedPath(miner, bucket, pos))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, params.SubChunkSize)
	if _, err := f.ReadAt(buf, int64(sub*params.SubChunkSize)); err != nil {
		return nil, err
	}
	return buf, nil
}

// DropBucket removes packed replicas of an expired bucket.
func (s *Store) DropBucket(miner cryptox.Address, bucket uint32) error {
	return os.RemoveAll(filepath.Join(s.dir, "packed", hex.EncodeToString(miner[:]), fmt.Sprint(bucket)))
}

// HexRoot formats a root for paths and logs.
func HexRoot(h Hash) string { return hex.EncodeToString(h[:8]) }
