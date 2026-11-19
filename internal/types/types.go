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
	TxTransfer   TxKind = iota + 1
	TxPost              // a post or a comment (Parent set)
	TxUpload            // registers a data root (media) with a retention tier
	TxExtend            // anyone extends a data root to a longer tier
	TxStake             // locks coins for the free lane
	TxUnstake           // starts unbonding
	TxRegister          // declares the partitions a storage node keeps
	TxProof             // epoch storage proof
	TxLikes             // a batch of likes (and pledges to funding requests)
	TxAnonPost          // unsigned post of the anonymous lane, carrying an RLN proof
	TxRLNJoin           // moves mature stake into an RLN membership bond
	TxRLNLeave          // starts the exit of a membership (bond returns after unbonding)
	TxRLNSlash          // unsigned: reveals a member's secret recovered from a double signal
	TxFundUpload        // stores funded data, paid from its request's escrow (anyone may send)
	TxFundProof         // proves the funded data's sampled sub-chunks, paid from the escrow
)

var kindNames = [...]string{"?", "transfer", "post", "upload", "extend", "stake", "unstake", "register", "proof", "likes",
	"anonpost", "rlnjoin", "rlnleave", "rlnslash", "fundupload", "fundproof"}

func (k TxKind) String() string { return kindNames[min(int(k), len(kindNames)-1)] }

// Signed reports whether a kind is signed by an account. Anonymous posts and
// slashes have no sender, nonce, fee or signature.
func (k TxKind) Signed() bool { return k != TxAnonPost && k != TxRLNSlash }

// Lane selects how a text transaction pays.
type Lane uint8

const (
	LaneFee  Lane = 0 // pays base fee (burned) + storage + tip
	LaneAnon Lane = 1 // anonymous RLN posts; storage paid from issuance
)

// LaneOf is the lane a transaction uses.
func (t *Tx) LaneOf() Lane {
	if t.Kind == TxAnonPost {
		return LaneAnon
	}
	return LaneFee
}

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

// RLNProof is the rate-limit proof of an anonymous post. Root, Nullifier and
// Y are BN254 scalars in canonical big-endian form.
type RLNProof struct {
	Root      Hash
	Epoch     uint64
	Nullifier Hash
	Y         Hash
	Proof     []byte
}

// LikeRef names a post by the height of its block and the first bytes of
// its id: enough to find it there, short enough to batch many. Likes are not
// part of the chain state: nodes resolve and count them in their index.
type LikeRef struct {
	Height uint64
	Prefix [4]byte
}

// RefOf is the like reference of a post placed at a height.
func RefOf(post Hash, height uint64) LikeRef {
	r := LikeRef{Height: height}
	copy(r.Prefix[:], post[:4])
	return r
}

// Matches reports whether a post at a height is the one a reference names.
func (r LikeRef) Matches(post Hash, height uint64) bool {
	return r.Height == height && [4]byte(post[:4]) == r.Prefix
}

// Pledge backs a funding request with coins; it also counts as a like.
type Pledge struct {
	Request Hash
	Amount  uint64
}

// MaxLikes bounds the entries (likes plus pledges) of one batch.
const MaxLikes = 256

// MaxRLNProof bounds the encoded Groth16 proof.
const MaxRLNProof = 256

// Tx is a transaction. Only the fields of its Kind are encoded.
type Tx struct {
	Kind   TxKind
	From   []byte // Ed25519 public key
	Nonce  uint64
	Lane   Lane
	MaxFee uint64 // most the sender pays in total (fee lane)
	Tip    uint64

	To     Address // transfer
	Amount uint64  // transfer, stake, unstake

	Board    string // post
	TextHash Hash   // post, extend of a post: hash of the body (zero: no text)
	TextLen  uint16 // post, extend of a post: body length in bytes
	Parent   Hash   // post: zero for a top-level post, else the post commented on
	Attach   []Hash // post: data roots

	DataRoot Hash // upload, extend of data
	Size     uint64
	Tier     uint16 // upload, extend, post (retention of the body)
	Media    uint8

	Target Hash // extend of a post (the post id)

	Likes    []LikeRef      // likes
	Pledges  []Pledge       // likes
	Requests []*FundRequest // likes: funding requests carried on chain with their first pledge
	Samples  []FundSample   // fundproof

	// Text is the body of a post. It is not part of the encoding, ID or
	// signature, which commit to TextHash: it travels next to the
	// transaction on the network and is stored off-chain, bundled into the
	// weave with the retention tier the post paid for.
	Text []byte

	Partitions []uint64 // register

	Proof *StorageProof

	Commitment Hash   // rlnjoin, rlnleave: identity commitment
	Limit      uint16 // rlnjoin: messages per epoch
	RLN        *RLNProof
	PseudoPub  []byte // anonpost, optional: Ed25519 key of a pseudonym
	PseudoSig  []byte
	Secret     Hash // rlnslash: the revealed identity secret

	Sig []byte
}

const maxPartitions = 4096

func (t *Tx) encodeBody(w *codec.Writer) {
	w.U8(uint8(t.Kind))
	if t.Kind.Signed() {
		w.Fixed(t.From)
		w.Var(t.Nonce)
		w.U8(uint8(t.Lane))
		w.Var(t.MaxFee)
		w.Var(t.Tip)
	}
	switch t.Kind {
	case TxTransfer:
		w.Fixed(t.To[:])
		w.Var(t.Amount)
	case TxStake, TxUnstake:
		w.Var(t.Amount)
	case TxPost:
		w.Bytes([]byte(t.Board))
		w.Fixed(t.TextHash[:])
		w.U16(t.TextLen)
		w.U16(t.Tier)
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
		w.Fixed(t.Target[:])
		w.Fixed(t.TextHash[:])
		w.U16(t.TextLen)
	case TxLikes:
		w.Var(uint64(len(t.Likes)))
		for _, l := range t.Likes {
			w.Var(l.Height)
			w.Fixed(l.Prefix[:])
		}
		w.Var(uint64(len(t.Pledges)))
		for _, p := range t.Pledges {
			w.Fixed(p.Request[:])
			w.Var(p.Amount)
		}
		w.Var(uint64(len(t.Requests)))
		for _, r := range t.Requests {
			r.encodeBody(w)
			w.Fixed(r.Sig)
		}
	case TxFundUpload:
		w.Fixed(t.Target[:])
	case TxFundProof:
		w.Fixed(t.Target[:])
		encodeSamples(w, t.Samples)
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
	case TxAnonPost:
		t.encodeAnonContent(w)
		r := t.RLN
		w.Fixed(r.Root[:])
		w.Var(r.Epoch)
		w.Fixed(r.Nullifier[:])
		w.Fixed(r.Y[:])
		w.Bytes(r.Proof)
		w.Bool(len(t.PseudoPub) > 0)
		if len(t.PseudoPub) > 0 {
			w.Fixed(t.PseudoSig)
		}
	case TxRLNJoin:
		w.Var(t.Amount)
		w.Fixed(t.Commitment[:])
		w.U16(t.Limit)
	case TxRLNLeave:
		w.Fixed(t.Commitment[:])
	case TxRLNSlash:
		w.Fixed(t.Secret[:])
	}
}

// encodeAnonContent is what an anonymous post says: board, text (by hash),
// tier, parent and the optional pseudonym key.
func (t *Tx) encodeAnonContent(w *codec.Writer) {
	w.Bytes([]byte(t.Board))
	w.Fixed(t.TextHash[:])
	w.U16(t.TextLen)
	w.U16(t.Tier)
	w.Fixed(t.Parent[:])
	if len(t.PseudoPub) > 0 {
		w.Fixed(t.PseudoPub)
	} else {
		w.Fixed(make([]byte, ed25519.PublicKeySize))
	}
}

// Signal is the RLN signal of an anonymous post: the hash of its content.
// Two posts with the same nullifier and different signals reveal the secret.
func (t *Tx) Signal() Hash {
	var w codec.Writer
	t.encodeAnonContent(&w)
	return cryptox.Tagged("cb/anon/signal", w.B)
}

// PseudoHash is what a pseudonym key signs.
func (t *Tx) PseudoHash() Hash {
	sig := t.Signal()
	return cryptox.Tagged("cb/anon/pseudo", sig[:])
}

// Author is the address shown for a post: the sender, the pseudonym, or zero
// for a fully anonymous post.
func (t *Tx) Author() Address {
	switch {
	case t.Kind.Signed():
		return t.Sender()
	case len(t.PseudoPub) == ed25519.PublicKeySize:
		return cryptox.AddressOf(t.PseudoPub)
	}
	return Address{}
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
	if t.Kind.Signed() {
		w.Fixed(t.Sig)
	}
	return w.B
}

// ID is the transaction hash.
func (t *Tx) ID() Hash { return cryptox.Tagged("cb/tx", t.Encode()) }

// Sender is the address of From (zero for unsigned kinds).
func (t *Tx) Sender() Address {
	if !t.Kind.Signed() {
		return Address{}
	}
	return cryptox.AddressOf(t.From)
}

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
	if t.Kind.Signed() {
		t.From = make([]byte, ed25519.PublicKeySize)
		r.Fixed(t.From)
		t.Nonce = r.Var()
		t.Lane = Lane(r.U8())
		t.MaxFee = r.Var()
		t.Tip = r.Var()
	}
	switch t.Kind {
	case TxTransfer:
		r.Fixed(t.To[:])
		t.Amount = r.Var()
	case TxStake, TxUnstake:
		t.Amount = r.Var()
	case TxPost:
		t.Board = string(r.Bytes(params.MaxBoardBytes))
		r.Fixed(t.TextHash[:])
		t.TextLen = r.U16()
		t.Tier = r.U16()
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
		r.Fixed(t.Target[:])
		r.Fixed(t.TextHash[:])
		t.TextLen = r.U16()
	case TxLikes:
		n := r.Count(MaxLikes)
		for i := 0; i < n && r.Err == nil; i++ {
			var l LikeRef
			l.Height = r.Var()
			r.Fixed(l.Prefix[:])
			t.Likes = append(t.Likes, l)
		}
		n = r.Count(MaxLikes)
		for i := 0; i < n && r.Err == nil; i++ {
			var p Pledge
			r.Fixed(p.Request[:])
			p.Amount = r.Var()
			t.Pledges = append(t.Pledges, p)
		}
		n = r.Count(MaxRequestsPerTx)
		for i := 0; i < n && r.Err == nil; i++ {
			t.Requests = append(t.Requests, decodeRequest(r))
		}
	case TxFundUpload:
		r.Fixed(t.Target[:])
	case TxFundProof:
		r.Fixed(t.Target[:])
		t.Samples = decodeSamples(r)
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
	case TxAnonPost:
		t.Board = string(r.Bytes(params.MaxBoardBytes))
		r.Fixed(t.TextHash[:])
		t.TextLen = r.U16()
		t.Tier = r.U16()
		r.Fixed(t.Parent[:])
		pub := make([]byte, ed25519.PublicKeySize)
		r.Fixed(pub)
		p := &RLNProof{}
		r.Fixed(p.Root[:])
		p.Epoch = r.Var()
		r.Fixed(p.Nullifier[:])
		r.Fixed(p.Y[:])
		p.Proof = r.Bytes(MaxRLNProof)
		t.RLN = p
		if r.Bool() {
			t.PseudoPub = pub
			t.PseudoSig = make([]byte, ed25519.SignatureSize)
			r.Fixed(t.PseudoSig)
		} else if r.Err == nil && !allZero(pub) {
			return nil, errors.New("pseudonym key without signature")
		}
	case TxRLNJoin:
		t.Amount = r.Var()
		r.Fixed(t.Commitment[:])
		t.Limit = r.U16()
	case TxRLNLeave:
		r.Fixed(t.Commitment[:])
	case TxRLNSlash:
		r.Fixed(t.Secret[:])
	default:
		return nil, fmt.Errorf("unknown tx kind %d", t.Kind)
	}
	if t.Kind.Signed() {
		t.Sig = make([]byte, ed25519.SignatureSize)
		r.Fixed(t.Sig)
	}
	return t, r.Err
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
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

// TextHashOf is the commitment a post makes to its body.
func TextHashOf(body []byte) Hash { return cryptox.Tagged("cb/text", body) }

// SetText attaches a body and commits to it.
func (t *Tx) SetText(body []byte) {
	t.Text = append([]byte(nil), body...)
	t.TextLen = uint16(len(body))
	t.TextHash = Hash{}
	if len(body) > 0 {
		t.TextHash = TextHashOf(body)
	}
}

// NeedsBody reports whether the transaction commits to a body that must
// travel with it: a post with text, or the extension of a post.
func (t *Tx) NeedsBody() bool {
	switch t.Kind {
	case TxPost, TxAnonPost:
		return t.TextLen > 0
	case TxExtend:
		return !t.Target.IsZero()
	}
	return false
}

// CheckBody verifies the attached body against the commitment.
func (t *Tx) CheckBody() error {
	if len(t.Text) != int(t.TextLen) {
		return errors.New("text body has the wrong length")
	}
	if len(t.Text) > 0 && TextHashOf(t.Text) != t.TextHash {
		return errors.New("text body does not match its hash")
	}
	if !utf8.Valid(t.Text) {
		return errors.New("post text must be UTF-8")
	}
	return nil
}

func checkTextCommitment(t *Tx) error {
	if t.TextLen > params.MaxPostBytes {
		return fmt.Errorf("text longer than %d bytes", params.MaxPostBytes)
	}
	if (t.TextLen == 0) != t.TextHash.IsZero() {
		return errors.New("text hash and length disagree")
	}
	if t.Text != nil {
		return t.CheckBody()
	}
	return nil
}

func checkPostContent(t *Tx) error {
	if !utf8.ValidString(t.Board) {
		return errors.New("board must be UTF-8")
	}
	if len(t.Board) == 0 {
		return errors.New("post needs a board")
	}
	if err := checkTextCommitment(t); err != nil {
		return err
	}
	if t.TextLen == 0 {
		if len(t.Attach) == 0 {
			return errors.New("empty post")
		}
		if t.Tier != 0 {
			return errors.New("retention tier without text")
		}
	} else if _, _, ok := params.TierMonths(t.Tier); !ok {
		return errors.New("bad text retention tier")
	}
	if !t.Parent.IsZero() && len(t.Attach) > 0 {
		return errors.New("comments cannot carry attachments")
	}
	return nil
}

// LaneSize is what a transaction takes from its lane: its encoding plus its
// body, which nodes relay and the weave stores.
func (t *Tx) LaneSize() int { return t.EncodedSize() + int(t.TextLen) }

// maxTxBytes bounds one encoded transaction (proofs are the largest).
const maxTxBytes = 1 << 20

// EncodeWire is the network form: the encoding and then the body.
func (t *Tx) EncodeWire() []byte {
	var w codec.Writer
	w.Bytes(t.Encode())
	w.Bytes(t.Text)
	return w.B
}

// DecodeWire parses the network form and checks the body.
func DecodeWire(b []byte) (*Tx, error) {
	r := codec.NewReader(b)
	raw := r.Bytes(maxTxBytes)
	body := r.Bytes(params.MaxPostBytes)
	if err := r.Done(); err != nil {
		return nil, err
	}
	t, err := DecodeTx(raw)
	if err != nil {
		return nil, err
	}
	if len(body) > 0 || t.NeedsBody() {
		t.Text = body
		if t.Text == nil {
			t.Text = []byte{}
		}
		if err := t.CheckBody(); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// CheckStateless validates everything that does not need chain state.
// RLN proofs are verified by the state, which knows the accepted roots.
func (t *Tx) CheckStateless() error {
	switch t.Kind {
	case TxAnonPost:
		if t.RLN == nil || len(t.RLN.Proof) == 0 || len(t.RLN.Proof) > MaxRLNProof {
			return errors.New("anonymous post needs an RLN proof")
		}
		if len(t.Attach) > 0 || t.TextLen == 0 {
			return errors.New("anonymous posts are text only")
		}
		if t.Tier != params.TierFree {
			return errors.New("anonymous posts are kept for one month")
		}
		if err := checkPostContent(t); err != nil {
			return err
		}
		if len(t.PseudoPub) > 0 {
			h := t.PseudoHash()
			if len(t.PseudoPub) != ed25519.PublicKeySize || len(t.PseudoSig) != ed25519.SignatureSize ||
				!cryptox.Verify(t.PseudoPub, h[:], t.PseudoSig) {
				return errors.New("bad pseudonym signature")
			}
		}
		return nil
	case TxRLNSlash:
		return nil
	}
	if len(t.From) != ed25519.PublicKeySize || len(t.Sig) != ed25519.SignatureSize {
		return errors.New("bad key or signature length")
	}
	if t.Lane != LaneFee {
		return errors.New("signed transactions use the fee lane")
	}
	switch t.Kind {
	case TxPost:
		if err := checkPostContent(t); err != nil {
			return err
		}
	case TxLikes:
		if err := checkLikes(t); err != nil {
			return err
		}
	case TxUpload:
		if t.Size == 0 {
			return errors.New("empty upload")
		}
		if _, _, ok := params.TierMonths(t.Tier); !ok {
			return errors.New("bad tier")
		}
	case TxExtend:
		if _, _, ok := params.TierMonths(t.Tier); !ok {
			return errors.New("bad tier")
		}
		if t.DataRoot.IsZero() == t.Target.IsZero() {
			return errors.New("extend names either a data root or a post")
		}
		if t.Target.IsZero() && (t.TextLen != 0 || !t.TextHash.IsZero()) {
			return errors.New("extend of data carries no text")
		}
		if !t.Target.IsZero() {
			if t.TextLen == 0 {
				return errors.New("extend of a post without text")
			}
			if err := checkTextCommitment(t); err != nil {
				return err
			}
		}
	case TxRLNJoin:
		if t.Limit == 0 || t.Amount == 0 {
			return errors.New("membership needs a limit and a bond")
		}
	case TxFundUpload:
		if t.Target.IsZero() {
			return errors.New("funded upload names no request")
		}
	case TxFundProof:
		if t.Target.IsZero() || len(t.Samples) != FundSamples {
			return fmt.Errorf("funding proof needs %d samples", FundSamples)
		}
		for _, x := range t.Samples {
			if len(x.Sub) == 0 || len(x.Sub) > params.SubChunkSize {
				return errors.New("funding proof: bad sample")
			}
		}
	case TxTransfer, TxStake, TxUnstake, TxRegister, TxRLNLeave:
	case TxProof:
		if t.Proof == nil || len(t.Proof.PackedSub) != params.SubChunkSize {
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

func checkLikes(t *Tx) error {
	n := len(t.Likes) + len(t.Pledges)
	if n == 0 || n > MaxLikes {
		return fmt.Errorf("a like batch carries 1 to %d entries", MaxLikes)
	}
	seen := make(map[LikeRef]bool, len(t.Likes))
	for _, l := range t.Likes {
		if seen[l] {
			return errors.New("the same post is liked twice in one batch")
		}
		seen[l] = true
	}
	pledged := make(map[Hash]bool, len(t.Pledges))
	for _, p := range t.Pledges {
		if p.Amount == 0 || pledged[p.Request] {
			return errors.New("pledges need an amount and distinct requests")
		}
		pledged[p.Request] = true
	}
	for _, r := range t.Requests {
		if err := r.Check(); err != nil {
			return err
		}
		if !pledged[r.ID()] {
			return errors.New("a carried funding request needs a pledge in the same batch")
		}
		pledged[r.ID()] = false // carried once
	}
	return nil
}

// IsPost reports whether the tx creates a post.
func (t *Tx) IsPost() bool { return t.Kind == TxPost || t.Kind == TxAnonPost }

// Solution is the storage proof that wins a block.
type Solution struct {
	Partition uint64 // bucket<<32 | index
	Position  uint64 // chunk position inside the bucket (in the step's recall range)
	Sub       uint8
	PackedSub []byte
	Path      chunk.Proof
}

// Header is a block header.
type Header struct {
	Version   uint32
	Height    uint64
	Prev      Hash
	Time      int64
	Target    Hash // big-endian
	TxRoot    Hash
	BoardRoot Hash
	StateRoot Hash // the state after this block (see state.Root)
	// TextBundles places the texts of the closing seal window into the
	// weave, one bundle per retention bucket. Empty except in seal blocks.
	TextBundles []TextBundle
	VDFStep     uint64 // global index of the last VDF output
	VDFOutputs  []Hash // outputs since the parent; the last one seeds the challenge
	Sol         Solution
	Miner       []byte // Ed25519 public key
	Sig         []byte
}

const maxVDFOutputs = 100_000

// TextBundle is the data root and size of the bodies a seal block places in
// one bucket: the bodies of that bucket's queued texts, concatenated.
type TextBundle struct {
	Bucket uint32
	Root   Hash
	Size   uint64
}

func (h *Header) encodeBody(w *codec.Writer) {
	w.U32(h.Version)
	w.Var(h.Height)
	w.Fixed(h.Prev[:])
	w.U64(uint64(h.Time))
	w.Fixed(h.Target[:])
	w.Fixed(h.TxRoot[:])
	w.Fixed(h.BoardRoot[:])
	w.Fixed(h.StateRoot[:])
	w.Var(uint64(len(h.TextBundles)))
	for _, tb := range h.TextBundles {
		w.U32(tb.Bucket)
		w.Fixed(tb.Root[:])
		w.Var(tb.Size)
	}
	w.Var(h.VDFStep)
	w.Var(uint64(len(h.VDFOutputs)))
	for _, o := range h.VDFOutputs {
		w.Fixed(o[:])
	}
	w.U64(h.Sol.Partition)
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
	r.Fixed(h.StateRoot[:])
	nb := r.Count(params.MaxTextBundles)
	for i := 0; i < nb && r.Err == nil; i++ {
		var tb TextBundle
		tb.Bucket = r.U32()
		r.Fixed(tb.Root[:])
		tb.Size = r.Var()
		h.TextBundles = append(h.TextBundles, tb)
	}
	h.VDFStep = r.Var()
	n := r.Count(maxVDFOutputs)
	for i := 0; i < n; i++ {
		var o Hash
		r.Fixed(o[:])
		h.VDFOutputs = append(h.VDFOutputs, o)
	}
	h.Sol.Partition = r.U64()
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
		tb := r.Bytes(maxTxBytes)
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
		if t.IsPost() {
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
