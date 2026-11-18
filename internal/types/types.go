// Package types defines transactions and blocks and their canonical encoding.
package types

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"math/big"
	"unicode/utf8"

	"github.com/namsaeg/chainboard/internal/chunk"
	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/merkle"
	"github.com/namsaeg/chainboard/internal/params"
)

type (
	Hash    = cryptox.Hash
	Address = cryptox.Address
)

// TxKind is the type of a transaction.
type TxKind uint8

const (
	TxTransfer TxKind = iota + 1
	TxPost            // a post or a comment (Parent set)
	TxUpload          // registers a data root (media) with a retention tier
	TxExtend          // anyone extends a data root to a longer tier
	TxStake           // locks coins for the free lane
	TxUnstake         // starts unbonding
	TxRegister        // declares the partitions a storage node keeps
	TxProof           // epoch storage proof
	TxLike
)

func (k TxKind) String() string {
	return [...]string{"?", "transfer", "post", "upload", "extend", "stake", "unstake", "register", "proof", "like"}[min(int(k), 9)]
}

// Lane selects how a text transaction pays.
type Lane uint8

const (
	LaneFee  Lane = 0 // pays base fee (burned) + storage + tip
	LaneFree Lane = 1 // spends stake quota; storage paid from issuance
)

// Media classes accepted by node policy (consensus only records the class).
const (
	MediaImage uint8 = 1
	MediaGIF   uint8 = 2
	MediaOther uint8 = 3
)

// StorageProof proves one packed sub-chunk for an epoch challenge.
type StorageProof struct {
	Epoch     uint64
	Partition uint64
	Position  uint64
	Sub       uint8
	PackedSub []byte
	Path      chunk.Proof
}

// Tx is a signed transaction. Only the fields of its Kind are encoded.
type Tx struct {
	Kind   TxKind
	From   []byte // Ed25519 public key
	Nonce  uint64
	Lane   Lane
	MaxFee uint64 // most the sender pays in total (fee lane)
	Tip    uint64

	To     Address // transfer
	Amount uint64  // transfer, stake, unstake

	Board  string // post
	Text   []byte
	Parent Hash   // post: zero for a top-level post, else the post commented on
	Attach []Hash // post: data roots

	DataRoot Hash // upload, extend
	Size     uint64
	Tier     uint16
	Media    uint8

	Target Hash // like

	Partitions []uint64 // register

	Proof *StorageProof

	Sig []byte
}

const maxPartitions = 4096

func (t *Tx) encodeBody(w *codec.Writer) {
	w.U8(uint8(t.Kind))
	w.Fixed(t.From)
	w.Var(t.Nonce)
	w.U8(uint8(t.Lane))
	w.Var(t.MaxFee)
	w.Var(t.Tip)
	switch t.Kind {
	case TxTransfer:
		w.Fixed(t.To[:])
		w.Var(t.Amount)
	case TxStake, TxUnstake:
		w.Var(t.Amount)
	case TxPost:
		w.Bytes([]byte(t.Board))
		w.Bytes(t.Text)
		w.Fixed(t.Parent[:])
		w.Var(uint64(len(t.Attach)))
		for _, a := range t.Attach {
			w.Fixed(a[:])
		}
	case TxUpload:
		w.Fixed(t.DataRoot[:])
		w.Var(t.Size)
		w.U16(t.Tier)
		w.U8(t.Media)
	case TxExtend:
		w.Fixed(t.DataRoot[:])
		w.U16(t.Tier)
	case TxLike:
		w.Fixed(t.Target[:])
	case TxRegister:
		w.Var(uint64(len(t.Partitions)))
		for _, p := range t.Partitions {
			w.U64(p)
		}
	case TxProof:
		p := t.Proof
		w.Var(p.Epoch)
		w.U64(p.Partition)
		w.Var(p.Position)
		w.U8(p.Sub)
		w.Bytes(p.PackedSub)
		chunk.EncodeProof(w, p.Path)
	}
}

// SigHash is what the sender signs.
func (t *Tx) SigHash() Hash {
	var w codec.Writer
	t.encodeBody(&w)
	return cryptox.Tagged("cb/tx/sig", w.B)
}

// Encode returns the full encoding including the signature.
func (t *Tx) Encode() []byte {
	var w codec.Writer
	t.encodeBody(&w)
	w.Fixed(t.Sig)
	return w.B
}

// ID is the transaction hash.
func (t *Tx) ID() Hash { return cryptox.Tagged("cb/tx", t.Encode()) }

// Sender is the address of From.
func (t *Tx) Sender() Address { return cryptox.AddressOf(t.From) }

// EncodedSize is the encoded size in bytes.
func (t *Tx) EncodedSize() int { return len(t.Encode()) }

// Sign fills in From and Sig.
func (t *Tx) Sign(k *cryptox.Key) {
	t.From = append([]byte(nil), k.Pub...)
	h := t.SigHash()
	t.Sig = k.Sign(h[:])
}

func decodeTx(r *codec.Reader) (*Tx, error) {
	t := &Tx{}
	t.Kind = TxKind(r.U8())
	t.From = make([]byte, ed25519.PublicKeySize)
	r.Fixed(t.From)
	t.Nonce = r.Var()
	t.Lane = Lane(r.U8())
	t.MaxFee = r.Var()
	t.Tip = r.Var()
	switch t.Kind {
	case TxTransfer:
		r.Fixed(t.To[:])
		t.Amount = r.Var()
	case TxStake, TxUnstake:
		t.Amount = r.Var()
	case TxPost:
		t.Board = string(r.Bytes(params.MaxBoardBytes))
		t.Text = r.Bytes(params.MaxPostBytes)
		r.Fixed(t.Parent[:])
		n := r.Count(params.MaxAttachments)
		for i := 0; i < n; i++ {
			var h Hash
			r.Fixed(h[:])
			t.Attach = append(t.Attach, h)
		}
	case TxUpload:
		r.Fixed(t.DataRoot[:])
		t.Size = r.Var()
		t.Tier = r.U16()
		t.Media = r.U8()
	case TxExtend:
		r.Fixed(t.DataRoot[:])
		t.Tier = r.U16()
	case TxLike:
		r.Fixed(t.Target[:])
	case TxRegister:
		n := r.Count(maxPartitions)
		for i := 0; i < n; i++ {
			t.Partitions = append(t.Partitions, r.U64())
		}
	case TxProof:
		p := &StorageProof{}
		p.Epoch = r.Var()
		p.Partition = r.U64()
		p.Position = r.Var()
		p.Sub = r.U8()
		p.PackedSub = r.Bytes(params.SubChunkSize)
		p.Path = chunk.DecodeProof(r)
		t.Proof = p
	default:
		return nil, fmt.Errorf("unknown tx kind %d", t.Kind)
	}
	t.Sig = make([]byte, ed25519.SignatureSize)
	r.Fixed(t.Sig)
	return t, r.Err
}

// DecodeTx decodes one transaction, rejecting trailing bytes.
func DecodeTx(b []byte) (*Tx, error) {
	r := codec.NewReader(b)
	t, err := decodeTx(r)
	if err != nil {
		return nil, err
	}
	return t, r.Done()
}

// CheckStateless validates everything that does not need chain state.
func (t *Tx) CheckStateless() error {
	if len(t.From) != ed25519.PublicKeySize || len(t.Sig) != ed25519.SignatureSize {
		return errors.New("bad key or signature length")
	}
	if t.Lane > LaneFree {
		return errors.New("bad lane")
	}
	switch t.Kind {
	case TxPost:
		if !utf8.Valid(t.Text) || !utf8.ValidString(t.Board) {
			return errors.New("post text and board must be UTF-8")
		}
		if len(t.Board) == 0 {
			return errors.New("post needs a board")
		}
		if len(t.Text) == 0 && len(t.Attach) == 0 {
			return errors.New("empty post")
		}
		if !t.Parent.IsZero() && len(t.Attach) > 0 {
			return errors.New("comments cannot carry attachments")
		}
	case TxLike:
	case TxUpload:
		if t.Size == 0 {
			return errors.New("empty upload")
		}
		if _, _, ok := params.TierMonths(t.Tier); !ok {
			return errors.New("bad tier")
		}
		if t.Lane != LaneFee {
			return errors.New("uploads use the fee lane")
		}
	case TxExtend:
		if _, _, ok := params.TierMonths(t.Tier); !ok {
			return errors.New("bad tier")
		}
		if t.Lane != LaneFee {
			return errors.New("extends use the fee lane")
		}
	case TxTransfer, TxStake, TxUnstake, TxRegister, TxProof:
		if t.Lane != LaneFee {
			return errors.New("only text uses the free lane")
		}
		if t.Kind == TxProof && (t.Proof == nil || len(t.Proof.PackedSub) != params.SubChunkSize) {
			return errors.New("bad proof")
		}
	default:
		return errors.New("unknown kind")
	}
	h := t.SigHash()
	if !cryptox.Verify(t.From, h[:], t.Sig) {
		return errors.New("bad signature")
	}
	return nil
}

// IsText reports whether the tx counts against text lane capacity.
func (t *Tx) IsText() bool { return t.Kind == TxPost || t.Kind == TxLike }

// Solution is the storage proof that wins a block.
type Solution struct {
	Partition uint64 // bucket<<32 | index
	K         uint8  // which challenge of the step
	Position  uint64 // chunk position inside the bucket
	Sub       uint8
	PackedSub []byte
	Path      chunk.Proof
}

// Header is a block header.
type Header struct {
	Version    uint32
	Height     uint64
	Prev       Hash
	Time       int64
	Target     Hash // big-endian
	TxRoot     Hash
	BoardRoot  Hash
	VDFStep    uint64 // global index of the last VDF output
	VDFOutputs []Hash // outputs since the parent; the last one seeds the challenge
	Sol        Solution
	Miner      []byte // Ed25519 public key
	Sig        []byte
}

const maxVDFOutputs = 100_000

func (h *Header) encodeBody(w *codec.Writer) {
	w.U32(h.Version)
	w.Var(h.Height)
	w.Fixed(h.Prev[:])
	w.U64(uint64(h.Time))
	w.Fixed(h.Target[:])
	w.Fixed(h.TxRoot[:])
	w.Fixed(h.BoardRoot[:])
	w.Var(h.VDFStep)
	w.Var(uint64(len(h.VDFOutputs)))
	for _, o := range h.VDFOutputs {
		w.Fixed(o[:])
	}
	w.U64(h.Sol.Partition)
	w.U8(h.Sol.K)
	w.Var(h.Sol.Position)
	w.U8(h.Sol.Sub)
	w.Bytes(h.Sol.PackedSub)
	chunk.EncodeProof(w, h.Sol.Path)
	w.Fixed(h.Miner)
}

// Hash is the block id (it commits to the signature too).
func (h *Header) Hash() Hash {
	var w codec.Writer
	h.encodeBody(&w)
	w.Fixed(h.Sig)
	return cryptox.Tagged("cb/block", w.B)
}

// SigHash is what the miner signs.
func (h *Header) SigHash() Hash {
	var w codec.Writer
	h.encodeBody(&w)
	return cryptox.Tagged("cb/block/sig", w.B)
}

func (h *Header) TargetInt() *big.Int { return new(big.Int).SetBytes(h.Target[:]) }

// MinerAddr is the miner's address.
func (h *Header) MinerAddr() Address { return cryptox.AddressOf(h.Miner) }

// Block is a header and its transactions.
type Block struct {
	Header
	Txs []*Tx
}

// Encode serialises a whole block.
func (b *Block) Encode() []byte {
	var w codec.Writer
	b.encodeBody(&w)
	w.Fixed(b.Sig)
	w.Var(uint64(len(b.Txs)))
	for _, t := range b.Txs {
		w.Bytes(t.Encode())
	}
	return w.B
}

const maxBlockTxs = 50_000

// DecodeBlock parses a block.
func DecodeBlock(b []byte) (*Block, error) {
	r := codec.NewReader(b)
	blk := &Block{}
	h := &blk.Header
	h.Version = r.U32()
	h.Height = r.Var()
	r.Fixed(h.Prev[:])
	h.Time = int64(r.U64())
	r.Fixed(h.Target[:])
	r.Fixed(h.TxRoot[:])
	r.Fixed(h.BoardRoot[:])
	h.VDFStep = r.Var()
	n := r.Count(maxVDFOutputs)
	for i := 0; i < n; i++ {
		var o Hash
		r.Fixed(o[:])
		h.VDFOutputs = append(h.VDFOutputs, o)
	}
	h.Sol.Partition = r.U64()
	h.Sol.K = r.U8()
	h.Sol.Position = r.Var()
	h.Sol.Sub = r.U8()
	h.Sol.PackedSub = r.Bytes(params.SubChunkSize)
	h.Sol.Path = chunk.DecodeProof(r)
	h.Miner = make([]byte, ed25519.PublicKeySize)
	r.Fixed(h.Miner)
	h.Sig = make([]byte, ed25519.SignatureSize)
	r.Fixed(h.Sig)
	nt := r.Count(maxBlockTxs)
	for i := 0; i < nt && r.Err == nil; i++ {
		tb := r.Bytes(1 << 20)
		if r.Err != nil {
			break
		}
		t, err := DecodeTx(tb)
		if err != nil {
			return nil, err
		}
		blk.Txs = append(blk.Txs, t)
	}
	if err := r.Done(); err != nil {
		return nil, err
	}
	return blk, nil
}

// TxRoot is the Merkle root of transaction ids.
func TxRoot(txs []*Tx) Hash {
	leaves := make([]Hash, len(txs))
	for i, t := range txs {
		id := t.ID()
		leaves[i] = merkle.Leaf(id[:])
	}
	return merkle.Root(leaves)
}

// BoardRoot commits to which posts each board received in this block, so a
// light client can prove that a node did not hide a post. Leaves are
// (board, post id) in transaction order.
func BoardRoot(txs []*Tx) Hash {
	var leaves []Hash
	for _, t := range txs {
		if t.Kind == TxPost {
			id := t.ID()
			leaves = append(leaves, merkle.Leaf(append([]byte(t.Board+"\x00"), id[:]...)))
		}
	}
	return merkle.Root(leaves)
}

// Work is the expected number of challenge checks a target represents.
func Work(target *big.Int) *big.Int {
	if target.Sign() <= 0 {
		return big.NewInt(0)
	}
	max := new(big.Int).Lsh(big.NewInt(1), 256)
	return max.Div(max, new(big.Int).Add(target, big.NewInt(1)))
}

// HashToTarget converts a big-endian 256-bit integer to a Hash.
func HashToTarget(v *big.Int) Hash {
	var h Hash
	v.FillBytes(h[:])
	return h
}
