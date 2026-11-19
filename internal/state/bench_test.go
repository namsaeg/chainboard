package state

import (
	"encoding/binary"
	"testing"

	"github.com/namsaeg/chainboard/internal/params"
)

// bigState fills a state with posts and accounts, as a busy network would
// have after a while.
func bigState(posts, accounts int) *State {
	s, _ := Genesis(params.Testnet())
	var k Hash
	for i := 0; i < posts; i++ {
		binary.LittleEndian.PutUint64(k[:], uint64(i)*0x9e3779b97f4a7c15)
		s.Posts.Set(k, &PostInfo{TextLen: uint16(i)})
	}
	var a Address
	for i := 0; i < accounts; i++ {
		binary.LittleEndian.PutUint64(a[:], uint64(i)*0x9e3779b97f4a7c15)
		s.Accounts.Set(a, &Account{Balance: uint64(i)})
	}
	return s
}

// BenchmarkCloneAndWrite is what a block, a mempool view or an API call
// pays to get its own state version and change one post.
func BenchmarkCloneAndWrite(b *testing.B) {
	s := bigState(1_000_000, 100_000)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c := s.Clone()
		var k Hash
		k[0] = byte(i)
		c.Posts.Set(k, &PostInfo{})
		c.Account(Address{byte(i)}).Balance++
	}
}
