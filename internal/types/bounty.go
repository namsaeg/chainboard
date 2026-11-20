package types

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/cryptox"
	"github.com/namsaeg/chainboard/internal/params"
)

// Bounties. A requester locks coins for data it wants (described in a
// note, with optional reference attachments); likes raise the pot. Hunters
// answer with signed offers that travel off chain: the data's root and
// size, and a preview the requester judges by. The full data stays with
// the hunter. Accepting an offer (only the requester, before the deadline)
// carries it on chain and locks the pot to it; the hunter then reveals the
// data and anyone proves sampled sub-chunks within a window, after which
// the pot (less the result's storage) goes to the hunter unless the
// requester disputes within a further window, which burns it instead. The
// requester never sees the full data before the pot is locked, and the
// lock releases without the requester's help.

const (
	MaxBountyNote = 1000 // bytes, in the bounty transaction
	MaxOfferNote  = 300
	MaxBountyRefs = 4 // reference attachments
)

// Offer is a hunter's answer to a bounty.
type Offer struct {
	Bounty      Hash
	Hunter      []byte // Ed25519 public key; the pot is paid to its address
	DataRoot    Hash
	Size        uint64
	Media       uint8
	PreviewRoot Hash // optional free-form preview (thumbnail, excerpt), not bound to the data
	PreviewSize uint64
	Note        string
	Sig         []byte
}

func (o *Offer) encodeBody(w *codec.Writer) {
	w.Fixed(o.Bounty[:])
	w.Fixed(o.Hunter)
	w.Fixed(o.DataRoot[:])
	w.Var(o.Size)
	w.U8(o.Media)
	w.Fixed(o.PreviewRoot[:])
	w.Var(o.PreviewSize)
	w.Bytes([]byte(o.Note))
}

// ID names the offer.
func (o *Offer) ID() Hash {
	var w codec.Writer
	o.encodeBody(&w)
	return cryptox.Tagged("cb/offer", w.B)
}

// HunterAddr is where the pot goes.
func (o *Offer) HunterAddr() Address { return cryptox.AddressOf(o.Hunter) }

// Sign fills in Hunter and Sig.
func (o *Offer) Sign(k *cryptox.Key) {
	o.Hunter = append([]byte(nil), k.Pub...)
	id := o.ID()
	o.Sig = k.Sign(id[:])
}

// Check validates an offer on its own.
func (o *Offer) Check() error {
	if len(o.Hunter) != ed25519.PublicKeySize || len(o.Sig) != ed25519.SignatureSize {
		return errors.New("offer: bad key or signature length")
	}
	if o.Bounty.IsZero() || o.DataRoot.IsZero() || o.Size == 0 {
		return errors.New("offer names no bounty or data")
	}
	if o.PreviewRoot.IsZero() != (o.PreviewSize == 0) {
		return errors.New("offer: preview root and size go together")
	}
	if len(o.Note) > MaxOfferNote || !utf8.ValidString(o.Note) {
		return fmt.Errorf("offer: note must be UTF-8 of at most %d bytes", MaxOfferNote)
	}
	id := o.ID()
	if !cryptox.Verify(o.Hunter, id[:], o.Sig) {
		return errors.New("offer: bad signature")
	}
	return nil
}

// Encode is the offer with its signature.
func (o *Offer) Encode() []byte {
	var w codec.Writer
	o.encodeBody(&w)
	w.Fixed(o.Sig)
	return w.B
}

func decodeOffer(rd *codec.Reader) *Offer {
	o := &Offer{Hunter: make([]byte, ed25519.PublicKeySize), Sig: make([]byte, ed25519.SignatureSize)}
	rd.Fixed(o.Bounty[:])
	rd.Fixed(o.Hunter)
	rd.Fixed(o.DataRoot[:])
	o.Size = rd.Var()
	o.Media = rd.U8()
	rd.Fixed(o.PreviewRoot[:])
	o.PreviewSize = rd.Var()
	o.Note = string(rd.Bytes(MaxOfferNote))
	rd.Fixed(o.Sig)
	return o
}

// DecodeOffer parses an encoded offer.
func DecodeOffer(b []byte) (*Offer, error) {
	rd := codec.NewReader(b)
	o := decodeOffer(rd)
	if err := rd.Done(); err != nil {
		return nil, err
	}
	return o, nil
}

func checkBounty(t *Tx) error {
	if t.Amount == 0 {
		return errors.New("a bounty needs coins")
	}
	if len(t.Board) == 0 || len(t.Board) > params.MaxBoardBytes || !utf8.ValidString(t.Board) {
		return errors.New("bounty: bad board")
	}
	if len(t.Note) == 0 || len(t.Note) > MaxBountyNote || !utf8.ValidString(t.Note) {
		return fmt.Errorf("bounty: describe what you want in 1 to %d bytes of UTF-8", MaxBountyNote)
	}
	if len(t.Attach) > MaxBountyRefs {
		return fmt.Errorf("bounty: at most %d reference attachments", MaxBountyRefs)
	}
	if _, _, ok := params.TierMonths(t.Tier); !ok {
		return errors.New("bounty: bad retention tier for the result")
	}
	return nil
}
