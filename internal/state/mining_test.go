package state

import (
	"testing"

	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/types"
)

// withBuckets is a testnet state whose weave has buckets of the given
// lengths (in chunks), each one record.
func withBuckets(lengths ...uint64) *State {
	s, _ := Genesis(params.Testnet())
	for i, n := range lengths {
		id := uint32(1000 + i) // far in the future: full term weight
		b := s.bucket(id)
		b.Records.Append(Record{DataRoot: Hash{byte(i), 1}, Size: n * params.ChunkSize, Start: 0, Chunks: n})
		b.Len, b.EpochLen = n, n
	}
	return s
}

// Reads per step follow stored data, not the number of partitions: a weave
// of one big bucket and two hundred small ones costs about weave/span reads.
func TestRecallReadsFollowData(t *testing.T) {
	small := make([]uint64, 200)
	for i := range small {
		small[i] = 1024 // 256 MiB each
	}
	s := withBuckets(append([]uint64{3 << 21}, small...)...) // 1.5 TiB + 50 GiB
	weave := s.WeaveChunks()
	var miner cryptox.Address
	const steps = 400
	hits := 0
	for i := 0; i < steps; i++ {
		out := cryptox.Tagged("t", cryptox.U64(uint64(i)))
		for _, p := range s.Partitions() {
			if _, ok := s.RangeFor(out, p, miner, 1, weave); ok {
				hits++
			}
		}
	}
	perStep := float64(hits) / steps
	// Expected: 3 full partitions + the genesis and small buckets drawn over
	// span = weave/32 ≈ 49 GiB: 3 + (16 MiB + 50 GiB)/49 GiB ≈ 4.
	if perStep < 3.5 || perStep > 4.6 {
		t.Fatalf("%.2f recall ranges per step, want about 4", perStep)
	}
	if len(s.Partitions()) < 200 {
		t.Fatal("test weave too small")
	}
}

// Ranges never leave their partition and every position in them is a
// ticket of the data placed there.
func TestRecallRangeBounds(t *testing.T) {
	s := withBuckets(5)
	weave := s.WeaveChunks()
	var miner cryptox.Address
	for i := 0; i < 200; i++ {
		out := cryptox.Tagged("b", cryptox.U64(uint64(i)))
		rr, ok := s.RangeFor(out, PartitionID(1000, 0), miner, 1, weave)
		if !ok {
			continue
		}
		if rr.Start >= rr.End || rr.End > 5 || rr.End-rr.Start > s.P.RecallChunks {
			t.Fatalf("bad range %+v", rr)
		}
		if _, ok := s.Ticket(rr, rr.Start, 0); !ok {
			t.Fatal("first position of a range is not a ticket")
		}
		if _, ok := s.Ticket(rr, rr.End, 0); ok {
			t.Fatal("position past the range is a ticket")
		}
		if _, ok := s.Ticket(rr, rr.Start, params.ChunkSize/params.SubChunkSize); ok {
			t.Fatal("sub-chunk past the chunk is a ticket")
		}
	}
}

// The storage estimate inverts the ticket rate: the work of k replicas'
// worth of tickets per step reads as k times the weave.
func TestProvenBytesInvertsTickets(t *testing.T) {
	s := withBuckets(3<<21, 1024)
	weave := s.WeaveChunks()
	var ranges float64
	for _, p := range s.Partitions() {
		_, _, length, _ := s.PartitionRange(p)
		ranges += float64(length) / float64(s.Span(length, weave))
	}
	tickets := ranges * float64(s.P.RecallChunks*params.ChunkSize/params.SubChunkSize)
	const replicas, steps = 6, 60
	work := types.Work(params.Testnet().MaxTarget) // 2
	work.SetUint64(uint64(tickets * replicas * steps))
	got := float64(s.ProvenBytes(work, steps))
	want := float64(replicas * weave * params.ChunkSize)
	if got < want*0.99 || got > want*1.01 {
		t.Fatalf("proven %.3g, want %.3g", got, want)
	}
}
