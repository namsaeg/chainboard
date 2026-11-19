package rln

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"math/big"
	"sync"

	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	groth16 "github.com/consensys/gnark/backend/groth16/bn254"
	cs "github.com/consensys/gnark/constraint/bn254"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/frontend/cs/r1cs"
	"github.com/consensys/gnark/std/math/cmp"
)

// Anonymous likes. A batch of up to LikeBatch likes takes one message of the
// member's epoch (the same share and nullifier as a post, so going over the
// limit is slashable the same way) and, for each liked post, a tag
//
//	tag = H(5, secret, scope, post)
//
// which is the same whenever the same member likes the same post, and says
// nothing else: nodes count one like per (post, tag), so a member cannot
// like a post twice, yet two of its likes cannot be linked. Posts are named
// by their like reference (block height << 32 | first four id bytes); an
// empty slot has post 0 and tag 0.

// LikeBatch is the most likes one anonymous batch carries.
const LikeBatch = 16

const tagLike = 5

// LikeCircuit proves membership, a well-formed share, and the likes' tags.
type LikeCircuit struct {
	Root      frontend.Variable            `gnark:",public"`
	Epoch     frontend.Variable            `gnark:",public"`
	Scope     frontend.Variable            `gnark:",public"`
	Cap       frontend.Variable            `gnark:",public"`
	X         frontend.Variable            `gnark:",public"`
	Y         frontend.Variable            `gnark:",public"`
	Nullifier frontend.Variable            `gnark:",public"`
	Posts     [LikeBatch]frontend.Variable `gnark:",public"`
	Tags      [LikeBatch]frontend.Variable `gnark:",public"`

	Secret frontend.Variable
	Limit  frontend.Variable
	MsgID  frontend.Variable
	Path   [Depth]frontend.Variable
	Bits   [Depth]frontend.Variable
}

// Define implements frontend.Circuit.
func (c *LikeCircuit) Define(api frontend.API) error {
	idc := hash(api, tagIdentity, c.Secret)
	node := hash(api, tagLeaf, idc, c.Limit)
	for i := 0; i < Depth; i++ {
		api.AssertIsBoolean(c.Bits[i])
		l := api.Select(c.Bits[i], c.Path[i], node)
		r := api.Select(c.Bits[i], node, c.Path[i])
		node = hash(api, l, r)
	}
	api.AssertIsEqual(node, c.Root)
	api.ToBinary(c.MsgID, LimitBits)
	api.ToBinary(c.Limit, LimitBits)
	api.ToBinary(c.Cap, LimitBits)
	bc := cmp.NewBoundedComparator(api, big.NewInt(1<<LimitBits), false)
	bc.AssertIsLess(c.MsgID, c.Limit)
	bc.AssertIsLess(c.MsgID, c.Cap)
	a1 := hash(api, tagShare, c.Secret, c.Epoch, c.Scope, c.MsgID)
	api.AssertIsEqual(c.Y, api.Add(c.Secret, api.Mul(a1, c.X)))
	api.AssertIsEqual(c.Nullifier, hash(api, tagNullifier, a1))
	for i := 0; i < LikeBatch; i++ {
		tag := hash(api, tagLike, c.Secret, c.Scope, c.Posts[i])
		api.AssertIsEqual(c.Tags[i], api.Select(api.IsZero(c.Posts[i]), 0, tag))
	}
	return nil
}

// LikeTag is a member's tag for a post (0 for an empty slot).
func LikeTag(secret fr.Element, scope uint64, post uint64) fr.Element {
	if post == 0 {
		return fr.Element{}
	}
	return H(u64(tagLike), secret, u64(scope), u64(post))
}

//go:embed keys/like.vk
var likeVKBytes []byte

//go:embed keys/like.pk
var likePKBytes []byte

var (
	likeVKOnce sync.Once
	likeVK     *groth16.VerifyingKey
	likeVKErr  error

	likePKOnce sync.Once
	likePK     *groth16.ProvingKey
	likeCCS    *cs.R1CS
	likePKErr  error
)

// CompileLikes builds the like circuit's constraint system.
func CompileLikes() (*cs.R1CS, error) {
	c, err := frontend.Compile(ecc.BN254.ScalarField(), r1cs.NewBuilder, &LikeCircuit{})
	if err != nil {
		return nil, err
	}
	return c.(*cs.R1CS), nil
}

// SetupLikes is Setup for the like circuit (single-party, testnets only).
func SetupLikes(pkOut, vkOut io.Writer) error {
	c, err := CompileLikes()
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

func loadLikeVK() (*groth16.VerifyingKey, error) {
	likeVKOnce.Do(func() {
		var v groth16.VerifyingKey
		if _, err := v.ReadFrom(bytes.NewReader(likeVKBytes)); err != nil {
			likeVKErr = fmt.Errorf("rln: like verifying key: %w", err)
			return
		}
		if err := v.Precompute(); err != nil {
			likeVKErr = err
			return
		}
		likeVK = &v
	})
	return likeVK, likeVKErr
}

func loadLikePK() (*groth16.ProvingKey, *cs.R1CS, error) {
	likePKOnce.Do(func() {
		var p groth16.ProvingKey
		if _, err := p.ReadFrom(bytes.NewReader(likePKBytes)); err != nil {
			likePKErr = fmt.Errorf("rln: like proving key: %w", err)
			return
		}
		c, err := CompileLikes()
		if err != nil {
			likePKErr = err
			return
		}
		likePK, likeCCS = &p, c
	})
	return likePK, likeCCS, likePKErr
}

// LikePublic are the public inputs of a like batch's proof.
type LikePublic struct {
	Public
	Posts [LikeBatch]uint64
	Tags  [LikeBatch]fr.Element
}

func (p *LikePublic) vector() fr.Vector {
	v := p.Public.vector()
	for _, x := range p.Posts {
		v = append(v, u64(x))
	}
	return append(v, p.Tags[:]...)
}

// ProveLikes creates a like batch's proof.
func ProveLikes(pub LikePublic, w Witness) ([]byte, error) {
	p, c, err := loadLikePK()
	if err != nil {
		return nil, err
	}
	a := LikeCircuit{
		Root: pub.Root, Epoch: pub.Epoch, Scope: pub.Scope, Cap: pub.Cap,
		X: pub.X, Y: pub.Y, Nullifier: pub.Nullifier,
		Secret: w.Secret, Limit: w.Limit, MsgID: w.MsgID,
	}
	for i := 0; i < LikeBatch; i++ {
		a.Posts[i] = pub.Posts[i]
		a.Tags[i] = pub.Tags[i]
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

// VerifyLikes checks a like batch's proof.
func VerifyLikes(proof []byte, pub LikePublic) error {
	v, err := loadLikeVK()
	if err != nil {
		return err
	}
	return verifyWith(v, proof, pub.vector())
}

// VerifyLikesCached is VerifyLikes with the success cache (see VerifyCached).
func VerifyLikesCached(key [32]byte, proof []byte, pub LikePublic) error {
	return cached(key, func() error { return VerifyLikes(proof, pub) })
}
