package vdf

import "testing"

func TestChain(t *testing.T) {
	var start Hash
	outs := []Hash{}
	cur := start
	for i := 0; i < 5; i++ {
		cur = Step(cur, 100)
		outs = append(outs, cur)
	}
	if !VerifyChain(start, outs, 100) {
		t.Fatal("valid chain rejected")
	}
	outs[3][0] ^= 1
	if VerifyChain(start, outs, 100) {
		t.Fatal("bad chain accepted")
	}
}
