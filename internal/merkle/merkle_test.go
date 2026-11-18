package merkle

import (
	"fmt"
	"testing"
)

func TestProveVerifyAllShapes(t *testing.T) {
	for n := 1; n <= 17; n++ {
		leaves := make([]Hash, n)
		for i := range leaves {
			leaves[i] = Leaf([]byte(fmt.Sprint(i)))
		}
		root := Root(leaves)
		for i := 0; i < n; i++ {
			p, err := Prove(leaves, i)
			if err != nil {
				t.Fatal(err)
			}
			if !Verify(root, leaves[i], i, n, p) {
				t.Fatalf("n=%d i=%d verify failed", n, i)
			}
			if n > 1 && Verify(root, leaves[(i+1)%n], i, n, p) {
				t.Fatalf("n=%d i=%d wrong leaf accepted", n, i)
			}
		}
	}
}
