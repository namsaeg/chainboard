package p2p

import (
	"testing"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/merkle"
	"github.com/namsaeg/chainboard/internal/params"
)

func fakePeer() *peer { return &peer{sendCh: make(chan frame, 8), quit: make(chan struct{})} }

func bareHost(ps ...*peer) *Host {
	h := &Host{relays: map[relayKey]*relay{}, peers: map[*peer]bool{}}
	for _, p := range ps {
		h.peers[p] = true
	}
	return h
}

// next takes the one frame a fake peer was sent, if any.
func next(p *peer) (frame, bool) {
	select {
	case f := <-p.sendCh:
		return f, true
	default:
		return frame{}, false
	}
}

func getTTL(t *testing.T, f frame) uint8 {
	t.Helper()
	if f.typ != msgGetChunk {
		t.Fatalf("frame %d, want a chunk request", f.typ)
	}
	r := codec.NewReader(f.b)
	var root Hash
	r.Fixed(root[:])
	r.Var()
	ttl := r.U8()
	if err := r.Done(); err != nil {
		t.Fatal(err)
	}
	return ttl
}

// TestRelayHops: a request crosses two relays (asker -> h1 -> h2 -> the
// holder) with its hop budget falling by one at each, stops when it runs
// out, is passed on once however many ask, and only a chunk that verifies
// goes back, to every asker.
func TestRelayHops(t *testing.T) {
	data := make([]byte, 2*params.ChunkSize+5)
	for i := range data {
		data[i] = byte(i * 7)
	}
	root, leaves := chunk.Root(data)
	c := chunk.Split(data)[1]
	path, err := merkle.Prove(leaves, 1)
	if err != nil {
		t.Fatal(err)
	}

	asker, mid := fakePeer(), fakePeer()
	h1 := bareHost(asker, mid)
	h1.relayGet(asker, root, 1, relayTTL)
	f, ok := next(mid)
	if !ok || getTTL(t, f) != relayTTL-1 {
		t.Fatal("first relay did not pass the request on with one hop less")
	}
	if _, ok := next(asker); ok {
		t.Fatal("request sent back to its asker")
	}

	from, holder := fakePeer(), fakePeer()
	h2 := bareHost(from, holder)
	h2.relayGet(from, root, 1, relayTTL-1)
	f, ok = next(holder)
	if !ok || getTTL(t, f) != 0 {
		t.Fatal("second relay did not reach the holder")
	}
	h3 := bareHost(fakePeer(), fakePeer())
	h3.relayGet(holder, root, 1, 0)
	if len(h3.relays) != 0 {
		t.Fatal("a request with no hops left was passed on")
	}

	// A second asker joins the first request instead of a new one.
	other := fakePeer()
	h1.peers[other] = true
	h1.relayGet(other, root, 1, relayTTL)
	if _, ok := next(mid); ok {
		t.Fatal("the same chunk asked for twice")
	}

	bad := append([]byte(nil), c...)
	bad[0] ^= 1
	h1.relayChunk(root, 1, uint64(len(data)), bad, path, []byte("bad"))
	if _, ok := next(asker); ok {
		t.Fatal("a chunk that does not verify was passed back")
	}
	// ... and it did not cancel the relay: the good chunk still goes back.
	h1.relayChunk(root, 1, uint64(len(data)), c, path, []byte("good"))
	for _, p := range []*peer{asker, other} {
		if f, ok := next(p); !ok || f.typ != msgChunk || string(f.b) != "good" {
			t.Fatal("verified chunk not passed back to an asker")
		}
	}
	if len(h1.relays) != 0 {
		t.Fatal("relay kept after its chunk came back")
	}
}
