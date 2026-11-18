package miner

import (
	"github.com/namsaeg/chainboard/internal/chain"
	"github.com/namsaeg/chainboard/internal/mempool"
	"github.com/namsaeg/chainboard/internal/params"
)

func chainOpen(p *params.Params, dir string) (*chain.Chain, error) { return chain.Open(p, dir) }
func newPool(c *chain.Chain) *mempool.Pool                         { return mempool.New(c.Tip(), nil) }
