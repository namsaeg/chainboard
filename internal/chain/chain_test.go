package chain

import (
	"os"
	"testing"

	"github.com/namsaeg/chainboard/internal/params"
)

// TestSetAsideOtherNetwork: a database written for another genesis is moved
// away instead of being replayed against the wrong chain.
func TestSetAsideOtherNetwork(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(params.Devnet(), dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	c, err = Open(params.Devnet(), dir)
	if err != nil || c.SetAside != "" {
		t.Fatalf("same network set aside: %q %v", c.SetAside, err)
	}
	c.Close()
	other := params.Devnet()
	other.Magic++ // a different network: a different genesis
	c, err = Open(other, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.SetAside == "" {
		t.Fatal("database of another genesis was reused")
	}
	if _, err := os.Stat(c.SetAside); err != nil {
		t.Fatal(err)
	}
}
