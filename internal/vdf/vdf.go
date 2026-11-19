// Package vdf is the sequential hash chain that paces mining: each step is a
// fixed number of SHA-256 iterations, so challenges are unlocked at a rate set
// by time, not by how much hardware a miner has. Blocks carry every step
// output since their parent, so verification can run steps in parallel.
package vdf

import (
	"crypto/sha256"
	"runtime"
	"sync"

	"github.com/namsaeg/chainboard/internal/cryptox"
)

type Hash = cryptox.Hash

// Step runs one VDF step from in.
func Step(in Hash, iterations uint64) Hash {
	cur := in
	for i := uint64(0); i < iterations; i++ {
		cur = sha256.Sum256(cur[:])
	}
	return cur
}

// Reseed mixes a new block hash into the chain so steps cannot be computed
// far ahead of the chain tip.
func Reseed(last Hash, blockHash Hash) Hash {
	return cryptox.Tagged("cb/vdf/reseed", last[:], blockHash[:])
}

// VerifyChain checks that outputs[0] = Step(start) and each next output is the
// step of the previous one. Steps are checked in parallel on every core.
func VerifyChain(start Hash, outputs []Hash, iterations uint64) bool {
	idx := make([]int, len(outputs))
	for i := range idx {
		idx[i] = i
	}
	return VerifySteps(start, outputs, iterations, idx, runtime.NumCPU())
}

// VerifySteps checks only the steps at idx (each from the output before it),
// on the given number of workers. Checking a random sample catches a forged
// step with the sampled share's probability; the rest can be checked later.
func VerifySteps(start Hash, outputs []Hash, iterations uint64, idx []int, workers int) bool {
	if len(outputs) == 0 {
		return false
	}
	workers = max(1, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := true
	jobs := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				in := start
				if i > 0 {
					in = outputs[i-1]
				}
				if Step(in, iterations) != outputs[i] {
					mu.Lock()
					ok = false
					mu.Unlock()
				}
			}
		}()
	}
	for _, i := range idx {
		if i < 0 || i >= len(outputs) {
			continue
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return ok
}
