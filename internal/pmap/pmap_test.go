package pmap

import (
	"encoding/binary"
	"math/rand"
	"testing"
)

// check compares a map version against the plain map it should equal.
func check(t *testing.T, m Map[uint64, int], want map[uint64]int) {
	t.Helper()
	if m.Len() != len(want) {
		t.Fatalf("len %d, want %d", m.Len(), len(want))
	}
	for k, v := range want {
		if got, ok := m.Get(k); !ok || got != v {
			t.Fatalf("key %d: %d %v, want %d", k, got, ok, v)
		}
	}
	seen := 0
	m.Range(func(k uint64, v int) bool {
		if want[k] != v {
			t.Fatalf("range gave %d=%d", k, v)
		}
		seen++
		return true
	})
	if seen != len(want) {
		t.Fatalf("range saw %d of %d", seen, len(want))
	}
}

func testRandom(t *testing.T, hash func(uint64) uint64, keys uint64) {
	r := rand.New(rand.NewSource(1))
	m := New[uint64, int](hash)
	want := map[uint64]int{}
	type snap struct {
		m    Map[uint64, int]
		want map[uint64]int
	}
	var snaps []snap
	for step := 0; step < 20000; step++ {
		k := r.Uint64() % keys
		if r.Intn(3) == 0 {
			m.Delete(k)
			delete(want, k)
		} else {
			m.Set(k, step)
			want[k] = step
		}
		if _, ok := m.Get(keys + 1); ok {
			t.Fatal("absent key found")
		}
		if step%2000 == 0 {
			cp := map[uint64]int{}
			for k, v := range want {
				cp[k] = v
			}
			snaps = append(snaps, snap{m, cp}) // O(1) copy
		}
	}
	check(t, m, want)
	for _, s := range snaps {
		check(t, s.m, s.want) // later writes did not touch earlier versions
	}
}

func TestMapRandom(t *testing.T) { testRandom(t, Uint64, 3000) }

// TestMapCollisions forces every key into the collision lists.
func TestMapCollisions(t *testing.T) { testRandom(t, func(uint64) uint64 { return 42 }, 60) }

// TestMapPartialCollisions shares long hash prefixes.
func TestMapPartialCollisions(t *testing.T) {
	testRandom(t, func(k uint64) uint64 { return k & 7 << 59 }, 200)
}

func TestVec(t *testing.T) {
	var v Vec[int]
	var want []int
	var snaps []Vec[int]
	var lens []int
	for i := 0; i < 2000; i++ {
		v.Append(i)
		want = append(want, i)
		if i%300 == 0 {
			snaps, lens = append(snaps, v), append(lens, v.Len())
		}
	}
	v.Set(5, -5)
	v.Set(1999, -1999)
	want[5], want[1999] = -5, -1999
	for i, w := range want {
		if v.At(i) != w {
			t.Fatalf("at %d: %d", i, v.At(i))
		}
	}
	for j, s := range snaps {
		if s.Len() != lens[j] {
			t.Fatal("snapshot length changed")
		}
		for i := 0; i < s.Len(); i++ {
			if s.At(i) != i {
				t.Fatalf("snapshot %d changed at %d", j, i)
			}
		}
	}
	// Two versions appending to the same shared tail stay apart.
	a, b := snaps[1], snaps[1]
	a.Append(-1)
	b.Append(-2)
	if a.At(a.Len()-1) != -1 || b.At(b.Len()-1) != -2 {
		t.Fatal("appends to shared versions interfered")
	}
}

func BenchmarkMapCopyAndSet(b *testing.B) {
	m := New[uint64, int](Uint64)
	for i := 0; i < 1_000_000; i++ {
		m.Set(uint64(i), i)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c := m // a new version
		c.Set(uint64(i), -i)
	}
}

func BenchmarkGoMapCopyAndSet(b *testing.B) {
	m := map[uint64]int{}
	for i := 0; i < 1_000_000; i++ {
		m[uint64(i)] = i
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c := make(map[uint64]int, len(m))
		for k, v := range m {
			c[k] = v
		}
		c[uint64(i)] = -i
	}
}

func sumOf(k uint64, v int) Sum {
	var s Sum
	binary.BigEndian.PutUint64(s[:], k)
	binary.BigEndian.PutUint64(s[8:], uint64(v))
	return s
}

// Equal contents give equal roots, whatever the order of writes and
// deletes, also through hash collisions; any change moves the root.
func TestRootCanonical(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	keys := make([]uint64, 3000)
	for i := range keys {
		keys[i] = rng.Uint64()
	}
	weak := func(k uint64) uint64 { return k % 50 } // forces collisions and deep paths
	for _, hash := range []func(uint64) uint64{Uint64, weak} {
		a := Committed[uint64, int](hash, sumOf)
		for i, k := range keys {
			a.Set(k, i)
		}
		b := Committed[uint64, int](hash, sumOf)
		for _, i := range rng.Perm(len(keys)) {
			b.Set(keys[i], -1) // overwritten below
			b.Set(rng.Uint64(), 0)
		}
		b = Committed[uint64, int](hash, sumOf)
		extra := rng.Uint64()
		b.Set(extra, 7)
		for _, i := range rng.Perm(len(keys)) {
			b.Set(keys[i], i)
		}
		b.Delete(extra)
		if a.Root() != b.Root() {
			t.Fatal("same contents, different roots")
		}
		before := a.Root()
		c := a
		c.Set(keys[5], 99)
		if c.Root() == before || a.Root() != before {
			t.Fatal("a write did not move only its own version's root")
		}
	}
}

func TestVecRoot(t *testing.T) {
	item := func(x int) Sum { return sumOf(uint64(x), 0) }
	var a, b Vec[int]
	for i := 0; i < 700; i++ {
		a.Append(i)
		b.Append(i)
	}
	r := a.Root(item)
	b.Set(300, -1)
	if b.Root(item) == r || a.Root(item) != r {
		t.Fatal("vector roots")
	}
	b.Set(300, 300)
	if b.Root(item) != r {
		t.Fatal("restored vector has another root")
	}
}
