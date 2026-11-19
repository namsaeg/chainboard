package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	bolt "go.etcd.io/bbolt"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
)

// Packed replicas live in flat files, one per segment of SegChunks positions
// of a bucket: packed/<miner>/<bucket>/<segment>.dat, the replica of
// position p at offset (p mod SegChunks) × ChunkSize. Mining reads a recall
// range with one positioned read; files stay small enough (64 MiB) that a
// file system without sparse files wastes little on a partly filled segment.
// A presence bitmap per segment in the database says which replicas are
// complete: a replica is written and synced before its bit is set.
const SegChunks = 256

// Packed is one packed replica to store.
type Packed struct {
	Bucket   uint32
	Position uint64
	Data     []byte
}

func segKey(miner cryptox.Address, bucket uint32, seg uint64) []byte {
	k := make([]byte, 0, 32)
	k = append(k, miner[:]...)
	k = binary.BigEndian.AppendUint32(k, bucket)
	return binary.BigEndian.AppendUint64(k, seg)
}

func (s *Store) bucketDir(miner cryptox.Address, bucket uint32) string {
	return filepath.Join(s.dir, "packed", fmt.Sprintf("%x", miner[:]), fmt.Sprint(bucket))
}

func (s *Store) segPath(miner cryptox.Address, bucket uint32, seg uint64) string {
	return filepath.Join(s.bucketDir(miner, bucket), fmt.Sprintf("%d.dat", seg))
}

// fileCache keeps segment files open between reads.
type fileCache struct {
	mu   sync.Mutex
	open map[string]*os.File
}

const maxOpenFiles = 128

func (c *fileCache) get(path string, create bool) (*os.File, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.open[path]; ok {
		return f, nil
	}
	flag := os.O_RDWR
	if create {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		flag |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flag, 0o600)
	if err != nil {
		return nil, err
	}
	if len(c.open) >= maxOpenFiles {
		for p, old := range c.open {
			old.Close()
			delete(c.open, p)
			break
		}
	}
	c.open[path] = f
	return f, nil
}

func (c *fileCache) closeUnder(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := dir + string(filepath.Separator)
	for p, f := range c.open {
		if len(p) > len(prefix) && p[:len(prefix)] == prefix {
			f.Close()
			delete(c.open, p)
		}
	}
}

func (c *fileCache) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for p, f := range c.open {
		f.Close()
		delete(c.open, p)
	}
}

// PutPacked stores packed replicas of one miner. Replicas are unique to a
// miner address, so they are kept per address.
func (s *Store) PutPacked(miner cryptox.Address, ps ...Packed) error {
	type segID struct {
		bucket uint32
		seg    uint64
	}
	touched := map[segID][]uint64{}
	files := map[*os.File]bool{}
	for _, p := range ps {
		if len(p.Data) != params.ChunkSize {
			return errors.New("store: packed replica must be a full chunk")
		}
		id := segID{p.Bucket, p.Position / SegChunks}
		f, err := s.files.get(s.segPath(miner, id.bucket, id.seg), true)
		if err != nil {
			return err
		}
		if _, err := f.WriteAt(p.Data, int64(p.Position%SegChunks)*params.ChunkSize); err != nil {
			return err
		}
		files[f] = true
		touched[id] = append(touched[id], p.Position%SegChunks)
	}
	for f := range files {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bPackIdx)
		for id, offs := range touched {
			k := segKey(miner, id.bucket, id.seg)
			bits := make([]byte, SegChunks/8)
			copy(bits, b.Get(k))
			for _, o := range offs {
				bits[o/8] |= 1 << (o % 8)
			}
			if err := b.Put(k, bits); err != nil {
				return err
			}
		}
		return nil
	})
}

// presence returns, for positions [pos, pos+n), whether each replica is stored.
func (s *Store) presence(miner cryptox.Address, bucket uint32, pos, n uint64) []bool {
	out := make([]bool, n)
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bPackIdx)
		var seg uint64 = ^uint64(0)
		var bits []byte
		for i := uint64(0); i < n; i++ {
			p := pos + i
			if p/SegChunks != seg {
				seg = p / SegChunks
				bits = b.Get(segKey(miner, bucket, seg))
			}
			o := p % SegChunks
			out[i] = bits != nil && bits[o/8]&(1<<(o%8)) != 0
		}
		return nil
	})
	return out
}

// HasPacked reports whether a packed replica exists.
func (s *Store) HasPacked(miner cryptox.Address, bucket uint32, pos uint64) bool {
	return s.presence(miner, bucket, pos, 1)[0]
}

// ReadPacked reads the replicas of positions [pos, pos+n) with as few reads
// as possible: one per run of stored positions inside a segment. Entry i is
// nil when position pos+i is not stored.
func (s *Store) ReadPacked(miner cryptox.Address, bucket uint32, pos, n uint64) ([][]byte, error) {
	have := s.presence(miner, bucket, pos, n)
	out := make([][]byte, n)
	for i := uint64(0); i < n; {
		if !have[i] {
			i++
			continue
		}
		seg := (pos + i) / SegChunks
		j := i
		for j < n && have[j] && (pos+j)/SegChunks == seg {
			j++
		}
		f, err := s.files.get(s.segPath(miner, bucket, seg), false)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, (j-i)*params.ChunkSize)
		if _, err := f.ReadAt(buf, int64((pos+i)%SegChunks)*params.ChunkSize); err != nil {
			return nil, err
		}
		for k := i; k < j; k++ {
			out[k] = buf[(k-i)*params.ChunkSize : (k-i+1)*params.ChunkSize]
		}
		i = j
	}
	return out, nil
}

// PackedSub reads one packed sub-chunk.
func (s *Store) PackedSub(miner cryptox.Address, bucket uint32, pos uint64, sub int) ([]byte, error) {
	if sub < 0 || sub >= params.ChunkSize/params.SubChunkSize || !s.HasPacked(miner, bucket, pos) {
		return nil, os.ErrNotExist
	}
	f, err := s.files.get(s.segPath(miner, bucket, pos/SegChunks), false)
	if err != nil {
		return nil, err
	}
	out := make([]byte, params.SubChunkSize)
	_, err = f.ReadAt(out, int64(pos%SegChunks)*params.ChunkSize+int64(sub)*params.SubChunkSize)
	return out, err
}

// PackedBuckets lists the buckets this miner has replicas of.
func (s *Store) PackedBuckets(miner cryptox.Address) []uint32 {
	seen := map[uint32]bool{}
	_ = s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bPackIdx).Cursor()
		for k, _ := c.Seek(miner[:]); k != nil && string(k[:20]) == string(miner[:]); k, _ = c.Next() {
			seen[binary.BigEndian.Uint32(k[20:24])] = true
		}
		return nil
	})
	out := make([]uint32, 0, len(seen))
	for b := range seen {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// DropBucket removes the packed replicas of an expired bucket.
func (s *Store) DropBucket(miner cryptox.Address, bucket uint32) error {
	prefix := segKey(miner, bucket, 0)[:24]
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bPackIdx)
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
	if err != nil {
		return err
	}
	dir := s.bucketDir(miner, bucket)
	s.files.closeUnder(dir)
	return os.RemoveAll(dir)
}
