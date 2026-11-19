package chunk

import (
	"crypto/rand"
	"testing"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/merkle"
	"github.com/namsaeg/chainboard/internal/pack"
	"github.com/namsaeg/chainboard/internal/params"
)

func TestSubProofsAndPacking(t *testing.T) {
	p := params.Devnet()
	for _, size := range []int{1, 8191, 8192, 300_000, 3*params.ChunkSize + 777} {
		data := make([]byte, size)
		rand.Read(data)
		root, leaves := Root(data)
		chunks := Split(data)
		if uint64(len(chunks)) != Count(uint64(size)) {
			t.Fatal("count")
		}
		miner := cryptox.Address{1}
		for i, c := range chunks {
			cp, _ := merkle.Prove(leaves, i)
			if !VerifyChunk(root, uint64(size), uint64(i), c, cp) {
				t.Fatal("chunk verify")
			}
			seed := pack.Seed(p, 3, uint64(100+i), miner)
			packed := pack.Pack(seed, c)
			for s := 0; s < SubCount(uint64(size), uint64(i)); s++ {
				pr, err := ProveSub(leaves, i, c, s)
				if err != nil {
					t.Fatal(err)
				}
				psub := packed[s*params.SubChunkSize : (s+1)*params.SubChunkSize]
				sub := pack.UnpackSub(seed, s, psub, SubLen(uint64(size), uint64(i), s))
				if !VerifySub(root, uint64(size), uint64(i), s, sub, pr) {
					t.Fatalf("size=%d chunk=%d sub=%d verify failed", size, i, s)
				}
				other := pack.UnpackSub(pack.Seed(p, 3, uint64(100+i), cryptox.Address{2}), s, psub, len(sub))
				if VerifySub(root, uint64(size), uint64(i), s, other, pr) {
					t.Fatal("replica of another miner accepted")
				}
			}
		}
	}
}
