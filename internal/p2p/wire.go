package p2p

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"time"

	"github.com/namsaeg/chainboard/internal/codec"
	"github.com/namsaeg/chainboard/internal/cryptox"
)

// Message types.
const (
	msgHello  uint8 = iota + 1
	msgTx           // fluff-phase transaction (gossip to everyone)
	msgStemTx       // Dandelion++ stem-phase transaction (one hop, to one peer)
	msgBlock
	msgGetBlocks
	msgBlocks
	msgGetChunk
	msgChunk
	msgGetPeers
	msgPeers
	msgGetTexts // post bodies by hash
	msgTexts
	msgItemInv  // ids of gossiped items (funding requests, bounty offers) a node holds
	msgGetItems // gossiped items by id
	msgItems    // gossiped items (each with its proof of work), fluff phase
	msgStemItem // a gossiped item in its stem phase (one hop, to one peer)
	msgPad      // cover traffic: a fixed-size frame carrying nothing
	msgPadded   // cover traffic: a fixed-size frame carrying part of a stem transaction
)

const maxMessage = 32 << 20

func writeMsg(w io.Writer, typ uint8, payload []byte) error {
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(payload)+1))
	hdr[4] = typ
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readMsg(r io.Reader) (uint8, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:4])
	if n == 0 || n > maxMessage {
		return 0, nil, errors.New("p2p: bad message length")
	}
	buf := make([]byte, n-1)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return hdr[4], buf, nil
}

type hello struct {
	Magic   uint32
	Version string
	Genesis cryptox.Hash
	Height  uint64
	Best    cryptox.Hash
	Listen  string
	Nonce   uint64
}

func (h *hello) encode() []byte {
	var w codec.Writer
	w.U32(h.Magic)
	w.Bytes([]byte(h.Version))
	w.Fixed(h.Genesis[:])
	w.Var(h.Height)
	w.Fixed(h.Best[:])
	w.Bytes([]byte(h.Listen))
	w.U64(h.Nonce)
	return w.B
}

func decodeHello(b []byte) (*hello, error) {
	r := codec.NewReader(b)
	h := &hello{}
	h.Magic = r.U32()
	h.Version = string(r.Bytes(64))
	r.Fixed(h.Genesis[:])
	h.Height = r.Var()
	r.Fixed(h.Best[:])
	h.Listen = string(r.Bytes(256))
	h.Nonce = r.U64()
	return h, r.Done()
}

// tlsConfig builds a mutual-TLS 1.3 config from the node key. Peers are
// authenticated by their self-signed Ed25519 certificates; the node key is not
// the wallet key, so a network identity reveals no account. The ALPN list is
// the one browsers send, so a connection looks like ordinary HTTPS.
func tlsConfig(key ed25519.PrivateKey) (*tls.Config, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	verify := func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) != 1 {
			return errors.New("p2p: expected one certificate")
		}
		c, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return err
		}
		if _, ok := c.PublicKey.(ed25519.PublicKey); !ok {
			return errors.New("p2p: peer key is not Ed25519")
		}
		// Self-signed: check the certificate's own signature with its own key.
		return c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature)
	}
	return &tls.Config{
		Certificates:          []tls.Certificate{cert},
		MinVersion:            tls.VersionTLS13,
		ClientAuth:            tls.RequireAnyClientCert,
		InsecureSkipVerify:    true, // identity is checked in VerifyPeerCertificate
		VerifyPeerCertificate: verify,
		NextProtos:            []string{"h2", "http/1.1"},
	}, nil
}

func peerKey(cs tls.ConnectionState) []byte {
	if len(cs.PeerCertificates) == 0 {
		return nil
	}
	if k, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey); ok {
		return k
	}
	return nil
}
