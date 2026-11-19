package miner

import (
	"github.com/namsaeg/chainboard/internal/chain"
	"github.com/namsaeg/chainboard/internal/mempool"
	"github.com/namsaeg/chainboard/internal/params"
	"github.com/namsaeg/chainboard/internal/state"
	"github.com/namsaeg/chainboard/internal/types"
)

func chainOpen(p *params.Params, dir string) (*chain.Chain, error) { return chain.Open(p, dir) }
func newPool(c *chain.Chain) *mempool.Pool                         { return mempool.New(c.Tip(), nil) }

func postOf(st *state.State, id types.Hash) *state.PostInfo {
	p, _ := st.Posts.Get(id)
	return p
}

func memberOf(st *state.State, idc types.Hash) *state.Member {
	m, _ := st.Members.Get(idc)
	return m
}

func dataOf(st *state.State, root types.Hash) *state.DataInfo {
	d, _ := st.Data.Get(root)
	return d
}
