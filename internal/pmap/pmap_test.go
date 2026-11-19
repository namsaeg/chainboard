package pmap

import (
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
