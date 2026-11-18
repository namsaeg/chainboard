package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/namsaeg/chainboard/internal/chunk"
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
	if _, err := s.PackedSub(m, 4, 7, params.SubPerChunk); err == nil {
		t.Fatal("out of range sub read")
	}
	if err := s.DropBucket(m, 4); err != nil {
		t.Fatal(err)
	}
	if s.HasPacked(m, 4, 7) || !s.HasPacked(m, 5, 1) {
		t.Fatal("drop removed the wrong bucket")
	}
}

func TestImportV010(t *testing.T) {
	old := t.TempDir()
	good := sample(params.ChunkSize+10, 5)
	bad := sample(params.ChunkSize+10, 6)
	write := func(data []byte, drop int) {
		root, _ := chunk.Root(data)
		d := filepath.Join(old, "data", root.String())
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "meta.json"), []byte(fmt.Sprintf(`{"size":%d}`, len(data))), 0o644)
		for i, c := range chunk.Split(data) {
			if i != drop {
				os.WriteFile(filepath.Join(d, fmt.Sprintf("%d.chunk", i)), c, 0o644)
			}
		}
	}
	write(good, -1)
	write(bad, 1) // incomplete: skipped
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n, err := s.ImportV010(old)
	if err != nil || n != 1 {
		t.Fatalf("imported %d, %v", n, err)
	}
	gr, _ := chunk.Root(good)
	br, _ := chunk.Root(bad)
	if !s.Complete(gr) || s.Complete(br) {
		t.Fatal("import picked the wrong files")
	}
}
