package store

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
)

func sample(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	return b
}

func TestDataAndChunks(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data := sample(3*params.ChunkSize+100, 1)
	root, err := s.PutData(data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Data(root)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("data round trip failed")
	}

	// A second store receives the same data chunk by chunk, out of order.
	s2, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.Begin(root, uint64(len(data))); err != nil {
		t.Fatal(err)
	}
	for _, i := range []uint64{3, 1} {
		c, path, err := s.ChunkProof(root, i)
		if err != nil {
			t.Fatal(err)
		}
		if err := s2.PutChunk(root, i, c, path); err != nil {
			t.Fatal(err)
		}
	}
	if m, _ := s2.Missing(root); fmt.Sprint(m) != "[0 2]" {
		t.Fatalf("missing %v", m)
	}
	c0, path0, _ := s.ChunkProof(root, 0)
	if s2.PutChunk(root, 0, append([]byte{c0[0] ^ 0xff}, c0[1:]...), path0) == nil {
		t.Fatal("tampered chunk accepted")
	}
	if s2.Begin(root, 5) == nil {
		t.Fatal("size change accepted")
	}
}

func TestPacked(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var m cryptox.Address
	m[0] = 9
	pk := sample(params.ChunkSize, 3)
	if err := s.PutPacked(m, Packed{Bucket: 4, Position: 7, Data: pk}, Packed{Bucket: 5, Position: 1, Data: pk}); err != nil {
		t.Fatal(err)
	}
	sub, err := s.PackedSub(m, 4, 7, 2)
	if err != nil || !bytes.Equal(sub, pk[2*params.SubChunkSize:3*params.SubChunkSize]) {
		t.Fatal("packed sub mismatch")
	}
	if _, err := s.PackedSub(m, 4, 7, (params.ChunkSize / params.SubChunkSize)); err == nil {
		t.Fatal("out of range sub read")
	}
	if err := s.DropBucket(m, 4); err != nil {
		t.Fatal(err)
	}
	if s.HasPacked(m, 4, 7) || !s.HasPacked(m, 5, 1) {
		t.Fatal("drop removed the wrong bucket")
	}
	if got := s.PackedBuckets(m); len(got) != 1 || got[0] != 5 {
		t.Fatalf("packed buckets %v", got)
	}
}

// A recall range is read with one read per run of stored positions, across
// segment boundaries and gaps.
func TestReadPacked(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var m cryptox.Address
	m[1] = 3
	stored := map[uint64]bool{SegChunks - 2: true, SegChunks - 1: true, SegChunks: true, SegChunks + 2: true}
	for pos := range stored {
		if err := s.PutPacked(m, Packed{Bucket: 2, Position: pos, Data: sample(params.ChunkSize, byte(pos))}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(s *Store) {
		got, err := s.ReadPacked(m, 2, SegChunks-3, 6)
		if err != nil {
			t.Fatal(err)
		}
		for i, b := range got {
			pos := uint64(SegChunks - 3 + i)
			if stored[pos] != (b != nil) || (b != nil && !bytes.Equal(b, sample(params.ChunkSize, byte(pos)))) {
				t.Fatalf("position %d: stored %v, read %d bytes", pos, stored[pos], len(b))
			}
		}
	}
	check(s)
	s.Close()
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	check(s2)
}

func TestUseGenesis(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data := sample(params.ChunkSize+10, 5)
	root, _ := s.PutData(data)
	miner := cryptox.Address{1}
	if err := s.PutPacked(miner, Packed{Bucket: 3, Data: make([]byte, params.ChunkSize)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutText([]byte("글")); err != nil {
		t.Fatal(err)
	}
	if dropped, err := s.UseGenesis(Hash{1}); dropped || err != nil {
		t.Fatalf("first genesis: dropped=%v %v", dropped, err)
	}
	if dropped, _ := s.UseGenesis(Hash{1}); dropped {
		t.Fatal("same genesis dropped data")
	}
	if dropped, _ := s.UseGenesis(Hash{2}); !dropped {
		t.Fatal("another genesis kept replicas")
	}
	if s.HasPacked(miner, 3, 0) || len(s.TextHashes()) != 0 || !s.Complete(root) {
		t.Fatal("wrong things dropped")
	}
}
