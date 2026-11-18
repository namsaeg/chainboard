package rln

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	groth16 "github.com/consensys/gnark/backend/groth16/bn254"
	cs "github.com/consensys/gnark/constraint/bn254"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/cs/r1cs"
	"github.com/consensys/gnark/logger"
)

// MaxProofSize bounds the encoded proof (Groth16 A, B, C plus the
// commitment and its proof of knowledge used by gnark's range checks).
const MaxProofSize = 256

//go:embed keys/rln.vk
var vkBytes []byte

//go:embed keys/rln.pk
var pkBytes []byte

var (
	vkOnce sync.Once
	vk     *groth16.VerifyingKey
	vkErr  error

	pkOnce sync.Once
	pk     *groth16.ProvingKey
	ccs    *cs.R1CS
	pkErr  error
)

// Compile builds the constraint system.
func Compile() (*cs.R1CS, error) {
	c, err := frontend.Compile(ecc.BN254.ScalarField(), r1cs.NewBuilder, &Circuit{})
	if err != nil {
		return nil, err
	}
	return c.(*cs.R1CS), nil
}

// Setup runs a fresh single-party Groth16 setup and writes the keys. The
// toxic waste lives only inside this call; the operator must trust it was
// discarded. Mainnet requires a multi-party ceremony instead.
func Setup(pkOut, vkOut io.Writer) error {
	c, err := Compile()
	if err != nil {
		return err
	}
	var p groth16.ProvingKey
	var v groth16.VerifyingKey
	if err := groth16.Setup(c, &p, &v); err != nil {
		return err
	}
	if _, err := p.WriteTo(pkOut); err != nil {
		return err
	}
	_, err = v.WriteTo(vkOut)
	return err
}

func loadVK() (*groth16.VerifyingKey, error) {
	vkOnce.Do(func() {
		var v groth16.VerifyingKey
		if _, err := v.ReadFrom(bytes.NewReader(vkBytes)); err != nil {
			vkErr = fmt.Errorf("rln: verifying key: %w", err)
			return
		}
		if err := v.Precompute(); err != nil {
			vkErr = err
			return
		}
		vk = &v
	})
	return vk, vkErr
}

func loadPK() (*groth16.ProvingKey, *cs.R1CS, error) {
	pkOnce.Do(func() {
		var p groth16.ProvingKey
		if _, err := p.ReadFrom(bytes.NewReader(pkBytes)); err != nil {
			pkErr = fmt.Errorf("rln: proving key: %w", err)
			return
		}
		c, err := Compile()
		if err != nil {
			pkErr = err
			return
		}
		pk, ccs = &p, c
	})
	return pk, ccs, pkErr
}

// Public are the public inputs of one RLN proof.
type Public struct {
	Root, X, Y, Nullifier fr.Element
	Epoch, Scope, Cap     uint64
}

func (p *Public) vector() fr.Vector {
	return fr.Vector{p.Root, u64(p.Epoch), u64(p.Scope), u64(p.Cap), p.X, p.Y, p.Nullifier}
}

// Witness is the private side of a proof.
type Witness struct {
	Secret fr.Element
	Limit  uint64
	MsgID  uint64
	Path   [Depth]fr.Element
	Bits   [Depth]uint8
}

// Prove creates a compressed proof.
func Prove(pub Public, w Witness) ([]byte, error) {
	p, c, err := loadPK()
	if err != nil {
		return nil, err
	}
	a := Circuit{
		Root: pub.Root, Epoch: pub.Epoch, Scope: pub.Scope, Cap: pub.Cap,
		X: pub.X, Y: pub.Y, Nullifier: pub.Nullifier,
		Secret: w.Secret, Limit: w.Limit, MsgID: w.MsgID,
	}
	for i := 0; i < Depth; i++ {
		a.Path[i] = w.Path[i]
		a.Bits[i] = w.Bits[i]
	}
	full, err := frontend.NewWitness(&a, ecc.BN254.ScalarField())
	if err != nil {
		return nil, err
	}
	proof, err := groth16.Prove(c, p, full)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if _, err := proof.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ErrProof is returned for any malformed or invalid proof.
var ErrProof = errors.New("rln: invalid proof")

// Verify checks a compressed proof against public inputs.
func Verify(proof []byte, pub Public) error {
	v, err := loadVK()
	if err != nil {
		return err
	}
	if len(proof) == 0 || len(proof) > MaxProofSize {
		return ErrProof
	}
	var pr groth16.Proof
	n, err := pr.ReadFrom(bytes.NewReader(proof))
	if err != nil || n != int64(len(proof)) {
		return ErrProof
	}
	var re bytes.Buffer
	if _, err := pr.WriteTo(&re); err != nil || !bytes.Equal(re.Bytes(), proof) {
		return ErrProof // non-canonical encoding
	}
	if err := groth16.Verify(&pr, v, pub.vector()); err != nil {
		return ErrProof
	}
	return nil
}

func init() { logger.Disable() }
