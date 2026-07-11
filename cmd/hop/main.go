package main

import (
	"os"

	"githop.xyz/hop/hop/internal/hop"
)

func main() {
	os.Exit(hop.RunCLI(os.Args[1:], os.Stdout, os.Stderr))
}
