// Package store keeps data on disk in one bbolt database: raw chunks of
// uploaded data (verified against their data root) and this node's packed
// replicas. Every write is a single atomic transaction.
package store

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/merkle"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/types"
)

type Hash = cryptox.Hash

var (
	bMeta   = []byte("meta")   // root -> size (u64)
	bChunks = []byte("chunks") // root || index (u64) -> raw chunk
	bPacked = []byte("packed") // miner || bucket (u32) || position (u64) -> packed replica
	bTexts  = []byte("texts")  // text hash -> post body
)

// Store is safe for concurrent use.
type Store struct {
	db *bolt.DB
	mu sync.RWMutex
	// leaves caches the chunk leaves of complete data, to build proofs.
	leaves map[Hash][]Hash
}

// Open opens (or creates) dir/store.db.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(dir, "store.db"), 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bMeta, bChunks, bPacked, bTexts} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, leaves: map[Hash][]Hash{}}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func chunkKey(root Hash, i uint64) []byte {
	k := make([]byte, 40)
	copy(k, root[:])
	binary.BigEndian.PutUint64(k[32:], i)
	return k
}

func packedPrefix(miner cryptox.Address, bucket uint32) []byte {
	k := make([]byte, 24, 32)
	copy(k, miner[:])
	binary.BigEndian.PutUint32(k[20:], bucket)
	return k
}

func packedKey(miner cryptox.Address, bucket uint32, pos uint64) []byte {
	return binary.BigEndian.AppendUint64(packedPrefix(miner, bucket), pos)
}

func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

// Begin declares data of a given size; chunks can then arrive in any order.
func (s *Store) Begin(root Hash, size uint64) error {
	if size == 0 {
		return chunk.ErrEmpty
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		m := tx.Bucket(bMeta)
		if v := m.Get(root[:]); v != nil {
			if binary.BigEndian.Uint64(v) != size {
				return errors.New("store: data root already declared with another size")
			}
			return nil
		}
		return m.Put(root[:], u64(size))
	})
}

// Size returns the declared size of data, if known.
func (s *Store) Size(root Hash) (size uint64, ok bool) {
	_ = s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bMeta).Get(root[:]); v != nil {
			size, ok = binary.BigEndian.Uint64(v), true
		}
		return nil
	})
	return
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
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bChunks).Put(chunkKey(root, i), c)
	})
}

// PutData stores complete data (its root is computed here) in one transaction.
func (s *Store) PutData(data []byte) (Hash, error) {
	if len(data) == 0 {
		return Hash{}, chunk.ErrEmpty
	}
	root, leaves := chunk.Root(data)
	err := s.db.Update(func(tx *bolt.Tx) error {
		m := tx.Bucket(bMeta)
		if v := m.Get(root[:]); v != nil && binary.BigEndian.Uint64(v) != uint64(len(data)) {
			return errors.New("store: data root already declared with another size")
		}
		if err := m.Put(root[:], u64(uint64(len(data)))); err != nil {
			return err
		}
		cb := tx.Bucket(bChunks)
		for i, c := range chunk.Split(data) {
			if err := cb.Put(chunkKey(root, uint64(i)), c); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return root, err
	}
	s.mu.Lock()
	s.leaves[root] = leaves
	s.mu.Unlock()
	return root, nil
}

// Chunk reads raw chunk i.
func (s *Store) Chunk(root Hash, i uint64) ([]byte, error) {
	var out []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bChunks).Get(chunkKey(root, i))
		if v == nil {
			return os.ErrNotExist
		}
		out = append([]byte(nil), v...)
		return nil
	})
	return out, err
}

// Missing lists chunk indexes not yet stored.
func (s *Store) Missing(root Hash) ([]uint64, bool) {
	size, ok := s.Size(root)
	if !ok {
		return nil, false
	}
	n := chunk.Count(size)
	have := make([]bool, n)
	_ = s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bChunks).Cursor()
		for k, _ := c.Seek(chunkKey(root, 0)); k != nil && Hash(k[:32]) == root; k, _ = c.Next() {
			if i := binary.BigEndian.Uint64(k[32:]); i < n {
				have[i] = true
			}
		}
		return nil
	})
	var out []uint64
	for i, h := range have {
		if !h {
			out = append(out, uint64(i))
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
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bChunks)
		for i := uint64(0); i < n; i++ {
			l[i] = chunk.ChunkLeaf(b.Get(chunkKey(root, i)))
		}
		return nil
	})
	if err != nil {
		return nil, err
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
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bChunks)
		for i := uint64(0); i < chunk.Count(size); i++ {
			v := b.Get(chunkKey(root, i))
			if v == nil {
				return errors.New("store: data incomplete")
			}
			out = append(out, v...)
		}
		return nil
	})
	return out, err
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

// Packed is one packed replica to store.
type Packed struct {
	Bucket   uint32
	Position uint64
	Data     []byte
}

// PutPacked stores packed replicas of one miner in a single transaction.
// Replicas are unique to a miner address, so they are keyed by it.
func (s *Store) PutPacked(miner cryptox.Address, ps ...Packed) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bPacked)
		for _, p := range ps {
			if err := b.Put(packedKey(miner, p.Bucket, p.Position), p.Data); err != nil {
				return err
			}
		}
		return nil
	})
}

// HasPacked reports whether a packed replica exists.
func (s *Store) HasPacked(miner cryptox.Address, bucket uint32, pos uint64) bool {
	var ok bool
	_ = s.db.View(func(tx *bolt.Tx) error {
		ok = tx.Bucket(bPacked).Get(packedKey(miner, bucket, pos)) != nil
		return nil
	})
	return ok
}

// PackedSub reads one packed sub-chunk.
func (s *Store) PackedSub(miner cryptox.Address, bucket uint32, pos uint64, sub int) ([]byte, error) {
	var out []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bPacked).Get(packedKey(miner, bucket, pos))
		lo, hi := sub*params.SubChunkSize, (sub+1)*params.SubChunkSize
		if v == nil || sub < 0 || hi > len(v) {
			return os.ErrNotExist
		}
		out = append([]byte(nil), v[lo:hi]...)
		return nil
	})
	return out, err
}

// DropBucket removes the packed replicas of an expired bucket.
func (s *Store) DropBucket(miner cryptox.Address, bucket uint32) error {
	prefix := packedPrefix(miner, bucket)
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bPacked)
		var keys [][]byte
		c := b.Cursor()
		for k, _ := c.Seek(prefix); k != nil && string(k[:24]) == string(prefix); k, _ = c.Next() {
			keys = append(keys, append([]byte(nil), k...))
		}
		for _, k := range keys {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

// ImportV010 copies the complete data of a 0.1.0 file store (dir/data/<root>/
// with meta.json and N.chunk files) into this store and returns how many
// data roots it imported. Each imported file is re-verified against its root.
// Packed replicas are not imported: 0.2.0 packs with a different function.
func (s *Store) ImportV010(dir string) (int, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "data"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		want, err := cryptox.ParseHash(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		var data []byte
		for i := 0; ; i++ {
			c, err := os.ReadFile(filepath.Join(dir, "data", e.Name(), fmt.Sprintf("%d.chunk", i)))
			if err != nil {
				break
			}
			data = append(data, c...)
		}
		if len(data) == 0 {
			continue
		}
		if root, _ := chunk.Root(data); root != want {
			continue // incomplete or damaged: the network will refill it
		}
		if _, err := s.PutData(data); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// HexRoot formats a root for logs.
func HexRoot(h Hash) string { return hex.EncodeToString(h[:8]) }

// PutText keeps a post body, keyed by its hash. Bodies are checked here, so
// a stored body always matches its key.
func (s *Store) PutText(body []byte) (Hash, error) {
	if len(body) == 0 || len(body) > params.MaxPostBytes || !utf8.Valid(body) {
		return Hash{}, errors.New("store: not a valid post body")
	}
	h := types.TextHashOf(body)
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bTexts)
		if b.Get(h[:]) != nil {
			return nil
		}
		return b.Put(h[:], body)
	})
	return h, err
}

// Text reads a post body.
func (s *Store) Text(h Hash) ([]byte, bool) {
	var out []byte
	_ = s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket(bTexts).Get(h[:]); v != nil {
			out = append([]byte(nil), v...)
		}
		return nil
	})
	return out, out != nil
}

// HasText reports whether a body is stored.
func (s *Store) HasText(h Hash) bool {
	ok := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		ok = tx.Bucket(bTexts).Get(h[:]) != nil
		return nil
	})
	return ok
}

// TextHashes lists the stored bodies.
func (s *Store) TextHashes() []Hash {
	var out []Hash
	_ = s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bTexts).ForEach(func(k, _ []byte) error {
			var h Hash
			copy(h[:], k)
			out = append(out, h)
			return nil
		})
	})
	return out
}

// DeleteTexts removes bodies.
func (s *Store) DeleteTexts(hs []Hash) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bTexts)
		for _, h := range hs {
			if err := b.Delete(h[:]); err != nil {
				return err
			}
		}
		return nil
	})
}

// Roots lists the data roots this store knows (complete or not).
func (s *Store) Roots() []Hash {
	var out []Hash
	_ = s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bMeta).ForEach(func(k, _ []byte) error {
			var h Hash
			copy(h[:], k)
			out = append(out, h)
			return nil
		})
	})
	return out
}

// DeleteData removes data and its chunks (not packed replicas).
func (s *Store) DeleteData(root Hash) error {
	size, ok := s.Size(root)
	if !ok {
		return nil
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		cb := tx.Bucket(bChunks)
		for i := uint64(0); i < chunk.Count(size); i++ {
			if err := cb.Delete(chunkKey(root, i)); err != nil {
				return err
			}
		}
		return tx.Bucket(bMeta).Delete(root[:])
	})
	s.mu.Lock()
	delete(s.leaves, root)
	s.mu.Unlock()
	return err
}
