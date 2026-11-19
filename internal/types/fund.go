package types

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"runtime"
	"sync"
	"unicode/utf8"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
)

// Crowdfunding works like a hash-locked, time-locked payment, kept on chain:
// a creator who holds data but no coins signs a FundRequest naming the data
// root (the hash lock) and a deadline (the time lock). Pledges lock coins in
// the request's escrow; they can only pay for storing exactly that data, and
// come back by themselves if it is not stored and proven in time.

// FundRequest asks for pledges to store one piece of data. The creator only
// signs it; the first pledge carries it on chain.
type FundRequest struct {
	Creator  []byte // Ed25519 public key
	DataRoot Hash
	Size     uint64
	Media    uint8
	MinTier  uint16 // the shortest retention the creator accepts
	Deadline uint64 // height by which the upload must be in a block
	Board    string // where the request is listed
	Note     string // what the data is, as shown to backers
	Sig      []byte
}

const (
	MaxFundNote      = 300 // bytes
	MaxRequestsPerTx = 4
	FundSamples      = 8 // sub-chunks a funded upload must prove
)

// FundSample is one sub-chunk of funded data with its path to the root.
type FundSample struct {
	Sub  []byte
	Path chunk.Proof
}

func (r *FundRequest) encodeBody(w *codec.Writer) {
	w.Fixed(r.Creator)
	w.Fixed(r.DataRoot[:])
	w.Var(r.Size)
	w.U8(r.Media)
	w.U16(r.MinTier)
	w.Var(r.Deadline)
	w.Bytes([]byte(r.Board))
	w.Bytes([]byte(r.Note))
}

// ID names the request; pledges point at it.
func (r *FundRequest) ID() Hash {
	var w codec.Writer
	r.encodeBody(&w)
	return cryptox.Tagged("cb/fund", w.B)
}

// CreatorAddr is the creator's address.
func (r *FundRequest) CreatorAddr() Address { return cryptox.AddressOf(r.Creator) }

// Sign fills in Creator and Sig.
func (r *FundRequest) Sign(k *cryptox.Key) {
	r.Creator = append([]byte(nil), k.Pub...)
	id := r.ID()
	r.Sig = k.Sign(id[:])
}

// Check validates a request on its own.
func (r *FundRequest) Check() error {
	if len(r.Creator) != ed25519.PublicKeySize || len(r.Sig) != ed25519.SignatureSize {
		return errors.New("funding request: bad key or signature length")
	}
	if r.Size == 0 || r.DataRoot.IsZero() {
		return errors.New("funding request names no data")
	}
	if _, _, ok := params.TierMonths(r.MinTier); !ok {
		return errors.New("funding request: bad minimum tier")
	}
	if len(r.Board) == 0 || len(r.Board) > params.MaxBoardBytes || !utf8.ValidString(r.Board) {
		return errors.New("funding request: bad board")
	}
	if len(r.Note) > MaxFundNote || !utf8.ValidString(r.Note) {
		return fmt.Errorf("funding request: note must be UTF-8 of at most %d bytes", MaxFundNote)
	}
	id := r.ID()
	if !cryptox.Verify(r.Creator, id[:], r.Sig) {
		return errors.New("funding request: bad signature")
	}
	return nil
}

// Encode is the request with its signature.
func (r *FundRequest) Encode() []byte {
	var w codec.Writer
	r.encodeBody(&w)
	w.Fixed(r.Sig)
	return w.B
}

func decodeRequest(rd *codec.Reader) *FundRequest {
	r := &FundRequest{Creator: make([]byte, ed25519.PublicKeySize), Sig: make([]byte, ed25519.SignatureSize)}
	rd.Fixed(r.Creator)
	rd.Fixed(r.DataRoot[:])
	r.Size = rd.Var()
	r.Media = rd.U8()
	r.MinTier = rd.U16()
	r.Deadline = rd.Var()
	r.Board = string(rd.Bytes(params.MaxBoardBytes))
	r.Note = string(rd.Bytes(MaxFundNote))
	rd.Fixed(r.Sig)
	return r
}

// DecodeFundRequest parses an encoded request (as creators share it).
func DecodeFundRequest(b []byte) (*FundRequest, error) {
	rd := codec.NewReader(b)
	r := decodeRequest(rd)
	if err := rd.Done(); err != nil {
		return nil, err
	}
	return r, nil
}

func encodeSamples(w *codec.Writer, ss []FundSample) {
	w.Var(uint64(len(ss)))
	for _, s := range ss {
		w.Bytes(s.Sub)
		chunk.EncodeProof(w, s.Path)
	}
}

func decodeSamples(r *codec.Reader) []FundSample {
	n := r.Count(FundSamples)
	out := make([]FundSample, 0, n)
	for i := 0; i < n && r.Err == nil; i++ {
		var s FundSample
		s.Sub = r.Bytes(params.SubChunkSize)
		s.Path = chunk.DecodeProof(r)
		out = append(out, s)
	}
	return out
}

// PoWBits is the work of a funding request's gossip nonce: the leading
// zero bits of SHA-256(tag || id || nonce). Requests are gossiped before
// any pledge carries them on chain, so this work is what makes a flood of
// them cost something.
func PoWBits(id Hash, nonce uint64) int {
	var buf [32 + 32 + 8]byte
	copy(buf[:32], powTag[:])
	copy(buf[32:64], id[:])
	binary.BigEndian.PutUint64(buf[64:], nonce)
	h := sha256.Sum256(buf[:])
	n := 0
	for _, b := range h {
		if b != 0 {
			return n + bits.LeadingZeros8(b)
		}
		n += 8
	}
	return n
}

var powTag = sha256.Sum256([]byte("cb/fund/pow"))

// SolvePoW finds a nonce of at least want bits, on all cores. It stops
// early (returning false) when stop is closed.
func SolvePoW(id Hash, want int, stop <-chan struct{}) (uint64, bool) {
	workers := max(1, runtime.NumCPU())
	found := make(chan uint64, workers)
	done := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(done) }) }
	defer finish()
	var start [8]byte
	_, _ = rand.Read(start[:])
	base := binary.BigEndian.Uint64(start[:])
	for w := 0; w < workers; w++ {
		go func(n uint64) {
			for i := 0; ; i++ {
				if i&0xffff == 0 {
					select {
					case <-done:
						return
					default:
					}
				}
				if PoWBits(id, n) >= want {
					found <- n
					finish()
					return
				}
				n += uint64(workers)
			}
		}(base + uint64(w))
	}
	select {
	case n := <-found:
		return n, true
	case <-stop:
		return 0, false
	}
}
