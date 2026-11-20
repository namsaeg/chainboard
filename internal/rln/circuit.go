package rln

import (
	"math/big"

	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/std/hash/mimc"
	"github.com/consensys/gnark/std/math/cmp"
)

// Depth of the membership tree: 2^20 ≈ 1M members.
const Depth = 20

// LimitBits bounds limits, caps and message ids to 16 bits.
const LimitBits = 16

// Circuit proves RLN membership and a well-formed share.
type Circuit struct {
	Root      frontend.Variable `gnark:",public"`
	Epoch     frontend.Variable `gnark:",public"`
	Scope     frontend.Variable `gnark:",public"`
	Cap       frontend.Variable `gnark:",public"`
	X         frontend.Variable `gnark:",public"`
	Y         frontend.Variable `gnark:",public"`
	Nullifier frontend.Variable `gnark:",public"`
	Tag       frontend.Variable `gnark:",public"`

	Secret frontend.Variable
	Limit  frontend.Variable
	MsgID  frontend.Variable
	Path   [Depth]frontend.Variable
	Bits   [Depth]frontend.Variable
}

func hash(api frontend.API, xs ...frontend.Variable) frontend.Variable {
	h, _ := mimc.NewMiMC(api)
	h.Write(xs...)
	return h.Sum()
}

// Define implements frontend.Circuit.
func (c *Circuit) Define(api frontend.API) error {
	idc := hash(api, tagIdentity, c.Secret)
	node := hash(api, tagLeaf, idc, c.Limit)
	for i := 0; i < Depth; i++ {
		api.AssertIsBoolean(c.Bits[i])
		l := api.Select(c.Bits[i], c.Path[i], node)
		r := api.Select(c.Bits[i], node, c.Path[i])
		node = hash(api, l, r)
	}
	api.AssertIsEqual(node, c.Root)

	// 0 <= msgID < limit, msgID < cap, all within 16 bits.
	api.ToBinary(c.MsgID, LimitBits)
	api.ToBinary(c.Limit, LimitBits)
	api.ToBinary(c.Cap, LimitBits)
	bc := cmp.NewBoundedComparator(api, big.NewInt(1<<LimitBits), false)
	bc.AssertIsLess(c.MsgID, c.Limit)
	bc.AssertIsLess(c.MsgID, c.Cap)

	key := hash(api, tagEpoch, c.Secret, c.Epoch, c.Scope)
	a1 := hash(api, tagShare, key, c.MsgID)
	api.AssertIsEqual(c.Y, api.Add(key, api.Mul(a1, c.X)))
	api.AssertIsEqual(c.Nullifier, hash(api, tagNullifier, a1))
	api.AssertIsEqual(c.Tag, api.Add(idc, hash(api, tagMask, a1)))
	return nil
}
