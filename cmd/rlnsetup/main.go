// Command rlnsetup regenerates the RLN Groth16 keys embedded in the binary.
// It is a single-party setup: suitable for testnets only.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/namsaeg/chainboard/internal/rln"
)

func main() {
	dir := "internal/rln/keys"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	pkf, err := os.Create(filepath.Join(dir, "rln.pk"))
	check(err)
	vkf, err := os.Create(filepath.Join(dir, "rln.vk"))
	check(err)
	check(rln.Setup(pkf, vkf))
	check(pkf.Close())
	check(vkf.Close())

	fmt.Println("wrote", dir)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
