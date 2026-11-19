package pack

import (
	"bytes"
	"testing"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
)

func TestPackRoundTrip(t *testing.T) {
	p := params.Devnet()
	var a, b cryptox.Address
	b[0] = 1
	s1 := Seed(p, 3, 9, a)
	if s1 == Seed(p, 3, 9, b) || s1 == Seed(p, 3, 10, a) || s1 != Seed(p, 3, 9, a) {
		t.Fatal("seed must depend on miner and position only")
	}
	raw := bytes.Repeat([]byte("chainboard"), 3000)
	pk := Pack(s1, raw)
	if len(pk) != params.ChunkSize || !bytes.Equal(xorAt(s1, 0, pk)[:len(raw)], raw) {
		t.Fatal("unpack")
	}
	sub := UnpackSub(s1, 1, pk[params.SubChunkSize:2*params.SubChunkSize], params.SubChunkSize)
	if !bytes.Equal(sub, raw[params.SubChunkSize:2*params.SubChunkSize]) {
		t.Fatal("unpack sub")
	}
}

func BenchmarkSeedTestnet(b *testing.B) {
	p := params.Testnet()
	var a cryptox.Address
	for i := 0; i < b.N; i++ {
		Seed(p, 1, uint64(i), a)
	}
}
